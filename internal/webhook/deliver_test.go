package webhook

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The demo bug: completed arrives before pending. DeliverNow stages it on
// demand regardless of the schedule.
func TestDeliverNowInGivenOrder(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "payment.pending", Body: []byte(`{}`), Delay: time.Hour})
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "payment.completed", Body: []byte(`{}`), Delay: 2 * time.Hour})

	res, err := d.DeliverNow(context.Background(), "s1", []string{"payment.completed", "payment.pending"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"payment.completed", "payment.pending"}
	if got := rec.got(); !equal(got, want) {
		t.Errorf("received = %v, want %v", got, want)
	}
	if len(res.Fired) != 2 || res.Fired[0].Result != StatusDelivered {
		t.Errorf("fired = %+v", res.Fired)
	}
	if n := d.PendingCount("s1"); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
	if off := d.ClockOffset("s1"); off != 0 {
		t.Errorf("DeliverNow moved the session clock: %v", off)
	}
}

// A later item may be queued by the transition an earlier one triggers.
func TestDeliverNowFollowsCascade(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	d.Schedule(ScheduleRequest{
		SessionID: "s1", TargetName: "cb", Target: tg, Event: "payment.succeeded", Body: []byte(`{}`), Delay: time.Hour,
		OnFirstAttempt: func() {
			d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "payment.chargeback", Body: []byte(`{}`), Delay: 30 * day})
		},
	})

	if _, err := d.DeliverNow(context.Background(), "s1", []string{"payment.succeeded", "payment.chargeback"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); !equal(got, []string{"payment.succeeded", "payment.chargeback"}) {
		t.Errorf("received = %v", got)
	}
}

func TestDeliverNowByDeliveryID(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "dup", Body: []byte(`{"n":1}`), Delay: time.Hour})
	second := d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "dup", Body: []byte(`{"n":2}`), Delay: 2 * time.Hour})

	res, err := d.DeliverNow(context.Background(), "s1", []string{second})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fired) != 1 || res.Fired[0].DeliveryID != second {
		t.Errorf("fired = %+v, want only %s", res.Fired, second)
	}
	if n := d.PendingCount("s1"); n != 1 {
		t.Errorf("pending = %d, want the other dup left", n)
	}
}

// An item that is not queued stops the call with a list of what is, and
// leaves the session's schedule intact.
func TestDeliverNowNotPending(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: target(srv.URL, 1, time.Second), Event: "soon", Body: []byte(`{}`), Delay: 100 * time.Millisecond})

	res, err := d.DeliverNow(context.Background(), "s1", []string{"payment.refunded"})
	var np *NotPendingError
	if !errors.As(err, &np) {
		t.Fatalf("err = %v, want NotPendingError", err)
	}
	if np.Item != "payment.refunded" || !equal(np.Pending, []string{"soon"}) {
		t.Errorf("error = %+v", np)
	}
	if len(res.Fired) != 0 {
		t.Errorf("fired = %+v", res.Fired)
	}
	// Released back to the scheduler: it still fires on time.
	waitStatus(t, d, id, StatusDelivered)
}

// Deliveries not named keep their schedule after the call.
func TestDeliverNowLeavesTheRestScheduled(t *testing.T) {
	var rec eventRecorder
	srv := rec.server(t)
	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	soon := d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "soon", Body: []byte(`{}`), Delay: 100 * time.Millisecond})
	d.Schedule(ScheduleRequest{SessionID: "s1", TargetName: "cb", Target: tg, Event: "later", Body: []byte(`{}`), Delay: time.Hour})

	if _, err := d.DeliverNow(context.Background(), "s1", []string{"later"}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, d, soon, StatusDelivered)
	if got := rec.got(); !equal(got, []string{"later", "soon"}) {
		t.Errorf("received = %v", got)
	}
}

func TestDeliverNowRejectsBadInput(t *testing.T) {
	d := testDispatcher(t)
	if _, err := d.DeliverNow(context.Background(), "", []string{"x"}); err == nil {
		t.Error("empty session id accepted")
	}
	if _, err := d.DeliverNow(context.Background(), "s1", nil); err == nil {
		t.Error("empty item list accepted")
	}
}
