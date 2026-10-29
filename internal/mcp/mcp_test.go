package mcp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// wantAnnotations is the tool table of the spec (section 5). A tool missing
// here, or one whose hints drift, fails TestToolAnnotations.
var wantAnnotations = map[string]struct{ readOnly, destructive, idempotent bool }{
	"twinstub_status":          {readOnly: true, idempotent: true},
	"twinstub_list_scenarios":  {readOnly: true, idempotent: true},
	"twinstub_validate_config": {readOnly: true, idempotent: true},
	"twinstub_load_config":     {destructive: true},
	"twinstub_start_session":   {idempotent: true},
	"twinstub_get_session":     {readOnly: true, idempotent: true},
	"twinstub_trigger":         {},
	"twinstub_advance_time":    {},
	"twinstub_deliver":         {},
	"twinstub_list_deliveries": {readOnly: true, idempotent: true},
	"twinstub_replay_delivery": {},
	"twinstub_explain_miss":    {readOnly: true, idempotent: true},
	"twinstub_reset":           {destructive: true, idempotent: true},
}

func TestToolAnnotations(t *testing.T) {
	cs := connect(t, newServer(t, Options{AdminURL: closedURL(t)}))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range res.Tools {
		seen[tool.Name] = true
		want, ok := wantAnnotations[tool.Name]
		if !ok {
			t.Errorf("%s is not in the annotation table; add it with its hints", tool.Name)
			continue
		}
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if a.Title == "" || tool.Title == "" || tool.Description == "" {
			t.Errorf("%s: title and description are required", tool.Name)
		}
		if a.ReadOnlyHint != want.readOnly {
			t.Errorf("%s: readOnlyHint = %v, want %v", tool.Name, a.ReadOnlyHint, want.readOnly)
		}
		// The SDK treats an unset destructiveHint and openWorldHint as true,
		// so both must be sent explicitly.
		if a.DestructiveHint == nil || *a.DestructiveHint != want.destructive {
			t.Errorf("%s: destructiveHint = %v, want explicit %v", tool.Name, ptrString(a.DestructiveHint), want.destructive)
		}
		if a.IdempotentHint != want.idempotent {
			t.Errorf("%s: idempotentHint = %v, want %v", tool.Name, a.IdempotentHint, want.idempotent)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: openWorldHint = %v, want explicit false", tool.Name, ptrString(a.OpenWorldHint))
		}
	}
	for name := range wantAnnotations {
		if !seen[name] {
			t.Errorf("%s is in the table but not served", name)
		}
	}
}

func ptrString(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}

func TestExplicitURLNeverFallsBack(t *testing.T) {
	url := closedURL(t)
	s := newServer(t, Options{AdminURL: url})
	if s.mode != ModeExternal || s.app != nil {
		t.Fatalf("mode = %s, embedded engine started = %v; an explicit URL must not fall back", s.mode, s.app != nil)
	}
	cs := connect(t, s)

	text, isErr := call(t, cs, "twinstub_status", nil)
	if !isErr || !strings.Contains(text, "Engine not reachable at "+url) || !strings.Contains(text, "--admin-url") || !strings.Contains(text, "twinstub serve") {
		t.Errorf("status: %v %q", isErr, text)
	}

	// Every tool that needs the engine fails with the same hint instead of
	// crashing. validate_config works offline and is left out.
	cfg := writeConfig(t, "http://127.0.0.1:1")
	args := map[string]map[string]any{
		"twinstub_list_scenarios":  nil,
		"twinstub_load_config":     {"path": cfg},
		"twinstub_start_session":   {"scenario": "payment.chargeback", "key": "k"},
		"twinstub_get_session":     {"session": "s"},
		"twinstub_trigger":         {"session": "s", "state": "processing"},
		"twinstub_advance_time":    {"session": "s", "duration": "1h"},
		"twinstub_deliver":         {"session": "s", "events": []string{"payment.succeeded"}},
		"twinstub_list_deliveries": nil,
		"twinstub_replay_delivery": {"delivery": "d"},
		"twinstub_explain_miss":    nil,
		"twinstub_reset":           nil,
	}
	for name, a := range args {
		text, isErr := call(t, cs, name, a)
		if !isErr || !strings.Contains(text, "Engine not reachable") {
			t.Errorf("%s: %v %q", name, isErr, text)
		}
	}
	if len(args)+2 != len(wantAnnotations) {
		t.Errorf("the offline check covers %d tools; update it when tools change", len(args)+2)
	}
}

func TestUsesRunningEngineWhenNoURL(t *testing.T) {
	admin := fakeAdmin(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/health": healthReply("1.1.0"),
	})
	s := newServer(t, Options{ProbeURL: admin.URL})
	if s.mode != ModeExternal || s.app != nil {
		t.Fatalf("mode = %s, want external without an embedded engine", s.mode)
	}
	out := callOK(t, connect(t, s), "twinstub_status", nil)
	if out["mode"] != ModeExternal || out["admin_url"] != admin.URL || out["public_port"] != float64(8080) || out["config_path"] != "/srv/twinstub.yaml" {
		t.Errorf("status = %v", out)
	}
}

func TestProbeIgnoresOtherServices(t *testing.T) {
	other := fakeAdmin(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/health": reply(200, `{"status": "ok"}`),
	})
	s := newServer(t, Options{ProbeURL: other.URL, PublicPort: -1})
	if s.mode != ModeEmbedded {
		t.Errorf("mode = %s; a service without a TwinStub version is not an engine", s.mode)
	}
	_ = s.Close(context.Background())
}

func TestEmbeddedWhenNothingAnswers(t *testing.T) {
	rc, _ := receiver(t)
	cfg := writeConfig(t, rc.URL)
	s := newServer(t, Options{ConfigPath: cfg, ConfigSet: true, PublicPort: -1})
	if s.mode != ModeEmbedded || s.app == nil {
		t.Fatalf("mode = %s, want embedded", s.mode)
	}
	out := callOK(t, connect(t, s), "twinstub_status", nil)
	port, _ := out["public_port"].(float64)
	if out["mode"] != ModeEmbedded || out["config_path"] != cfg || port <= 0 || !strings.HasPrefix(out["admin_url"].(string), "http://127.0.0.1:") {
		t.Errorf("status = %v", out)
	}
	if _, ok := out["config_error"]; ok {
		t.Errorf("unexpected config_error: %v", out["config_error"])
	}
}

func TestEmbeddedWithoutConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "twinstub.yaml")

	// The default path, absent: an empty engine and a hint, no error.
	s := newServer(t, Options{ConfigPath: missing, PublicPort: -1})
	out := callOK(t, connect(t, s), "twinstub_status", nil)
	if out["mode"] != ModeEmbedded || out["config_path"] != nil || out["hint"] == nil || out["config_error"] != nil {
		t.Errorf("status = %v", out)
	}

	// A path given on purpose that does not exist is reported.
	s = newServer(t, Options{ConfigPath: missing, ConfigSet: true, PublicPort: -1})
	out = callOK(t, connect(t, s), "twinstub_status", nil)
	if e, _ := out["config_error"].(string); !strings.Contains(e, "cannot read config") {
		t.Errorf("config_error = %v", out["config_error"])
	}
}

func TestStatusRejectsOldEngine(t *testing.T) {
	admin := fakeAdmin(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/health": healthReply("1.0.0"),
	})
	text, isErr := call(t, connect(t, newServer(t, Options{AdminURL: admin.URL})), "twinstub_status", nil)
	if !isErr || !strings.Contains(text, "runs twinstub 1.0.0") || !strings.Contains(text, MinEngineVersion) {
		t.Errorf("status: %v %q", isErr, text)
	}
}

func TestVersionOK(t *testing.T) {
	for v, want := range map[string]bool{
		"1.1.0": true, "v1.1.0": true, "1.2.0": true, "2.0.0": true, "1.10.0": true,
		"1.0.9": false, "1.0.0": false, "0.9.0": false,
		"dev": true, "1.1.0-rc1": true, "": true,
	} {
		if got := versionOK(v); got != want {
			t.Errorf("versionOK(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestErrorsCarryNextStep(t *testing.T) {
	admin := fakeAdmin(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/sessions/nope":                 reply(404, `{"error": "session_not_found", "id": "nope"}`),
		"POST /api/v1/sessions":                     reply(422, `{"error": "unknown_scenario", "detail": "scenario \"x\" is not defined in the loaded config", "known_scenarios": ["a", "b"]}`),
		"POST /api/v1/sessions/s1/deliver":          reply(422, `{"error": "not_pending", "detail": "payment.refund is not queued for this session", "pending_events": ["payment.pending"]}`),
		"POST /api/v1/deliveries/d1/replay":         reply(400, `{"error": "bad_mode", "detail": "mode must be as_is, redeliver, duplicate, bad_signature or no_signature"}`),
		"GET /api/v1/scenarios":                     reply(401, `{"error": "unauthorized", "detail": "set the Authorization: Bearer <token> header"}`),
		"POST /api/v1/sessions/s1/advance-time":     reply(409, `{"error": "session_busy", "detail": "session s1 is already advancing"}`),
		"GET /api/v1/deliveries/gone":               reply(404, `{"error": "delivery_not_found", "id": "gone"}`),
		"POST /api/v1/sessions/s1/advance":          reply(404, `{"error": "advance_failed", "detail": "state \"zzz\" not found"}`),
		"GET /api/v1/health":                        healthReply("dev"),
		"POST /api/v1/sessions/s1/unused-for-route": reply(500, `{}`),
	})
	cs := connect(t, newServer(t, Options{AdminURL: admin.URL}))
	for _, c := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"twinstub_get_session", map[string]any{"session": "nope"}, []string{"session not found: nope", "twinstub_start_session"}},
		{"twinstub_start_session", map[string]any{"scenario": "x", "key": "k"}, []string{"not defined", "Known scenarios: a, b", "twinstub_list_scenarios"}},
		{"twinstub_deliver", map[string]any{"session": "s1", "events": []string{"payment.refund"}}, []string{"not queued", "Queued events: payment.pending", "twinstub_replay_delivery"}},
		{"twinstub_replay_delivery", map[string]any{"delivery": "d1", "mode": "sideways"}, []string{"redeliver"}},
		{"twinstub_list_scenarios", nil, []string{"TWINSTUB_TOKEN"}},
		{"twinstub_advance_time", map[string]any{"session": "s1", "duration": "1h"}, []string{"already advancing", "wait for it"}},
		{"twinstub_list_deliveries", map[string]any{"delivery": "gone"}, []string{"delivery not found: gone", "twinstub_list_deliveries"}},
		{"twinstub_trigger", map[string]any{"session": "s1", "state": "zzz"}, []string{"not found", "twinstub_get_session"}},
	} {
		text, isErr := call(t, cs, c.tool, c.args)
		if !isErr {
			t.Errorf("%s: not an error: %s", c.tool, text)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: %q does not mention %q", c.tool, text, w)
			}
		}
	}
}

func TestBadArguments(t *testing.T) {
	cs := connect(t, newServer(t, Options{AdminURL: closedURL(t)}))
	// Required arguments are checked by the schema before any call.
	if text, isErr := call(t, cs, "twinstub_get_session", nil); !isErr || !strings.Contains(text, "session") {
		t.Errorf("get_session without session: %v %q", isErr, text)
	}
	if text, isErr := call(t, cs, "twinstub_validate_config", map[string]any{"path": "a.yaml", "content": "version: 1"}); !isErr || !strings.Contains(text, "not both") {
		t.Errorf("validate with both: %v %q", isErr, text)
	}
	if text, isErr := call(t, cs, "twinstub_validate_config", nil); !isErr || !strings.Contains(text, "path") {
		t.Errorf("validate with neither: %v %q", isErr, text)
	}
}

func TestListDeliveriesIsCompact(t *testing.T) {
	long := strings.Repeat("x", 5000)
	rec := `{"id": "d1", "session_id": "s1", "target": "merchant", "url": "http://app/hook", "event": "payment.succeeded",
	  "request_body": "` + long + `", "status": "delivered", "created_at": "2026-10-29T10:00:00Z", "fire_at": "2026-10-29T10:00:00Z",
	  "attempts": [{"num": 1, "at": "2026-10-29T10:00:01Z", "request_headers": {"X-A": "b"}, "status_code": 500, "duration_ms": 3, "signature": "valid"},
	               {"num": 2, "at": "2026-10-29T10:00:02Z", "request_headers": {"X-A": "b"}, "status_code": 200, "response_body": "ok", "duration_ms": 2, "signature": "valid"}]}`
	var gotQuery string
	admin := fakeAdmin(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/deliveries": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			reply(200, `{"deliveries": [`+rec+`], "count": 1, "next_cursor": "d1"}`)(w, r)
		},
		"GET /api/v1/deliveries/d1": reply(200, rec),
	})
	cs := connect(t, newServer(t, Options{AdminURL: admin.URL}))

	out := callOK(t, cs, "twinstub_list_deliveries", map[string]any{"session": "s1", "status": "delivered"})
	if !strings.Contains(gotQuery, "limit=20") || !strings.Contains(gotQuery, "session=s1") || !strings.Contains(gotQuery, "status=delivered") {
		t.Errorf("query = %s", gotQuery)
	}
	if out["next_cursor"] != "d1" {
		t.Errorf("next_cursor = %v", out["next_cursor"])
	}
	d := out["deliveries"].([]any)[0].(map[string]any)
	if body := d["request_body"].(string); len(body) != listBodyLimit || d["truncated"] != true || d["request_body_bytes"] != float64(5000) {
		t.Errorf("body len %d, truncated %v, bytes %v", len(body), d["truncated"], d["request_body_bytes"])
	}
	last := d["last_attempt"].(map[string]any)
	if d["attempts"] != float64(2) || last["status_code"] != float64(200) || last["signature"] != "valid" {
		t.Errorf("summary = %v", d)
	}
	if _, ok := last["request_headers"]; ok {
		t.Error("list view carries request headers")
	}

	full := callOK(t, cs, "twinstub_list_deliveries", map[string]any{"delivery": "d1"})
	if body := full["request_body"].(string); len(body) != 5000 {
		t.Errorf("full view body len %d, want 5000", len(body))
	}
}

func TestValidateConfig(t *testing.T) {
	cs := connect(t, newServer(t, Options{AdminURL: closedURL(t)}))

	out := callOK(t, cs, "twinstub_validate_config", map[string]any{"path": writeConfig(t, "http://127.0.0.1:1")})
	if out["valid"] != true || !strings.Contains(out["summary"].(string), "1 scenario(s)") {
		t.Errorf("valid config: %v", out)
	}

	bad := "version: 1\nscenarios:\n  - id: s\n    session: {key: path:id}\n    initial: nowhere\n    states:\n      a: {}\n"
	out = callOK(t, cs, "twinstub_validate_config", map[string]any{"content": bad})
	errs, _ := out["errors"].([]any)
	if out["valid"] != false || len(errs) == 0 || out["hint"] == nil {
		t.Fatalf("invalid config: %v", out)
	}
	first := errs[0].(map[string]any)
	if first["message"] == nil || first["file"] != nil {
		t.Errorf("error entry = %v; content errors carry no temp file name", first)
	}

	p := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(p, []byte("version: 1\nscenarios:\n  - id: s\n    initial: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = callOK(t, cs, "twinstub_validate_config", map[string]any{"path": p})
	if out["valid"] != false {
		t.Errorf("broken YAML: %v", out)
	}
}

func TestLoadConfigOnExternalEngine(t *testing.T) {
	cfg := writeConfig(t, "http://127.0.0.1:1")
	reloaded := false
	admin := fakeAdmin(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/health": reply(200, `{"version": "dev", "config_path": "`+cfg+`"}`),
		"POST /api/v1/reload": func(w http.ResponseWriter, r *http.Request) {
			reloaded = true
			reply(200, `{"reloaded": true}`)(w, r)
		},
		"GET /api/v1/scenarios": reply(200, `{"scenarios": [{"id": "payment.chargeback"}], "endpoints": [], "targets": []}`),
	})
	cs := connect(t, newServer(t, Options{AdminURL: admin.URL}))

	out := callOK(t, cs, "twinstub_load_config", map[string]any{"path": cfg})
	if !reloaded || out["loaded"] != true || out["sessions_reset"] != nil {
		t.Errorf("same file: reloaded %v, %v", reloaded, out)
	}

	other := writeConfig(t, "http://127.0.0.1:1")
	text, isErr := call(t, cs, "twinstub_load_config", map[string]any{"path": other})
	if !isErr || !strings.Contains(text, "cannot switch") || !strings.Contains(text, "twinstub serve -c") {
		t.Errorf("other file: %v %q", isErr, text)
	}
}

func TestToolsListMatchesSDKDefaults(t *testing.T) {
	// Guard against a tool registered without going through toolDef: the
	// SDK would leave destructiveHint and openWorldHint unset.
	s := newServer(t, Options{AdminURL: closedURL(t)})
	cs := connect(t, s)
	res, err := cs.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if !strings.HasPrefix(tool.Name, "twinstub_") {
			t.Errorf("%s has no twinstub_ prefix", tool.Name)
		}
	}
}

func TestInputSchemasArePortable(t *testing.T) {
	cs := connect(t, newServer(t, Options{AdminURL: closedURL(t)}))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		props, _ := tool.InputSchema.(map[string]any)["properties"].(map[string]any)
		for name, p := range props {
			if _, ok := p.(map[string]any)["type"].(string); !ok {
				t.Errorf("%s.%s: type is %v, want a single type", tool.Name, name, p.(map[string]any)["type"])
			}
		}
	}
	text, isErr := call(t, cs, "twinstub_replay_delivery", map[string]any{"delivery": "d1", "mode": "sideways"})
	if !isErr || !strings.Contains(text, "redeliver") {
		t.Errorf("bad mode: %v %q", isErr, text)
	}
}
