package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

const intSecret = "whsec_int_test"

// receiver is a local webhook sink that records every delivery.
type receiver struct {
	mu    sync.Mutex
	hooks []hook
	srv   *httptest.Server
	ch    chan hook
}

type hook struct {
	At      time.Time
	Event   string
	Body    []byte
	Headers http.Header
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{ch: make(chan hook, 32)}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h := hook{At: time.Now(), Event: r.Header.Get("X-TwinStub-Event"), Body: b, Headers: r.Header.Clone()}
		rc.mu.Lock()
		rc.hooks = append(rc.hooks, h)
		rc.mu.Unlock()
		rc.ch <- h
		w.WriteHeader(200)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) wait(t *testing.T) hook {
	t.Helper()
	select {
	case h := <-rc.ch:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("webhook did not arrive in time")
		return hook{}
	}
}

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "twinstub.yaml")
}

func projectFiles(receiverURL string) map[string]string {
	root := fmt.Sprintf(`
version: 1
server:
  port: 8080
  admin:
    enabled: true
    port: 9090
targets:
  merchant:
    url: %s
    headers: {X-Source: twinstub}
    signing:
      algorithm: hmac-sha256
      secret: %s
    retry: {attempts: 3, backoff: 50ms}
include:
  - endpoints/*.yaml
  - scenarios/*.yaml
`, receiverURL, intSecret)

	endpoints := `
version: 1
endpoints:
  - id: ping
    when: {method: GET, path: /ping}
    reply: {status: 200, body: '{"pong": true, "rev": "v1"}'}
`

	// Real intervals 10s and 20s; --time-scale=0.01 compresses them to
	// 100ms and 200ms, which is exactly what the spec sells.
	scenario := `
version: 1
scenarios:
  - id: payment.chargeback
    description: "Payment succeeds, then a chargeback arrives"
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
              body: '{"id": "pay_{{ .Session.ID }}", "order_id": "{{ .Session.Key }}", "status": "processing", "rev": "v1"}'
            set:
              amount: "{{ .Request.Body.amount }}"
            goto: processing
      processing:
        enter:
          - webhook:
              target: merchant
              event: payment.succeeded
              after: 10s
              body: '{"event": "payment.succeeded", "amount": {{ .Session.Vars.amount }}}'
            goto: succeeded
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{"status": "processing"}'}
      succeeded:
        enter:
          - webhook:
              target: merchant
              event: payment.chargeback
              after: 20s
              body: '{"event": "payment.chargeback", "reason": "fraudulent"}'
            goto: chargeback
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{"status": "succeeded"}'}
      chargeback:
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{"status": "chargeback"}'}
`
	return map[string]string{
		"twinstub.yaml":          root,
		"endpoints/ping.yaml":    endpoints,
		"scenarios/payment.yaml": scenario,
	}
}

func startApp(t *testing.T, configPath string) *App {
	t.Helper()
	a, err := New(Options{
		ConfigPath:    configPath,
		Port:          -1,
		AdminPort:     -1,
		TimeScale:     0.01,
		Seed:          42,
		Seeded:        true,
		AllowPrivate:  true,
		Logger:        zerolog.Nop(),
		SweepInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if err := a.Start(); err != nil {
		t.Fatalf("app.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	return a
}

func (a *App) publicURL() string { return fmt.Sprintf("http://127.0.0.1:%d", a.PublicPort()) }
func (a *App) adminURL() string  { return fmt.Sprintf("http://127.0.0.1:%d", a.AdminPort()) }

func doJSON(t *testing.T, method, url string, headers map[string]string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func verifySignature(t *testing.T, h hook) {
	t.Helper()
	sig := h.Headers.Get("X-TwinStub-Signature")
	if sig == "" {
		t.Fatal("signature header missing")
	}
	var ts, v1 string
	for _, part := range strings.Split(sig, ",") {
		if s, ok := strings.CutPrefix(part, "t="); ok {
			ts = s
		}
		if s, ok := strings.CutPrefix(part, "v1="); ok {
			v1 = s
		}
	}
	if ts == "" || v1 == "" {
		t.Fatalf("unexpected signature format: %q", sig)
	}
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(tsInt, 0)) > time.Minute {
		t.Fatalf("bad signature timestamp %q", ts)
	}
	mac := hmac.New(sha256.New, []byte(intSecret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(h.Body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(v1)) {
		t.Errorf("signature mismatch: got %s want %s", v1, want)
	}
}

// TestEndToEnd is the acceptance case from spec section 2: scenario header,
// deterministic sync reply, a chain of signed webhooks on schedule, state
// visible between hooks, delivery journal and replay over the admin API.
func TestEndToEnd(t *testing.T) {
	rc := newReceiver(t)
	a := startApp(t, writeProject(t, projectFiles(rc.srv.URL)))

	t0 := time.Now()
	code, body := doJSON(t, "POST", a.publicURL()+"/v1/payments",
		map[string]string{"X-TwinStub-Scenario": "payment.chargeback"},
		`{"order_id": "ord_42", "amount": 1999}`)
	if code != 201 {
		t.Fatalf("POST payment: %d %s", code, body)
	}
	var created struct {
		ID      string `json:"id"`
		OrderID string `json:"order_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.OrderID != "ord_42" || created.Status != "processing" || !strings.HasPrefix(created.ID, "pay_") {
		t.Fatalf("created = %+v", created)
	}

	poll := func() string {
		code, b := doJSON(t, "GET", a.publicURL()+"/v1/payments/"+created.ID, nil, `{"order_id": "ord_42"}`)
		if code != 200 {
			t.Fatalf("poll: %d %s", code, b)
		}
		var s struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(b, &s)
		return s.Status
	}
	if got := poll(); got != "processing" {
		t.Fatalf("state before first webhook: %q", got)
	}

	// First webhook: payment.succeeded after 10s * 0.01 = 100ms.
	h1 := rc.wait(t)
	if h1.Event != "payment.succeeded" {
		t.Fatalf("first event = %q", h1.Event)
	}
	if d := h1.At.Sub(t0); d < 80*time.Millisecond {
		t.Errorf("first webhook too early: %v (time scale not applied?)", d)
	}
	if !strings.Contains(string(h1.Body), `"amount": 1999`) {
		t.Errorf("webhook body = %s", h1.Body)
	}
	verifySignature(t, h1)
	if h1.Headers.Get("X-TwinStub-Delivery") == "" || h1.Headers.Get("X-TwinStub-Session") == "" {
		t.Error("standard delivery headers missing")
	}
	if ua := h1.Headers.Get("User-Agent"); !strings.HasPrefix(ua, "twinstub/") {
		t.Errorf("user agent = %q", ua)
	}

	// State machine has memory: same session, new state.
	deadline := time.Now().Add(2 * time.Second)
	for poll() != "succeeded" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := poll(); got != "succeeded" {
		t.Fatalf("state after first webhook: %q", got)
	}

	// Second webhook: the chargeback "30 days later".
	h2 := rc.wait(t)
	if h2.Event != "payment.chargeback" {
		t.Fatalf("second event = %q", h2.Event)
	}
	if gap := h2.At.Sub(h1.At); gap < 150*time.Millisecond {
		t.Errorf("chargeback arrived too early after success: %v", gap)
	}
	verifySignature(t, h2)

	deadline = time.Now().Add(2 * time.Second)
	for poll() != "chargeback" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := poll(); got != "chargeback" {
		t.Fatalf("terminal state: %q", got)
	}

	// Delivery journal over the admin API.
	code, b := doJSON(t, "GET", a.adminURL()+"/api/v1/deliveries", nil, "")
	if code != 200 {
		t.Fatalf("deliveries: %d %s", code, b)
	}
	var dl struct {
		Deliveries []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Event  string `json:"event"`
		} `json:"deliveries"`
		Count int `json:"count"`
	}
	_ = json.Unmarshal(b, &dl)
	if dl.Count != 2 {
		t.Fatalf("delivery count = %d, body %s", dl.Count, b)
	}
	for _, d := range dl.Deliveries {
		if d.Status != "delivered" {
			t.Errorf("delivery %s status = %s", d.ID, d.Status)
		}
	}

	// Replay and Edit: resend the first delivery with a tweaked body.
	first := dl.Deliveries[len(dl.Deliveries)-1]
	code, b = doJSON(t, "POST", a.adminURL()+"/api/v1/deliveries/"+first.ID+"/replay", nil,
		`{"body": "{\"event\": \"payment.succeeded\", \"replayed\": true}"}`)
	if code != 202 {
		t.Fatalf("replay: %d %s", code, b)
	}
	h3 := rc.wait(t)
	if !strings.Contains(string(h3.Body), `"replayed": true`) {
		t.Errorf("replayed body = %s", h3.Body)
	}
	verifySignature(t, h3)
}

func TestAdminSessionLifecycle(t *testing.T) {
	rc := newReceiver(t)
	a := startApp(t, writeProject(t, projectFiles(rc.srv.URL)))

	doJSON(t, "POST", a.publicURL()+"/v1/payments",
		map[string]string{"X-TwinStub-Scenario": "payment.chargeback"},
		`{"order_id": "ord_admin", "amount": 5}`)

	code, b := doJSON(t, "GET", a.adminURL()+"/api/v1/sessions", nil, "")
	if code != 200 {
		t.Fatalf("sessions: %d", code)
	}
	var sessions struct {
		Sessions []struct {
			ID       string `json:"id"`
			Key      string `json:"key"`
			Scenario string `json:"scenario"`
			State    string `json:"state"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(b, &sessions)
	if len(sessions.Sessions) != 1 {
		t.Fatalf("sessions = %s", b)
	}
	s := sessions.Sessions[0]
	if s.Key != "ord_admin" || s.Scenario != "payment.chargeback" || s.State != "processing" {
		t.Errorf("session = %+v", s)
	}

	// Detail includes vars and pending webhooks.
	code, b = doJSON(t, "GET", a.adminURL()+"/api/v1/sessions/"+s.ID, nil, "")
	if code != 200 || !strings.Contains(string(b), `"amount": "5"`) {
		t.Fatalf("detail: %d %s", code, b)
	}

	// Forced advance into chargeback (executes enter actions of the state).
	code, b = doJSON(t, "POST", a.adminURL()+"/api/v1/sessions/"+s.ID+"/advance", nil, `{"state": "chargeback"}`)
	if code != 200 || !strings.Contains(string(b), `"state": "chargeback"`) {
		t.Fatalf("advance: %d %s", code, b)
	}
	code, b = doJSON(t, "POST", a.adminURL()+"/api/v1/sessions/"+s.ID+"/advance", nil, `{"state": "bogus"}`)
	if code != 404 {
		t.Fatalf("advance to bogus state: %d %s", code, b)
	}

	// Reset the session.
	code, _ = doJSON(t, "DELETE", a.adminURL()+"/api/v1/sessions/"+s.ID, nil, "")
	if code != 200 {
		t.Fatalf("delete: %d", code)
	}
	code, b = doJSON(t, "GET", a.adminURL()+"/api/v1/sessions", nil, "")
	if code != 200 || !strings.Contains(string(b), `"count": 0`) {
		t.Errorf("sessions after delete: %d %s", code, b)
	}

	// Scenario catalog and health.
	code, b = doJSON(t, "GET", a.adminURL()+"/api/v1/scenarios", nil, "")
	if code != 200 || !strings.Contains(string(b), "payment.chargeback") {
		t.Fatalf("scenarios: %d %s", code, b)
	}
	code, b = doJSON(t, "GET", a.adminURL()+"/api/v1/health", nil, "")
	if code != 200 || !strings.Contains(string(b), `"status": "ok"`) {
		t.Fatalf("health: %d %s", code, b)
	}
}

func TestAdminTrigger(t *testing.T) {
	rc := newReceiver(t)
	a := startApp(t, writeProject(t, projectFiles(rc.srv.URL)))

	code, b := doJSON(t, "POST", a.adminURL()+"/api/v1/trigger", nil,
		`{"target": "merchant", "body": {"manual": true}, "sign": true}`)
	if code != 202 {
		t.Fatalf("trigger: %d %s", code, b)
	}
	h := rc.wait(t)
	var triggered map[string]any
	if err := json.Unmarshal(h.Body, &triggered); err != nil || triggered["manual"] != true {
		t.Errorf("trigger body = %s", h.Body)
	}
	verifySignature(t, h)

	// sign: false must omit the signature.
	code, _ = doJSON(t, "POST", a.adminURL()+"/api/v1/trigger", nil,
		`{"target": "merchant", "body": {"manual": 2}, "sign": false}`)
	if code != 202 {
		t.Fatalf("trigger unsigned: %d", code)
	}
	h = rc.wait(t)
	if h.Headers.Get("X-TwinStub-Signature") != "" {
		t.Error("signature present despite sign: false")
	}

	code, _ = doJSON(t, "POST", a.adminURL()+"/api/v1/trigger", nil, `{"target": "ghost", "body": {}}`)
	if code != 422 {
		t.Errorf("unknown target: %d", code)
	}
}

// Hot reload must not break active sessions: they finish on the pinned
// config version while new sessions see the new one (spec 4.3).
func TestHotReloadKeepsSessions(t *testing.T) {
	rc := newReceiver(t)
	files := projectFiles(rc.srv.URL)
	configPath := writeProject(t, files)
	a := startApp(t, configPath)

	doJSON(t, "POST", a.publicURL()+"/v1/payments",
		map[string]string{"X-TwinStub-Scenario": "payment.chargeback"},
		`{"order_id": "ord_old", "amount": 1}`)

	// Change the scenario sync reply and the endpoint, then reload.
	dir := filepath.Dir(configPath)
	newScenario := strings.ReplaceAll(files["scenarios/payment.yaml"], `"rev": "v1"`, `"rev": "v2"`)
	newScenario = strings.ReplaceAll(newScenario, `'{"status": "processing"}'`, `'{"status": "processing", "rev": "v2"}'`)
	if err := os.WriteFile(filepath.Join(dir, "scenarios/payment.yaml"), []byte(newScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	code, b := doJSON(t, "POST", a.adminURL()+"/api/v1/reload", nil, "")
	if code != 200 {
		t.Fatalf("reload: %d %s", code, b)
	}

	// Old session still runs the v1 definition.
	code, b = doJSON(t, "GET", a.publicURL()+"/v1/payments/pay_x", nil, `{"order_id": "ord_old"}`)
	if code != 200 || strings.Contains(string(b), "v2") {
		t.Errorf("old session leaked new config: %d %s", code, b)
	}

	// A new session uses the new definition.
	code, b = doJSON(t, "POST", a.publicURL()+"/v1/payments",
		map[string]string{"X-TwinStub-Scenario": "payment.chargeback"},
		`{"order_id": "ord_new", "amount": 2}`)
	if code != 201 || !strings.Contains(string(b), `"rev": "v2"`) {
		t.Errorf("new session on old config: %d %s", code, b)
	}
}

func TestReloadRejectsInvalidConfig(t *testing.T) {
	rc := newReceiver(t)
	files := projectFiles(rc.srv.URL)
	configPath := writeProject(t, files)
	a := startApp(t, configPath)

	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "endpoints/ping.yaml"),
		[]byte("version: 1\nendpoints:\n  - id: broken\n    when: {method: GET}\n    reply: {status: 200}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, b := doJSON(t, "POST", a.adminURL()+"/api/v1/reload", nil, "")
	if code != 422 || !strings.Contains(string(b), "path is required") {
		t.Fatalf("reload of broken config: %d %s", code, b)
	}
	// Server keeps serving the old config.
	code, _ = doJSON(t, "GET", a.publicURL()+"/ping", nil, "")
	if code != 200 {
		t.Errorf("old config not serving after failed reload: %d", code)
	}
}

func TestFsnotifyReload(t *testing.T) {
	rc := newReceiver(t)
	files := projectFiles(rc.srv.URL)
	configPath := writeProject(t, files)
	a := startApp(t, configPath)

	updated := strings.Replace(files["endpoints/ping.yaml"], `"rev": "v1"`, `"rev": "v2"`, 1)
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), "endpoints/ping.yaml"), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doJSON(t, "GET", a.publicURL()+"/ping", nil, "")
		if strings.Contains(string(b), `"rev": "v2"`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("fsnotify reload never picked up the change")
}

func TestAdminAuth(t *testing.T) {
	rc := newReceiver(t)
	files := projectFiles(rc.srv.URL)
	files["twinstub.yaml"] = strings.Replace(files["twinstub.yaml"],
		"admin:\n    enabled: true\n    port: 9090",
		"admin:\n    enabled: true\n    port: 9090\n    token: sekret", 1)
	a := startApp(t, writeProject(t, files))

	code, _ := doJSON(t, "GET", a.adminURL()+"/api/v1/health", nil, "")
	if code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
	code, _ = doJSON(t, "GET", a.adminURL()+"/api/v1/health",
		map[string]string{"Authorization": "Bearer sekret"}, "")
	if code != 200 {
		t.Errorf("with token: %d, want 200", code)
	}
}

func TestSessionTTLExpiry(t *testing.T) {
	rc := newReceiver(t)
	files := projectFiles(rc.srv.URL)
	files["scenarios/payment.yaml"] = strings.Replace(files["scenarios/payment.yaml"], "ttl: 30m", "ttl: 150ms", 1)
	a := startApp(t, writeProject(t, files))

	doJSON(t, "POST", a.publicURL()+"/v1/payments",
		map[string]string{"X-TwinStub-Scenario": "payment.chargeback"},
		`{"order_id": "ord_ttl", "amount": 1}`)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doJSON(t, "GET", a.adminURL()+"/api/v1/sessions", nil, "")
		if strings.Contains(string(b), `"count": 0`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("session never expired")
}

func TestGracefulShutdownUnder6s(t *testing.T) {
	rc := newReceiver(t)
	a := startApp(t, writeProject(t, projectFiles(rc.srv.URL)))

	// Queue a webhook far in the future, then shut down.
	doJSON(t, "POST", a.publicURL()+"/v1/payments",
		map[string]string{"X-TwinStub-Scenario": "payment.chargeback"},
		`{"order_id": "ord_shut", "amount": 1}`)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("shutdown took %v, spec demands <= 6s", elapsed)
	}
}
