package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeProject lays out a config tree in a temp dir and returns the root
// config path.
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

const validRoot = `
version: 1
server:
  port: 8080
  admin:
    enabled: true
    port: 9090
defaults:
  latency: 0ms
  scenario_header: X-TwinStub-Scenario
targets:
  cb:
    url: http://localhost:9999/hook
    signing:
      algorithm: hmac-sha256
      secret: s3cret
include:
  - endpoints/*.yaml
  - scenarios/*.yaml
`

const validEndpoints = `
version: 1
endpoints:
  - id: get_rates
    when:
      method: GET
      path: /v1/rates
      query: {currency: "USD"}
      headers: {X-Api-Key: "re:^key_.+"}
    reply:
      status: 200
      latency: 10ms
      body: '{"rate": 1.0}'
  - id: big_payment
    when:
      method: POST
      path: /v1/payments
      body:
        - path: $.amount
          gt: 100000
        - path: $.currency
          equals: "EUR"
    reply: {status: 201, body: '{}'}
`

const validScenario = `
version: 1
scenarios:
  - id: pay.flow
    description: "test"
    session:
      key: "body:$.order_id"
      ttl: 30m
    initial: new
    states:
      new:
        on:
          - when: {method: POST, path: /v1/payments}
            reply: {status: 201, body: '{}'}
            set: {amount: "{{ .Request.Body.amount }}"}
            goto: done
      done:
        enter:
          - webhook:
              target: cb
              after: 3s
              body: '{"ok": true}'
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{}'}
`

func validProject() map[string]string {
	return map[string]string{
		"twinstub.yaml":          validRoot,
		"endpoints/main.yaml":    validEndpoints,
		"scenarios/payment.yaml": validScenario,
	}
}

func TestLoadValid(t *testing.T) {
	cfg, warns, err := Load(writeProject(t, validProject()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if len(cfg.Endpoints) != 2 || len(cfg.Scenarios) != 1 {
		t.Fatalf("endpoints=%d scenarios=%d", len(cfg.Endpoints), len(cfg.Scenarios))
	}
	if cfg.Endpoints[0].ID != "get_rates" {
		t.Errorf("include order broken: first endpoint %s", cfg.Endpoints[0].ID)
	}
	// Defaults must be applied.
	tg := cfg.Targets["cb"]
	if time.Duration(tg.Timeout) != 10*time.Second || tg.Retry.Attempts != 3 || time.Duration(tg.Retry.Backoff) != 2*time.Second {
		t.Errorf("target defaults: %+v", tg)
	}
	if tg.Signing.Header != "X-TwinStub-Signature" || tg.Signing.Format != "t={timestamp},v1={signature}" {
		t.Errorf("signing defaults: %+v", tg.Signing)
	}
	if cfg.Limits.MaxSessions != 10000 || cfg.Limits.WebhookWorkers != 8 || cfg.Limits.DeliveryLogSize != 1000 {
		t.Errorf("limit defaults: %+v", cfg.Limits)
	}
	if cfg.Scenarios[0].Session.TTLOrDefault() != 30*time.Minute {
		t.Errorf("ttl: %v", cfg.Scenarios[0].Session.TTLOrDefault())
	}
	if cfg.Endpoints[0].Source.File == "" {
		t.Error("endpoint source file not recorded")
	}
}

// Every invalid fixture asserts a substring of the message: error text
// quality is part of the product contract (spec 9).
func TestLoadInvalid(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr []string
	}{
		{
			name:    "missing version",
			mutate:  func(f map[string]string) { f["twinstub.yaml"] = strings.Replace(validRoot, "version: 1", "", 1) },
			wantErr: []string{"version: 1", "required"},
		},
		{
			name:    "unknown field strict decoding",
			mutate:  func(f map[string]string) { f["twinstub.yaml"] = validRoot + "\nunknown_thing: 1\n" },
			wantErr: []string{"unknown_thing"},
		},
		{
			name: "unknown field in endpoint",
			mutate: func(f map[string]string) {
				f["endpoints/main.yaml"] = strings.Replace(validEndpoints, "latency: 10ms", "latencyy: 10ms", 1)
			},
			wantErr: []string{"latencyy"},
		},
		{
			name: "missing env var",
			mutate: func(f map[string]string) {
				f["twinstub.yaml"] = strings.Replace(validRoot, "secret: s3cret", "secret: ${TWINSTUB_TEST_MISSING_VAR}", 1)
			},
			wantErr: []string{"TWINSTUB_TEST_MISSING_VAR", "not set"},
		},
		{
			name: "duplicate endpoint id",
			mutate: func(f map[string]string) {
				f["endpoints/zz.yaml"] = "version: 1\nendpoints:\n  - id: get_rates\n    when: {method: GET, path: /dup}\n    reply: {status: 200}\n"
			},
			wantErr: []string{"duplicate endpoint id", "get_rates"},
		},
		{
			name: "goto unknown state",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario, "goto: done", "goto: nowhere", 1)
			},
			wantErr: []string{"goto", "nowhere", "known:"},
		},
		{
			name: "initial unknown state",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario, "initial: new", "initial: missing", 1)
			},
			wantErr: []string{"initial state", "missing"},
		},
		{
			name: "bad session selector",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario, "body:$.order_id", "cookie:session", 1)
			},
			wantErr: []string{"selector", "cookie:session"},
		},
		{
			name: "ttl too long",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario, "ttl: 30m", "ttl: 48h", 1)
			},
			wantErr: []string{"ttl", "24h"},
		},
		{
			name: "after exceeds 7d",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario, "after: 3s", "after: 8d", 1)
			},
			wantErr: []string{"after", "7d"},
		},
		{
			name: "unknown webhook target",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario, "target: cb", "target: nope", 1)
			},
			wantErr: []string{"unknown target", "nope"},
		},
		{
			name: "two body operators",
			mutate: func(f map[string]string) {
				f["endpoints/main.yaml"] = strings.Replace(validEndpoints, "gt: 100000", "gt: 100000\n          lt: 5", 1)
			},
			wantErr: []string{"exactly one operator"},
		},
		{
			name: "bad signing algorithm",
			mutate: func(f map[string]string) {
				f["twinstub.yaml"] = strings.Replace(validRoot, "hmac-sha256", "md5", 1)
			},
			wantErr: []string{"hmac-sha256"},
		},
		{
			name: "missing reply in rule",
			mutate: func(f map[string]string) {
				f["scenarios/payment.yaml"] = strings.Replace(validScenario,
					"            reply: {status: 200, body: '{}'}\n", "", 1)
			},
			wantErr: []string{"reply is required"},
		},
		{
			name: "bad duration",
			mutate: func(f map[string]string) {
				f["endpoints/main.yaml"] = strings.Replace(validEndpoints, "latency: 10ms", "latency: fast", 1)
			},
			wantErr: []string{"invalid duration"},
		},
		{
			name: "path without slash",
			mutate: func(f map[string]string) {
				f["endpoints/main.yaml"] = strings.Replace(validEndpoints, "path: /v1/rates", "path: v1/rates", 1)
			},
			wantErr: []string{"must start with /"},
		},
		{
			name: "limit above hard max",
			mutate: func(f map[string]string) {
				f["twinstub.yaml"] = validRoot + "\nlimits:\n  max_pending_webhooks_per_session: 5000\n"
			},
			wantErr: []string{"hard maximum", "1000"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := validProject()
			c.mutate(files)
			_, _, err := Load(writeProject(t, files))
			if err == nil {
				t.Fatal("expected validation error")
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err.Error(), want)
				}
			}
		})
	}
}

func TestEnvSubstitution(t *testing.T) {
	t.Setenv("TWINSTUB_TEST_SECRET", "from_env")
	files := validProject()
	files["twinstub.yaml"] = strings.Replace(validRoot, "secret: s3cret", "secret: ${TWINSTUB_TEST_SECRET}", 1)
	cfg, _, err := Load(writeProject(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Targets["cb"].Signing.Secret; got != "from_env" {
		t.Errorf("secret = %q", got)
	}
}

func TestEnvDefault(t *testing.T) {
	files := validProject()
	files["twinstub.yaml"] = strings.Replace(validRoot, "secret: s3cret", "secret: ${TWINSTUB_TEST_UNSET:-fallback}", 1)
	cfg, _, err := Load(writeProject(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Targets["cb"].Signing.Secret; got != "fallback" {
		t.Errorf("secret = %q", got)
	}
}

func TestUnreachableStateWarning(t *testing.T) {
	files := validProject()
	files["scenarios/payment.yaml"] = validScenario + `
      orphan:
        on:
          - when: {method: GET, path: /orphan}
            reply: {status: 200}
`
	_, warns, err := Load(writeProject(t, files))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w.Message, "unreachable") && strings.Contains(w.Message, "orphan") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unreachable warning, got %v", warns)
	}
}

func TestErrorsIncludeFileAndPath(t *testing.T) {
	files := validProject()
	files["scenarios/payment.yaml"] = strings.Replace(validScenario, "goto: done", "goto: nowhere", 1)
	_, _, err := Load(writeProject(t, files))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "payment.yaml") {
		t.Errorf("error lacks file name: %q", msg)
	}
	if !strings.Contains(msg, "scenarios[0].states.new.on[0].goto") {
		t.Errorf("error lacks field path: %q", msg)
	}
}

func TestIncludeOrderIsLexical(t *testing.T) {
	files := validProject()
	files["endpoints/a_first.yaml"] = "version: 1\nendpoints:\n  - id: a_first\n    when: {method: GET, path: /v1/rates}\n    reply: {status: 418}\n"
	cfg, _, err := Load(writeProject(t, files))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoints[0].ID != "a_first" {
		t.Errorf("lexical include order broken: %s", cfg.Endpoints[0].ID)
	}
}
