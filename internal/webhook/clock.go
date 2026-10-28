package webhook

import (
	"container/heap"
	"context"
	"errors"
	"time"
)

// Session virtual clocks.
//
// Every session runs on its own clock: wall-clock time plus an offset that
// starts at zero, so a session nobody advanced behaves exactly as before.
// Advance moves one session's clock forward and replays, in order and
// synchronously, everything that becomes due inside the window: deliveries,
// the transitions their first attempts trigger, the deliveries those
// transitions schedule, and retries. Other sessions keep running on wall
// time.
//
// Invariant: for every pending delivery of a session, fireAt + offset is the
// session time it is due. Moving the clock by s therefore shifts the offset
// by +s and every pending fireAt by -s.

// ErrSessionBusy is returned when a session is already being driven by
// another Advance call.
var ErrSessionBusy = errors.New("session is busy with another time advance")

// maxFiredPerAdvance stops a scenario that keeps scheduling zero-delay
// webhooks in a cycle from spinning one call forever.
const maxFiredPerAdvance = 1000

// FiredDelivery is one attempt made while advancing a session's clock.
type FiredDelivery struct {
	DeliveryID string    `json:"delivery_id"`
	Event      string    `json:"event,omitempty"`
	Target     string    `json:"target"`
	VirtualAt  time.Time `json:"virtual_at"`
	Attempt    int       `json:"attempt"`
	Result     string    `json:"result"` // delivered | retrying | failed | canceled
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// AdvanceResult reports what an Advance call did.
type AdvanceResult struct {
	VirtualNow  time.Time       `json:"virtual_now"`
	ClockOffset time.Duration   `json:"-"`
	Fired       []FiredDelivery `json:"fired"`
	// Partial is set when the call stopped before covering the whole window:
	// the context ended or the safety cap on attempts was reached. The clock
	// then stands at the last event that was processed.
	Partial bool   `json:"partial,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ClockOffset reports how far a session's clock runs ahead of wall time.
func (d *Dispatcher) ClockOffset(sessionID string) time.Duration {
	if sessionID == "" {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offsets[sessionID]
}

// Advance moves a session's clock forward by `by`, firing every delivery
// that becomes due in that window in session-time order and waiting for
// each attempt. Transitions triggered by those attempts may schedule more
// deliveries; the ones that fall inside the window fire in the same call.
func (d *Dispatcher) Advance(ctx context.Context, sessionID string, by time.Duration) (AdvanceResult, error) {
	if sessionID == "" {
		return AdvanceResult{}, errors.New("a session id is required")
	}
	if by < 0 {
		return AdvanceResult{}, errors.New("a session clock only moves forward")
	}
	if err := d.acquire(ctx, sessionID); err != nil {
		return AdvanceResult{}, err
	}
	defer d.release(sessionID)

	res := AdvanceResult{Fired: []FiredDelivery{}}
	remaining := by
	for {
		if err := ctx.Err(); err != nil {
			res.Partial, res.Reason = true, "deadline reached before the window was covered"
			break
		}
		if len(res.Fired) >= maxFiredPerAdvance {
			res.Partial, res.Reason = true, "stopped after the safety cap on attempts; the scenario may loop on zero-delay webhooks"
			break
		}
		d.mu.Lock()
		if d.gone[sessionID] {
			d.mu.Unlock()
			break
		}
		next := d.earliestLocked(sessionID)
		if next == nil {
			d.mu.Unlock()
			break
		}
		step := time.Until(next.fireAt)
		if step < 0 {
			step = 0
		}
		if step > remaining {
			d.mu.Unlock()
			break
		}
		d.shiftLocked(sessionID, step)
		remaining -= step
		next.running = true
		d.mu.Unlock()

		res.Fired = append(res.Fired, d.fire(next))
	}

	d.mu.Lock()
	if !res.Partial && !d.gone[sessionID] {
		d.shiftLocked(sessionID, remaining)
	}
	res.ClockOffset = d.offsets[sessionID]
	res.VirtualNow = time.Now().Add(res.ClockOffset)
	d.mu.Unlock()
	return res, nil
}

// fire runs one attempt synchronously and summarizes it.
func (d *Dispatcher) fire(del *delivery) FiredDelivery {
	off := d.ClockOffset(del.sessionID)
	att, status := d.attempt(del)
	at := att.At
	if at.IsZero() {
		at = time.Now()
	}
	return FiredDelivery{
		DeliveryID: del.id,
		Event:      del.event,
		Target:     del.targetName,
		VirtualAt:  at.Add(off),
		Attempt:    att.Num,
		Result:     status,
		StatusCode: att.StatusCode,
		Error:      att.Error,
	}
}

// acquire takes a session out of normal scheduling: its queued deliveries
// leave the heap, new ones stay out of it, and attempts already handed to
// workers are allowed to finish first.
func (d *Dispatcher) acquire(ctx context.Context, sessionID string) error {
	d.mu.Lock()
	if d.exclusive[sessionID] {
		d.mu.Unlock()
		return ErrSessionBusy
	}
	d.exclusive[sessionID] = true
	for _, del := range d.pending[sessionID] {
		if del.index >= 0 {
			heap.Remove(&d.heap, del.index)
		}
	}
	d.mu.Unlock()

	for {
		d.mu.Lock()
		busy := false
		for _, del := range d.pending[sessionID] {
			if del.running {
				busy = true
				break
			}
		}
		d.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			d.release(sessionID)
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// release returns a session to normal scheduling.
func (d *Dispatcher) release(sessionID string) {
	d.mu.Lock()
	delete(d.exclusive, sessionID)
	if d.gone[sessionID] {
		delete(d.gone, sessionID)
		delete(d.offsets, sessionID)
	}
	for _, del := range d.pending[sessionID] {
		if del.index < 0 && !del.running && !del.canceled.Load() {
			heap.Push(&d.heap, del)
		}
	}
	d.mu.Unlock()
	d.kick()
}

// shiftLocked moves a session's clock forward, keeping the invariant that
// fireAt + offset is each pending delivery's session due time. Caller holds
// d.mu and the session is exclusive, so no pending delivery is in the heap.
func (d *Dispatcher) shiftLocked(sessionID string, by time.Duration) {
	if by <= 0 {
		return
	}
	d.offsets[sessionID] += by
	for _, del := range d.pending[sessionID] {
		del.fireAt = del.fireAt.Add(-by)
		fireAt := del.fireAt
		d.log.update(del.id, func(r *Record) {
			if r.NextAttemptAt != nil {
				r.NextAttemptAt = &fireAt
			} else {
				r.FireAt = fireAt
			}
		})
	}
}

// earliestLocked picks the session's next due delivery: earliest fireAt,
// then scheduling order. Caller holds d.mu.
func (d *Dispatcher) earliestLocked(sessionID string) *delivery {
	var best *delivery
	for _, del := range d.pending[sessionID] {
		if del.canceled.Load() || del.running {
			continue
		}
		if best == nil || del.fireAt.Before(best.fireAt) ||
			(del.fireAt.Equal(best.fireAt) && del.seq < best.seq) {
			best = del
		}
	}
	return best
}
