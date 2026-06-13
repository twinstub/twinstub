package engine

import (
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/tmpl"
)

// When the per-session pending webhook limit is hit, the enter action is
// skipped with an error log instead of growing the queue unboundedly.
func TestPendingLimitSkipsScheduling(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	sched := &fakeSched{}
	eng := New(NewStore(10), sched, 1, 1, tmpl.New(1, true).ULID, zerolog.Nop())

	out := eng.Handle(snap, postPayment("ord_lim", 1), "payment.chargeback")
	if len(sched.scheduled) != 1 {
		t.Fatalf("scheduled = %d", len(sched.scheduled))
	}
	// Forced re-entry would schedule again, but the limit (1) blocks it.
	if err := eng.AdvanceTo(out.SessionID, "processing"); err != nil {
		t.Fatal(err)
	}
	if len(sched.scheduled) != 1 {
		t.Errorf("limit ignored, scheduled = %d", len(sched.scheduled))
	}
}

// A failing webhook body template must not block the state transition.
func TestWebhookTemplateErrorSkipsAction(t *testing.T) {
	cfg := `
version: 1
targets:
  cb: {url: "http://localhost:1/hook"}
scenarios:
  - id: s.broken
    session: {key: "query:order"}
    initial: a
    states:
      a:
        on:
          - when: {method: GET, path: /go}
            reply: {status: 200, body: '{}'}
            goto: b
      b:
        enter:
          - webhook:
              target: cb
              body: '{{ randInt 5 1 }}'
        on:
          - when: {method: GET, path: /state}
            reply: {status: 200, body: '{"in": "b"}'}
`
	snap := testSnapshot(t, cfg)
	eng, sched := testEngine(t, 10)

	r := postPayment("x", 1)
	r.Method, r.Path = "GET", "/go"
	r.Query.Set("order", "o1")
	out := eng.Handle(snap, r, "s.broken")
	if !out.Handled || out.State != "b" {
		t.Fatalf("transition blocked by template error: %+v", out)
	}
	if len(sched.scheduled) != 0 {
		t.Errorf("broken action was scheduled anyway")
	}
}

func TestRunSweeper(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 10)

	out := eng.Handle(snap, postPayment("ord_sw", 1), "payment.chargeback")
	sess := eng.Store.GetByID(out.SessionID)
	sess.mu.Lock()
	sess.ExpiresAt = time.Now().Add(-time.Second)
	sess.mu.Unlock()

	stop := eng.RunSweeper(10 * time.Millisecond)
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for eng.Store.GetByID(out.SessionID) != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if eng.Store.GetByID(out.SessionID) != nil {
		t.Error("sweeper never removed the expired session")
	}
}

// After a reload that drops a scenario, live sessions must still be
// reachable through the selector registry of the store.
func TestLiveSelectorLookupAfterScenarioRemoved(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 10)

	eng.Handle(snap, postPayment("ord_live", 7), "payment.chargeback")

	// New config version without any scenarios.
	emptySnap := testSnapshot(t, `
version: 1
endpoints:
  - id: ping
    when: {method: GET, path: /ping}
    reply: {status: 200}
`)
	out := eng.Handle(emptySnap, getPayment("ord_live"), "")
	if !out.Handled {
		t.Fatalf("session lost after scenario removal: %+v", out)
	}
}

func TestSessionView(t *testing.T) {
	snap := testSnapshot(t, testConfig)
	eng, _ := testEngine(t, 10)

	out := eng.Handle(snap, postPayment("ord_v", 3), "payment.chargeback")
	v := eng.Store.GetByID(out.SessionID).View()
	if v.Key != "ord_v" || v.Scenario != "payment.chargeback" || v.State != "processing" {
		t.Errorf("view = %+v", v)
	}
	if v.Vars["amount"] != "3" {
		t.Errorf("vars = %v", v.Vars)
	}
}
