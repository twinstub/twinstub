package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const day = 24 * time.Hour

// eventRecorder is a webhook receiver that remembers event order.
type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.events = append(r.events, req.Header.Get("X-TwinStub-Event"))
		r.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *eventRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func firedEvents(res AdvanceResult) []string {
	out := make([]string, len(res.Fired))
	for i, f := range res.Fired {
		out[i] = f.Event
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAdvanceFiresDueDeliveriesInOrder(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)

	// Scheduled out of order on purpose.
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "late", Body: []byte(`{}`), Delay: 10 * day})
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "second", Body: []byte(`{}`), Delay: 2 * time.Hour})
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "first", Body: []byte(`{}`), Delay: time.Hour})

	res, err := d.Advance(context.Background(), "s1", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := firedEvents(res); !equal(got, []string{"first", "second"}) {
		t.Errorf("fired = %v, want [first second]", got)
	}
	if got := rec.got(); !equal(got, []string{"first", "second"}) {
		t.Errorf("received = %v", got)
	}
	if res.ClockOffset != 3*time.Hour {
		t.Errorf("offset = %v, want 3h", res.ClockOffset)
	}
	if d := time.Until(res.VirtualNow) - 3*time.Hour; d < -time.Second || d > time.Second {
		t.Errorf("virtual now = %v, want about 3h ahead", res.VirtualNow)
	}

	// The late delivery is still pending and due 10 days after creation in
	// session time.
	pend := d.PendingForSession("s1")
	if len(pend) != 1 || pend[0].Event != "late" {
		t.Fatalf("pending = %+v", pend)
	}
	if d := time.Until(pend[0].FireAt) - 10*day; d < -time.Second || d > time.Second {
		t.Errorf("late due at %v, want about 10 days from now", pend[0].FireAt)
	}
	if res.Fired[0].VirtualAt.Before(time.Now().Add(time.Hour - time.Second)) {
		t.Errorf("first fired at session time %v, want about 1h ahead", res.Fired[0].VirtualAt)
	}
}

// A transition triggered by a delivery schedules the next one; when it falls
// inside the window it fires in the same call ("succeeds, then a chargeback
// 30 days later").
func TestAdvanceFollowsCascades(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)

	d.Schedule(ScheduleRequest{
		SessionID: "s1", TargetName: "cb", Target: tg, Event: "payment.succeeded", Body: []byte(`{}`),
		Delay: 3 * time.Second,
		OnFirstAttempt: func() {
			d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "payment.chargeback", Body: []byte(`{}`), Delay: 30 * day})
		},
	})

	res, err := d.Advance(context.Background(), "s1", 31*day)
	if err != nil {
		t.Fatal(err)
	}
	if got := firedEvents(res); !equal(got, []string{"payment.succeeded", "payment.chargeback"}) {
		t.Errorf("fired = %v", got)
	}
	if n := d.PendingCount("s1"); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
	cb := res.Fired[1].VirtualAt
	if d := time.Until(cb) - (30*day + 3*time.Second); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("chargeback at session time %v, want about 30d3s ahead", cb)
	}
}

// Retries of a failing delivery follow session time too, so "the handler
// returned 5xx, the provider retried" plays out inside the window.
func TestAdvanceRetriesInSessionTime(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: target(srv.URL, 3, time.Hour), Event: "e", Body: []byte(`{}`), Delay: time.Minute})

	res, err := d.Advance(context.Background(), "s1", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fired) != 2 || res.Fired[0].Result != StatusRetrying || res.Fired[1].Result != StatusDelivered {
		t.Fatalf("fired = %+v", res.Fired)
	}
	rec, _ := d.Log().Get(id)
	if rec.Status != StatusDelivered || len(rec.Attempts) != 2 {
		t.Errorf("record = %+v", rec)
	}
	for _, a := range rec.Attempts {
		if a.VirtualAt == nil {
			t.Error("attempt in an advanced session has no virtual_at")
		}
	}
}

func TestAdvanceLeavesOtherSessionsAlone(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)

	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "mine", Body: []byte(`{}`), Delay: time.Hour})
	d.Schedule(ScheduleRequest{SessionID: "s2", TargetName: "cb", Target: tg, Event: "theirs", Body: []byte(`{}`), Delay: time.Hour})

	if _, err := d.Advance(context.Background(), "s1", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); !equal(got, []string{"mine"}) {
		t.Errorf("received = %v, want only [mine]", got)
	}
	if n := d.PendingCount("s2"); n != 1 {
		t.Errorf("other session pending = %d, want 1", n)
	}
	if off := d.ClockOffset("s2"); off != 0 {
		t.Errorf("other session offset = %v", off)
	}
}

// After Advance the session returns to wall time: what is left fires on the
// normal scheduler at its shifted time.
func TestAdvanceHandsBackToWallClock(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: target(srv.URL, 1, time.Second), Event: "soon", Body: []byte(`{}`), Delay: time.Hour + 100*time.Millisecond})

	res, err := d.Advance(context.Background(), "s1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fired) != 0 {
		t.Fatalf("fired too early: %+v", res.Fired)
	}
	waitStatus(t, d, id, StatusDelivered)
}

func TestAdvanceSessionBusy(t *testing.T) {
	d := testDispatcher(t)
	if err := d.acquire(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	defer d.release("s1")
	if _, err := d.Advance(context.Background(), "s1", time.Hour); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("err = %v, want ErrSessionBusy", err)
	}
}

func TestAdvanceRejectsBadInput(t *testing.T) {
	d := testDispatcher(t)
	if _, err := d.Advance(context.Background(), "", time.Hour); err == nil {
		t.Error("empty session id accepted")
	}
	if _, err := d.Advance(context.Background(), "s1", -time.Hour); err == nil {
		t.Error("negative duration accepted")
	}
}

// A deadline stops the replay early: the result says so and the clock stays
// at the last processed event instead of jumping to the end of the window.
func TestAdvanceDeadlineIsPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	for _, delay := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "e", Body: []byte(`{}`), Delay: delay})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := d.Advance(ctx, "s1", 10*day)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Partial || res.Reason == "" {
		t.Errorf("result not marked partial: %+v", res)
	}
	if res.ClockOffset >= 10*day {
		t.Errorf("clock jumped to the end of the window on a partial run: %v", res.ClockOffset)
	}
}

// Deleting a session while it is being advanced must not leave its clock
// behind.
func TestCancelDuringAdvanceDropsClock(t *testing.T) {
	var once sync.Once
	var d *Dispatcher
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { d.CancelSession("s1") })
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d = testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "a", Body: []byte(`{}`), Delay: time.Hour})
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "b", Body: []byte(`{}`), Delay: 2 * time.Hour})

	res, err := d.Advance(context.Background(), "s1", 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fired) != 1 {
		t.Errorf("fired after cancel: %+v", res.Fired)
	}
	if off := d.ClockOffset("s1"); off != 0 {
		t.Errorf("clock of a deleted session survived: %v", off)
	}
}
