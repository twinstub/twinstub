package mcp

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFullCycle drives a real embedded engine through the tools in the
// order an agent would: start, trigger, deliver, advance_time,
// list_deliveries, replay redeliver, reset.
func TestFullCycle(t *testing.T) {
	rc, hits := receiver(t)
	s := newServer(t, Options{ConfigPath: writeConfig(t, rc.URL), ConfigSet: true, PublicPort: -1})
	cs := connect(t, s)

	out := callOK(t, cs, "twinstub_start_session", map[string]any{"scenario": "payment.chargeback", "key": "ord_1"})
	sid, _ := out["session_id"].(string)
	if sid == "" || out["state"] != "new" || out["created"] != true || out["virtual_now"] == nil {
		t.Fatalf("start_session = %v", out)
	}
	if again := callOK(t, cs, "twinstub_start_session", map[string]any{"scenario": "payment.chargeback", "key": "ord_1"}); again["session_id"] != sid || again["created"] != false {
		t.Errorf("second start_session = %v, want the same session", again)
	}

	out = callOK(t, cs, "twinstub_trigger", map[string]any{"session": sid, "state": "processing"})
	if out["state"] != "processing" || !hasEvent(out["pending_webhooks"], "payment.succeeded") {
		t.Fatalf("trigger = %v", out)
	}

	out = callOK(t, cs, "twinstub_deliver", map[string]any{"session": sid, "events": []string{"payment.succeeded"}})
	if out["state"] != "succeeded" || firedResultOf(out, 0) != "delivered" {
		t.Fatalf("deliver = %v", out)
	}

	out = callOK(t, cs, "twinstub_advance_time", map[string]any{"session": sid, "duration": "30d"})
	if out["state"] != "chargeback" || firedResultOf(out, 0) != "delivered" || out["clock_offset"] == nil {
		t.Fatalf("advance_time = %v", out)
	}

	session := callOK(t, cs, "twinstub_get_session", map[string]any{"session": sid})
	if session["state"] != "chargeback" || len(session["history"].([]any)) < 4 {
		t.Errorf("get_session = %v", session)
	}

	out = callOK(t, cs, "twinstub_list_deliveries", map[string]any{"session": sid})
	list := out["deliveries"].([]any)
	if len(list) != 2 {
		t.Fatalf("list_deliveries = %v", out)
	}
	newest := list[0].(map[string]any)
	if newest["event"] != "payment.chargeback" || newest["status"] != "delivered" {
		t.Errorf("newest delivery = %v", newest)
	}
	if !strings.Contains(newest["request_body"].(string), `"id": "evt_cb_ord_1"`) {
		t.Errorf("chargeback body = %v", newest["request_body"])
	}
	first := list[1].(map[string]any)["id"].(string)

	out = callOK(t, cs, "twinstub_replay_delivery", map[string]any{"delivery": first, "mode": "redeliver"})
	d, _ := out["delivery"].(map[string]any)
	if out["mode"] != "redeliver" || out["replayed_from"] != first || d["status"] != "delivered" || d["mode"] != "redeliver" {
		t.Fatalf("replay = %v", out)
	}
	if last := d["last_attempt"].(map[string]any); last["status_code"] != float64(200) || last["signature"] != "valid" {
		t.Errorf("replay attempt = %v", last)
	}
	if n := hits(); n != 3 {
		t.Errorf("receiver got %d webhooks, want 3", n)
	}

	out = callOK(t, cs, "twinstub_reset", nil)
	if out["sessions_deleted"] != float64(1) || out["deliveries_cleared"] != float64(3) {
		t.Errorf("reset = %v", out)
	}
	if text, isErr := call(t, cs, "twinstub_get_session", map[string]any{"session": sid}); !isErr || !strings.Contains(text, "not found") {
		t.Errorf("session after reset: %v %q", isErr, text)
	}
}

func TestExplainMissEmbedded(t *testing.T) {
	rc, _ := receiver(t)
	s := newServer(t, Options{ConfigPath: writeConfig(t, rc.URL), ConfigSet: true, PublicPort: -1})
	cs := connect(t, s)

	out := callOK(t, cs, "twinstub_explain_miss", nil)
	if out["count"] != float64(0) || out["note"] == nil {
		t.Errorf("no misses yet: %v", out)
	}
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/orders/ord_1/refunds", s.app.PublicPort()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	out = callOK(t, cs, "twinstub_explain_miss", map[string]any{"limit": 5})
	if out["count"] != float64(1) || !strings.Contains(fmt.Sprint(out["misses"]), "/v1/orders/ord_1/refunds") {
		t.Errorf("explain_miss = %v", out)
	}
}

func TestLoadConfigEmbedded(t *testing.T) {
	rc, _ := receiver(t)
	s := resolved(t, newServer(t, Options{ConfigPath: writeConfig(t, rc.URL), ConfigSet: true, PublicPort: -1}))
	cs := connect(t, s)
	port := s.app.PublicPort()
	callOK(t, cs, "twinstub_start_session", map[string]any{"scenario": "payment.chargeback", "key": "ord_1"})

	next := writeConfig(t, rc.URL)
	data, _ := os.ReadFile(next)
	if err := os.WriteFile(next, []byte(strings.Replace(string(data), "id: payment.chargeback", "id: payment.refund", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out := callOK(t, cs, "twinstub_load_config", map[string]any{"path": next})
	if out["loaded"] != true || out["sessions_reset"] != true || out["public_port"] != float64(port) || fmt.Sprint(out["scenarios"]) != "[payment.refund]" {
		t.Fatalf("load_config = %v (public port was %d)", out, port)
	}
	status := callOK(t, cs, "twinstub_status", nil)
	if status["config_path"] != next || status["sessions"] != float64(0) {
		t.Errorf("status after load = %v", status)
	}

	bad := writeConfig(t, rc.URL)
	if err := os.WriteFile(bad, []byte("version: 1\nscenarios:\n  - id: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, isErr := call(t, cs, "twinstub_load_config", map[string]any{"path": bad})
	if !isErr || !strings.Contains(text, "was not loaded") {
		t.Errorf("invalid config: %v %q", isErr, text)
	}
	// The engine keeps the last good config.
	if status := callOK(t, cs, "twinstub_status", nil); status["config_path"] != next {
		t.Errorf("config after a rejected load = %v", status["config_path"])
	}
}

func hasEvent(v any, event string) bool {
	list, _ := v.([]any)
	for _, x := range list {
		if m, ok := x.(map[string]any); ok && m["event"] == event {
			return true
		}
	}
	return false
}

func firedResultOf(out map[string]any, i int) any {
	list, _ := out["fired"].([]any)
	if i >= len(list) {
		return nil
	}
	m, _ := list[i].(map[string]any)
	return m["result"]
}

// TestEmptyEngineKeepsOffDefaultPort covers a plugin installed for every
// project: a session without a config must not hold 8080, and the first
// config it loads gets its own port.
func TestEmptyEngineKeepsOffDefaultPort(t *testing.T) {
	s := resolved(t, newServer(t, Options{ConfigPath: filepath.Join(t.TempDir(), "twinstub.yaml")}))
	cs := connect(t, s)
	if p := s.app.PublicPort(); p == 8080 {
		t.Fatalf("an engine without a config took port %d", p)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	rc, _ := receiver(t)
	path := writeConfig(t, rc.URL)
	data, _ := os.ReadFile(path)
	cfg := strings.Replace(string(data), "version: 1\n", fmt.Sprintf("version: 1\nserver:\n  port: %d\n", want), 1)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out := callOK(t, cs, "twinstub_load_config", map[string]any{"path": path})
	if out["public_port"] != float64(want) {
		t.Errorf("load_config = %v, want public_port %d", out, want)
	}
}
