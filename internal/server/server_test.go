package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/engine"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
	"github.com/twinstub/twinstub/internal/webhook"
)

const serverConfig = `
version: 1
defaults:
  latency: 0ms
endpoints:
  - id: get_rates
    when:
      method: GET
      path: /v1/rates
      query: {currency: "USD"}
    reply:
      status: 200
      headers: {Content-Type: application/json, X-Rate-Source: mock}
      body: '{"currency": "{{ .Request.Query.currency }}", "rate": 1.0}'
  - id: rates_fallback
    when: {method: GET, path: /v1/rates}
    reply: {status: 400, body: '{"error": "unsupported_currency"}'}
  - id: slow
    when: {method: GET, path: /v1/slow}
    reply: {status: 200, latency: 200ms, body: '{}'}
  - id: drop
    when: {method: GET, path: /v1/drop}
    reply: {status: 0}
  - id: echo_param
    when: {method: GET, path: "/v1/items/{item_id}"}
    reply: {status: 200, body: '{"item": "{{ .Request.Params.item_id }}"}'}
  - id: bad_render
    when: {method: GET, path: /v1/broken}
    reply: {status: 200, body: '{{ .Session.ID }}'}
`

type nullSched struct{}

func (nullSched) Schedule(webhook.ScheduleRequest) string { return "x" }
func (nullSched) CancelSession(string)                    {}
func (nullSched) PendingCount(string) int                 { return 0 }

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "twinstub.yaml")
	if err := os.WriteFile(path, []byte(serverConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	eng := tmpl.New(1, true)
	snap, err := snapshot.Compile(cfg, eng, 1)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(engine.NewStore(100), nullSched{}, 1, 100, eng.ULID, zerolog.Nop())
	srv := New(func() *snapshot.Snapshot { return snap }, e, zerolog.Nop())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestStatelessMatchOrder(t *testing.T) {
	ts := testServer(t)

	code, body, hdr := get(t, ts.URL+"/v1/rates?currency=USD")
	if code != 200 || !strings.Contains(body, `"rate": 1.0`) {
		t.Errorf("usd: %d %s", code, body)
	}
	if hdr.Get("X-Rate-Source") != "mock" {
		t.Errorf("custom header missing")
	}
	// First match wins: fallback answers for other currencies.
	code, body, _ = get(t, ts.URL+"/v1/rates?currency=GBP")
	if code != 400 || !strings.Contains(body, "unsupported_currency") {
		t.Errorf("fallback: %d %s", code, body)
	}
}

func TestPathParamTemplate(t *testing.T) {
	ts := testServer(t)
	code, body, _ := get(t, ts.URL+"/v1/items/sku_99")
	if code != 200 || !strings.Contains(body, "sku_99") {
		t.Errorf("%d %s", code, body)
	}
}

func TestLatency(t *testing.T) {
	ts := testServer(t)
	start := time.Now()
	code, _, _ := get(t, ts.URL+"/v1/slow")
	elapsed := time.Since(start)
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("latency not applied: %v", elapsed)
	}
}

// Chaos: status 0 closes the connection without an HTTP response.
func TestStatusZeroDropsConnection(t *testing.T) {
	ts := testServer(t)
	_, err := http.Get(ts.URL + "/v1/drop")
	if err == nil {
		t.Fatal("expected a transport error, got a response")
	}
	if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("unexpected error type: %v", err)
	}
}

func TestTemplateError500(t *testing.T) {
	ts := testServer(t)
	code, body, _ := get(t, ts.URL+"/v1/broken")
	if code != 500 {
		t.Fatalf("code = %d, body = %s", code, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("500 body is not JSON: %s", body)
	}
	if parsed["error"] != "template_error" || parsed["detail"] == "" {
		t.Errorf("template error must be exposed, got %s", body)
	}
}

func Test404Diagnostics(t *testing.T) {
	ts := testServer(t)
	resp, err := http.Post(ts.URL+"/v1/rates", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 {
		t.Fatalf("code = %d", resp.StatusCode)
	}
	var diag struct {
		Error      string `json:"error"`
		Method     string `json:"method"`
		Path       string `json:"path"`
		Candidates []struct {
			Endpoint string `json:"endpoint"`
			Reason   string `json:"reason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(b, &diag); err != nil {
		t.Fatalf("bad diagnostics json: %s", b)
	}
	if diag.Error != "no_match" || diag.Method != "POST" || diag.Path != "/v1/rates" {
		t.Errorf("diag = %+v", diag)
	}
	if len(diag.Candidates) == 0 {
		t.Fatal("candidates missing; diagnostics are a required DX feature")
	}
	found := false
	for _, c := range diag.Candidates {
		if c.Endpoint == "get_rates" && strings.Contains(c.Reason, "method") {
			found = true
		}
	}
	if !found {
		t.Errorf("candidate reasons unhelpful: %s", b)
	}
}

func TestDefaultContentType(t *testing.T) {
	ts := testServer(t)
	_, _, hdr := get(t, ts.URL+"/v1/rates?currency=GBP")
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("json body should default to application/json, got %q", ct)
	}
}

func TestBodyTooLarge(t *testing.T) {
	ts := testServer(t)
	big := strings.NewReader(strings.Repeat("x", config.HardMaxBodySize+1))
	resp, err := http.Post(ts.URL+"/v1/rates", "text/plain", big)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 413 {
		t.Errorf("code = %d, want 413", resp.StatusCode)
	}
}

// Throughput guard for the >= 1000 rps NFR. Run with: make bench
func BenchmarkStatelessReply(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "twinstub.yaml")
	if err := os.WriteFile(path, []byte(serverConfig), 0o644); err != nil {
		b.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		b.Fatal(err)
	}
	eng := tmpl.New(1, true)
	snap, err := snapshot.Compile(cfg, eng, 1)
	if err != nil {
		b.Fatal(err)
	}
	e := engine.New(engine.NewStore(100), nullSched{}, 1, 100, eng.ULID, zerolog.Nop())
	srv := New(func() *snapshot.Snapshot { return snap }, e, zerolog.Nop())
	h := srv.Handler()

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "/v1/rates?currency=USD", nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				b.Fatalf("code = %d", w.Code)
			}
		}
	})
}
