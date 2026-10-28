package webhook

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DeliverResult reports what a DeliverNow call did.
type DeliverResult struct {
	Fired []FiredDelivery `json:"fired"`
	// Partial is set when the context ended before every item was sent.
	Partial bool   `json:"partial,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// NotPendingError is returned when an item names no pending delivery of the
// session. Deliveries sent before it stay sent.
type NotPendingError struct {
	Item    string
	Pending []string // events (or delivery ids, for unnamed ones) still queued
}

func (e *NotPendingError) Error() string {
	if len(e.Pending) == 0 {
		return fmt.Sprintf("%q is not pending: the session has no queued webhooks", e.Item)
	}
	return fmt.Sprintf("%q is not pending; queued now: %s", e.Item, strings.Join(e.Pending, ", "))
}

// DeliverNow sends a session's queued webhooks immediately, in the given
// order, ignoring their schedule. Each item is a delivery id or an event
// name; an event name picks the earliest queued delivery of that event.
// Transitions triggered by an earlier item may queue a later one, so
// ["payment.succeeded", "payment.chargeback"] works for a chained flow.
// The session clock does not move: deliveries left over keep their
// schedule, and retries of failed attempts continue on it.
func (d *Dispatcher) DeliverNow(ctx context.Context, sessionID string, items []string) (DeliverResult, error) {
	if sessionID == "" {
		return DeliverResult{}, errors.New("a session id is required")
	}
	if len(items) == 0 {
		return DeliverResult{}, errors.New("name at least one event or delivery id")
	}
	if err := d.acquire(ctx, sessionID); err != nil {
		return DeliverResult{}, err
	}
	defer d.release(sessionID)

	res := DeliverResult{Fired: []FiredDelivery{}}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			res.Partial, res.Reason = true, "deadline reached before every item was sent"
			return res, nil
		}
		d.mu.Lock()
		if d.gone[sessionID] {
			d.mu.Unlock()
			return res, &NotPendingError{Item: item}
		}
		del := d.findLocked(sessionID, item)
		if del == nil {
			pending := d.pendingNamesLocked(sessionID)
			d.mu.Unlock()
			return res, &NotPendingError{Item: item, Pending: pending}
		}
		del.running = true
		d.mu.Unlock()

		res.Fired = append(res.Fired, d.fire(del))
	}
	return res, nil
}

// findLocked resolves an item to a queued delivery: an exact delivery id
// first, otherwise the earliest delivery of that event. Caller holds d.mu.
func (d *Dispatcher) findLocked(sessionID, item string) *delivery {
	if del, ok := d.pending[sessionID][item]; ok && !del.canceled.Load() && !del.running {
		return del
	}
	var best *delivery
	for _, del := range d.pending[sessionID] {
		if del.event != item || del.canceled.Load() || del.running {
			continue
		}
		if best == nil || del.fireAt.Before(best.fireAt) ||
			(del.fireAt.Equal(best.fireAt) && del.seq < best.seq) {
			best = del
		}
	}
	return best
}

// pendingNamesLocked lists what is queued for a session, in due order, for
// error messages. Caller holds d.mu.
func (d *Dispatcher) pendingNamesLocked(sessionID string) []string {
	dels := make([]*delivery, 0, len(d.pending[sessionID]))
	for _, del := range d.pending[sessionID] {
		if !del.canceled.Load() {
			dels = append(dels, del)
		}
	}
	sort.Slice(dels, func(i, j int) bool {
		if dels[i].fireAt.Equal(dels[j].fireAt) {
			return dels[i].seq < dels[j].seq
		}
		return dels[i].fireAt.Before(dels[j].fireAt)
	})
	out := make([]string, len(dels))
	for i, del := range dels {
		if del.event != "" {
			out[i] = del.event
		} else {
			out[i] = del.id
		}
	}
	return out
}
