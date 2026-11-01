package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

// connect runs s on an in-memory transport and returns a connected client.
func connect(t *testing.T, s *Server) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = s.Run(ctx, st) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		_ = s.Close(context.Background())
	})
	return cs
}

// call invokes a tool and returns its text and whether it was an error.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks, want 1", name, len(res.Content))
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("%s: content is %T", name, res.Content[0])
	}
	return text.Text, res.IsError
}

// callOK invokes a tool that must succeed and decodes its JSON answer.
func callOK(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := call(t, cs, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: answer is not a JSON object: %s", name, text)
	}
	return out
}

// closedURL is an http URL nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

// fakeAdmin serves canned admin API answers keyed by "METHOD /path".
func fakeAdmin(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			_, _ = fmt.Fprintf(w, `{"error": "no_route", "detail": "fake admin has no %s %s"}`, r.Method, r.URL.Path)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func healthReply(version string) func(http.ResponseWriter, *http.Request) {
	return reply(200, fmt.Sprintf(`{"status": "ok", "version": %q, "config_path": "/srv/twinstub.yaml", "public_port": 8080, "time_scale": 1, "sessions": 0}`, version))
}

func newServer(t *testing.T, opts Options) *Server {
	t.Helper()
	opts.Logger = zerolog.Nop()
	if opts.ProbeURL == "" {
		opts.ProbeURL = closedURL(t)
	}
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	s, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// resolved makes the server choose its engine, as the first tool call that
// needs one would.
func resolved(t *testing.T, s *Server) *Server {
	t.Helper()
	if err := s.resolve(context.Background()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return s
}

// writeConfig writes a project whose webhooks go to targetURL and returns
// the root config path.
func writeConfig(t *testing.T, targetURL string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`version: 1
targets:
  merchant:
    url: %s
    signing:
      algorithm: hmac-sha256
      secret: whsec_test
      header: X-TwinStub-Signature
      format: "t={timestamp},v1={signature}"
    retry:
      attempts: 1
scenarios:
  - id: payment.chargeback
    session:
      key: "path:{order_id}"
    initial: new
    states:
      new:
        on:
          - when: {method: POST, path: "/v1/orders/{order_id}/payment"}
            reply: {status: 202, body: '{"status": "processing"}'}
            goto: processing
      processing:
        enter:
          - webhook:
              target: merchant
              event: payment.succeeded
              after: 3s
              body: '{"id": "evt_ok_{{ .Session.Key }}", "type": "payment.succeeded", "created": "{{ .Webhook.At }}"}'
            goto: succeeded
      succeeded:
        enter:
          - webhook:
              target: merchant
              event: payment.chargeback
              after: 30d
              body: '{"id": "evt_cb_{{ .Session.Key }}", "type": "payment.chargeback", "created": "{{ .Webhook.At }}"}'
            goto: chargeback
      chargeback: {}
`, targetURL)
	p := filepath.Join(dir, "twinstub.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// receiver accepts webhooks and counts them.
func receiver(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	hits := make(chan struct{}, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { return len(hits) }
}
