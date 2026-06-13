package snapshot

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/match"
	"github.com/twinstub/twinstub/internal/tmpl"
)

func load(t *testing.T, yaml string) (*config.Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "twinstub.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	return cfg, err
}

func TestCompileReportsTemplateErrors(t *testing.T) {
	cfg, err := load(t, `
version: 1
endpoints:
  - id: bad_tmpl
    when: {method: GET, path: /x}
    reply: {status: 200, body: '{{ .Broken'}
`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compile(cfg, tmpl.New(0, false), 1)
	if err == nil {
		t.Fatal("expected compile error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "endpoints[0].reply") || !strings.Contains(msg, "twinstub.yaml") {
		t.Errorf("error lacks location: %q", msg)
	}
}

func TestCompileReportsBadRegex(t *testing.T) {
	cfg, err := load(t, `
version: 1
endpoints:
  - id: bad_re
    when:
      method: GET
      path: /x
      headers: {X-K: "re:["}
    reply: {status: 200}
`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Compile(cfg, tmpl.New(0, false), 1); err == nil {
		t.Fatal("expected bad regex error")
	}
}

func TestKeySelectorParse(t *testing.T) {
	good := map[string][2]string{
		"body:$.order_id": {"body", "$.order_id"},
		"header:X-Name":   {"header", "X-Name"},
		"query:param":     {"query", "param"},
		"path:{order_id}": {"path", "order_id"},
	}
	for in, want := range good {
		sel, err := ParseKeySelector(in)
		if err != nil {
			t.Errorf("ParseKeySelector(%q): %v", in, err)
			continue
		}
		if sel.Kind != want[0] || sel.Arg != want[1] {
			t.Errorf("ParseKeySelector(%q) = %+v", in, sel)
		}
	}
	for _, in := range []string{"cookie:x", "body", "path:{}", ""} {
		if _, err := ParseKeySelector(in); err == nil {
			t.Errorf("ParseKeySelector(%q): expected error", in)
		}
	}
}

func TestPathSelectorExtract(t *testing.T) {
	cfg, err := load(t, `
version: 1
scenarios:
  - id: s
    session: {key: "path:{pid}"}
    initial: a
    states:
      a:
        on:
          - when: {method: GET, path: "/v1/payments/{pid}"}
            reply: {status: 200}
`)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := Compile(cfg, tmpl.New(0, false), 1)
	if err != nil {
		t.Fatal(err)
	}
	sc := snap.ScenarioByID["s"]
	r := &match.Request{Method: "GET", Path: "/v1/payments/pay_9", Query: url.Values{}, Headers: http.Header{}}
	key, ok := sc.KeySelector.Extract(r, sc)
	if !ok || key != "pay_9" {
		t.Errorf("extract = %q %v", key, ok)
	}
	r2 := &match.Request{Method: "GET", Path: "/other", Query: url.Values{}, Headers: http.Header{}}
	if _, ok := sc.KeySelector.Extract(r2, sc); ok {
		t.Error("extract should fail on non-matching path")
	}
}

func TestTerminalState(t *testing.T) {
	s := &State{}
	if !s.Terminal() {
		t.Error("empty state should be terminal")
	}
	s = &State{Enter: []*Action{{Goto: "next"}}}
	if s.Terminal() {
		t.Error("state with goto enter action is not terminal")
	}
}
