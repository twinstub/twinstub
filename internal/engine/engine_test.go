package engine

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/match"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
	"github.com/twinstub/twinstub/internal/webhook"
)

const testConfig = `
version: 1
targets:
  cb:
    url: http://localhost:1/hook
scenarios:
  - id: payment.chargeback
    session:
      key: "body:$.order_id"
      ttl: 30m
    initial: new
    states:
      new:
        on:
          - when: {method: POST, path: /v1/payments}
            reply:
              status: 201
              body: '{"id": "pay_{{ .Session.ID }}", "status": "processing"}'
            set:
              amount: "{{ .Request.Body.amount }}"
            goto: processing
      processing:
        enter:
          - webhook:
              target: cb
              event: payment.succeeded
              after: 10s
              body: '{"amount": {{ .Session.Vars.amount }}}'
            goto: succeeded
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{"status": "processing"}'}
      succeeded:
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{"status": "succeeded"}'}
  - id: kyc.flow
    session:
      key: "header:X-Client-ID"
      ttl: 5m
    initial: pending
    states:
      pending:
        on:
          - when: {method: GET, path: /v1/kyc}
            reply: {status: 200, body: '{"kyc": "pending"}'}
`

type fakeSched struct {
	mu        sync.Mutex
	scheduled []webhook.ScheduleRequest
	canceled  []string
}

func (f *fakeSched) Schedule(req webhook.ScheduleRequest) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduled = append(f.scheduled, req)
	return fmt.Sprintf("dlv_%d", len(f.scheduled))
}

func (f *fakeSched) CancelSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, id)
}

func (f *fakeSched) PendingCount(string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scheduled)
}

func (f *fakeSched) last(t *testing.T) webhook.ScheduleRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scheduled) == 0 {
		t.Fatal("nothing scheduled")
	}
	return f.scheduled[len(f.scheduled)-1]
}

func testSnapshot(t *testing.T, yaml string) *snapshot.Snapshot {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "twinstub.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	snap, err := snapshot.Compile(cfg, tmpl.New(1, true), 1)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return snap
}

func testEngine(t *testing.T, maxSessions int) (*Engine, *fakeSched) {
	t.Helper()
	sched := &fakeSched{}
	eng := New(NewStore(maxSessions), sched, 0.5, 100, tmpl.New(1, true).ULID, zerolog.Nop())
	return eng, sched
}

func postPayment(orderID string, amount float64) *match.Request {
	return &match.Request{
		Method:  "POST",
		Path:    "/v1/payments",
		Query:   url.Values{},
		Headers: http.Header{},
		Body:    map[string]any{"order_id": orderID, "amount": amount},
		RawBody: []byte(fmt.Sprintf(`{"order_id":%q,"amount":%v}`, orderID, amount)),
	}
}

func getPayment(orderID string) *match.Request {
	return &match.Request{
		Method:  "GET",
		Path:    "/v1/payments/pay_1",
		Query:   url.Values{},
		Headers: http.Header{},
		Body:    map[string]any{"order_id": orderID},
		RawBody: []byte(fmt.Sprintf(`{"order_id":%q}`, orderID)),
	}
}

func TestScenarioFlow(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, sched := testEngine(t, 100)

	// First request creates the session and transitions new -> processing.
	out := eng.Handle(snap, postPayment("ord_1", 1999), "payment.chargeback")
	if !out.Handled {
		t.Fatalf("not handled: %+v", out)
	}
	if out.Reply.Status != 201 {
		t.Errorf("status = %d", out.Reply.Status)
	}
	if !strings.Contains(string(out.Reply.Body), `"id": "pay_`) {
		t.Errorf("body = %s", out.Reply.Body)
	}
	if out.State != "processing" {
		t.Errorf("state = %s", out.State)
	}

	// Entering processing scheduled the webhook with the scaled delay.
	req := sched.last(t)
	if req.Event != "payment.succeeded" {
		t.Errorf("event = %s", req.Event)
	}
	if req.Delay != 5*time.Second { // 10s * 0.5 time scale
		t.Errorf("delay = %v, want 5s (time scale applied)", req.Delay)
	}
	if string(req.Body) != `{"amount": 1999}` {
		t.Errorf("webhook body = %s", req.Body)
	}

	// Second request joins the same session by body key, header absent.
	out = eng.Handle(snap, getPayment("ord_1"), "")
	if !out.Handled || !strings.Contains(string(out.Reply.Body), "processing") {
		t.Fatalf("poll outcome: %+v", out)
	}

	// Scenario header on a follow-up request is ignored, not a recreate.
	out = eng.Handle(snap, getPayment("ord_1"), "payment.chargeback")
	if !out.Handled {
		t.Fatalf("header on existing session must be ignored: %+v", out)
	}

	// Post-dispatch goto: first attempt fires, session advances.
	if req.OnFirstAttempt == nil {
		t.Fatal("OnFirstAttempt not set")
	}
	req.OnFirstAttempt()
	out = eng.Handle(snap, getPayment("ord_1"), "")
	if !strings.Contains(string(out.Reply.Body), "succeeded") {
		t.Errorf("after dispatch goto, body = %s", out.Reply.Body)
	}
}

func TestSessionConflict409(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	eng.Handle(snap, postPayment("ord_x", 1), "payment.chargeback")
	out := eng.Handle(snap, getPayment("ord_x"), "kyc.flow")
	if out.Err == nil || out.Err.Status != 409 {
		t.Fatalf("expected 409, got %+v", out)
	}
	if !strings.Contains(fmt.Sprint(out.Err.Body["detail"]), "already bound") {
		t.Errorf("detail: %v", out.Err.Body)
	}
}

func TestMissingKey422(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	r := &match.Request{Method: "POST", Path: "/v1/payments", Query: url.Values{}, Headers: http.Header{}}
	out := eng.Handle(snap, r, "payment.chargeback")
	if out.Err == nil || out.Err.Status != 422 {
		t.Fatalf("expected 422, got %+v", out)
	}
	if !strings.Contains(fmt.Sprint(out.Err.Body["detail"]), "body:$.order_id") {
		t.Errorf("detail should name the selector: %v", out.Err.Body)
	}
}

func TestUnknownScenario404(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("o", 1), "nope.scenario")
	if out.Err == nil || out.Err.Status != 404 {
		t.Fatalf("expected 404, got %+v", out)
	}
}

func TestNoHeaderNoSessionPassesThrough(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("ord_pass", 1), "")
	if out.Handled || out.Err != nil {
		t.Fatalf("expected pass-through, got %+v", out)
	}
}

func TestFallThroughKeepsSessionInfo(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	eng.Handle(snap, postPayment("ord_f", 1), "payment.chargeback")
	r := &match.Request{
		Method: "DELETE", Path: "/v1/unknown",
		Query: url.Values{}, Headers: http.Header{},
		Body: map[string]any{"order_id": "ord_f"},
	}
	out := eng.Handle(snap, r, "")
	if out.Handled || !out.InSession {
		t.Fatalf("expected in-session fall-through, got %+v", out)
	}
	if out.State != "processing" {
		t.Errorf("state = %s", out.State)
	}
}

func TestSessionLimit429(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 2)

	eng.Handle(snap, postPayment("o1", 1), "payment.chargeback")
	eng.Handle(snap, postPayment("o2", 1), "payment.chargeback")
	out := eng.Handle(snap, postPayment("o3", 1), "payment.chargeback")
	if out.Err == nil || out.Err.Status != 429 {
		t.Fatalf("expected 429, got %+v", out)
	}
}

func TestHeaderSelector(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	r := &match.Request{Method: "GET", Path: "/v1/kyc", Query: url.Values{}, Headers: http.Header{}}
	r.Headers.Set("X-Client-ID", "client_7")
	out := eng.Handle(snap, r, "kyc.flow")
	if !out.Handled {
		t.Fatalf("kyc not handled: %+v", out)
	}
	if sess := eng.Store.GetByKey("client_7"); sess == nil {
		t.Error("session not keyed by header value")
	}
}

func TestTTLSweep(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, sched := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("ord_ttl", 1), "payment.chargeback")
	sess := eng.Store.GetByID(out.SessionID)
	if sess == nil {
		t.Fatal("session missing")
	}
	sess.mu.Lock()
	sess.ExpiresAt = time.Now().Add(-time.Second)
	sess.mu.Unlock()

	eng.SweepExpired()
	if eng.Store.GetByID(out.SessionID) != nil {
		t.Error("expired session not swept")
	}
	if len(sched.canceled) != 1 || sched.canceled[0] != out.SessionID {
		t.Errorf("webhooks not canceled: %v", sched.canceled)
	}
}

func TestSlidingTTL(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("ord_slide", 1), "payment.chargeback")
	sess := eng.Store.GetByID(out.SessionID)
	sess.mu.Lock()
	before := sess.ExpiresAt
	sess.ExpiresAt = before.Add(-10 * time.Minute)
	sess.mu.Unlock()

	eng.Handle(snap, getPayment("ord_slide"), "")
	sess.mu.Lock()
	after := sess.ExpiresAt
	sess.mu.Unlock()
	if !after.After(before.Add(-time.Minute)) {
		t.Errorf("TTL not extended: before=%v after=%v", before, after)
	}
}

func TestAdvanceTo(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, sched := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("ord_a", 5), "payment.chargeback")
	n := len(sched.scheduled)

	if err := eng.AdvanceTo(out.SessionID, "processing"); err != nil {
		t.Fatal(err)
	}
	// Forced advance re-enters processing: enter actions run again.
	if len(sched.scheduled) != n+1 {
		t.Errorf("enter actions did not run on advance")
	}
	if err := eng.AdvanceTo(out.SessionID, "nope"); err == nil {
		t.Error("expected error for unknown state")
	}
	if err := eng.AdvanceTo("missing", "processing"); err == nil {
		t.Error("expected error for unknown session")
	}
}

// A stale post-dispatch goto must not yank the session backwards.
func TestStaleAdvanceIgnored(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, sched := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("ord_e", 5), "payment.chargeback")
	cb := sched.last(t).OnFirstAttempt

	// Admin moves the session before the webhook's first attempt completes.
	if err := eng.AdvanceTo(out.SessionID, "succeeded"); err != nil {
		t.Fatal(err)
	}
	cb() // stale: epoch moved on
	sess := eng.Store.GetByID(out.SessionID)
	if sess.State != "succeeded" {
		t.Errorf("stale goto applied, state = %s", sess.State)
	}
}

func TestDeleteSessionCancelsWebhooks(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, sched := testEngine(t, 100)

	out := eng.Handle(snap, postPayment("ord_d", 5), "payment.chargeback")
	if !eng.DeleteSession(out.SessionID) {
		t.Fatal("delete failed")
	}
	if len(sched.canceled) != 1 {
		t.Errorf("cancel not called: %v", sched.canceled)
	}
	if eng.DeleteSession(out.SessionID) {
		t.Error("double delete should report false")
	}
	// Key is free again: a new scenario can start.
	out = eng.Handle(snap, getPayment("ord_d"), "kyc.flow")
	if out.Err != nil && out.Err.Status == 409 {
		t.Error("key still bound after delete")
	}
}

func TestStoreShardingAndList(t *testing.T) {
	store := NewStore(0)
	snap := testSnapshot(t, testConfig)
	sc := snap.ScenarioByID["payment.chargeback"]
	for i := 0; i < 100; i++ {
		_, created, err := store.Create(&Session{
			ID: fmt.Sprintf("id_%d", i), Key: fmt.Sprintf("key_%d", i),
			Scenario: sc, State: "new", Vars: map[string]string{},
			ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil || !created {
			t.Fatalf("create %d: %v %v", i, created, err)
		}
	}
	if store.Len() != 100 {
		t.Errorf("len = %d", store.Len())
	}
	if got := len(store.List()); got != 100 {
		t.Errorf("list = %d", got)
	}
	if s := store.GetByKey("key_42"); s == nil || s.ID != "id_42" {
		t.Errorf("GetByKey: %+v", s)
	}
	store.Delete("id_42")
	if store.GetByKey("key_42") != nil || store.Len() != 99 {
		t.Error("delete did not clean both indexes")
	}
}
