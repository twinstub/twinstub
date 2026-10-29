package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/twinstub/twinstub/internal/version"
)

// Size limits that keep answers small enough for an agent's context.
const (
	defaultListLimit = 20
	maxListLimit     = 100
	listBodyLimit    = 500   // request and response bodies in list views
	fullBodyLimit    = 16384 // bodies of a single delivery
	historyLimit     = 20
)

// toolDef is one tool with its annotations. The annotation test keeps its
// own copy of these values, so a change here has to be made on purpose.
type toolDef struct {
	name        string
	title       string
	description string
	readOnly    bool
	destructive bool
	idempotent  bool
}

func (d toolDef) tool() *sdk.Tool {
	f := false
	destructive := d.destructive
	return &sdk.Tool{
		Name:        d.name,
		Title:       d.title,
		Description: d.description,
		Annotations: &sdk.ToolAnnotations{
			Title:           d.title,
			ReadOnlyHint:    d.readOnly,
			DestructiveHint: &destructive,
			IdempotentHint:  d.idempotent,
			OpenWorldHint:   &f,
		},
	}
}

// add registers a tool whose handler returns a value to send back as
// compact JSON, or an error that already says what to do next.
func add[In any](s *Server, d toolDef, h func(context.Context, In) (any, error)) {
	sdk.AddTool(s.sdk, d.tool(), func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		out, err := h(ctx, in)
		if err != nil {
			return nil, nil, s.explain(err)
		}
		data, err := json.Marshal(out)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}}, nil, nil
	})
}

type (
	noInput      struct{}
	scenarioArgs struct {
		Name string `json:"name,omitempty" jsonschema:"scenario id; omit to list all"`
	}
	validateArgs struct {
		Path    string `json:"path,omitempty" jsonschema:"path to a root config file; includes are resolved next to it"`
		Content string `json:"content,omitempty" jsonschema:"YAML of a self-contained root config (version: 1, no includes); use instead of path"`
	}
	loadArgs struct {
		Path string `json:"path" jsonschema:"path to the root config file"`
	}
	startArgs struct {
		Scenario     string `json:"scenario" jsonschema:"scenario id from twinstub_list_scenarios"`
		Key          string `json:"key" jsonschema:"session key: the value the app will send in the field the scenario's session_key selects"`
		InitialState string `json:"initial_state,omitempty" jsonschema:"start in this state instead of the scenario's initial one"`
	}
	sessionArgs struct {
		Session string `json:"session" jsonschema:"session id"`
	}
	triggerArgs struct {
		Session string `json:"session" jsonschema:"session id"`
		State   string `json:"state" jsonschema:"state to move the session to; its webhooks are scheduled as on a normal transition"`
	}
	advanceArgs struct {
		Session  string `json:"session" jsonschema:"session id"`
		Duration string `json:"duration" jsonschema:"how far to move the session clock, like 30s, 2h or 30d"`
	}
	deliverArgs struct {
		Session string   `json:"session" jsonschema:"session id"`
		Events  []string `json:"events" jsonschema:"queued webhooks to send now, in this order; event names or delivery ids"`
	}
	listArgs struct {
		Session  string `json:"session,omitempty" jsonschema:"only this session's deliveries"`
		Status   string `json:"status,omitempty" jsonschema:"pending, retrying, delivered, failed or canceled"`
		Limit    int    `json:"limit,omitempty" jsonschema:"page size, default 20, at most 100"`
		Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
		Delivery string `json:"delivery,omitempty" jsonschema:"one delivery id: return that delivery in full instead of a list"`
	}
	replayArgs struct {
		Delivery string  `json:"delivery" jsonschema:"delivery id from twinstub_list_deliveries"`
		Mode     string  `json:"mode,omitempty" jsonschema:"as_is (default), redeliver, duplicate, bad_signature or no_signature"`
		Body     *string `json:"body,omitempty" jsonschema:"replacement body, mode as_is only"`
		Wait     *bool   `json:"wait,omitempty" jsonschema:"wait for the receiver's answer (default true)"`
	}
	missArgs struct {
		Limit int `json:"limit,omitempty" jsonschema:"how many recent misses, default 20"`
	}
	resetArgs struct {
		Session string `json:"session,omitempty" jsonschema:"delete only this session; omit to reset everything"`
	}
)

var tools = struct {
	status, listScenarios, validateConfig, loadConfig, startSession, getSession,
	trigger, advanceTime, deliver, listDeliveries, replayDelivery, explainMiss, reset toolDef
}{
	status: toolDef{
		name: "twinstub_status", title: "Engine status", readOnly: true, idempotent: true,
		description: "Call first. Shows which engine is used (mode external or embedded), its admin URL, version, config path and public_port: the port the app under test must send its provider API calls to.",
	},
	listScenarios: toolDef{
		name: "twinstub_list_scenarios", title: "List scenarios", readOnly: true, idempotent: true,
		description: "Describe the loaded scenarios: states, request rules, webhooks with their delays and targets, the session key selector, stateless endpoints and webhook targets. Use it before starting sessions or editing YAML.",
	},
	validateConfig: toolDef{
		name: "twinstub_validate_config", title: "Validate config", readOnly: true, idempotent: true,
		description: "Check a config without loading it. Returns valid, and every error with file, line and field. Run it after each YAML edit, then twinstub_load_config.",
	},
	loadConfig: toolDef{
		name: "twinstub_load_config", title: "Load config", destructive: true,
		description: "Load a config file into the engine. The embedded engine restarts with it on the same public port, which ends all sessions. An external engine can only reload the file it was started with.",
	},
	startSession: toolDef{
		name: "twinstub_start_session", title: "Start session", idempotent: true,
		description: "Start a scenario session for a key without waiting for the app's first request. Returns the session id, state, virtual_now and the webhooks the start state scheduled. An existing session for the key is returned as is.",
	},
	getSession: toolDef{
		name: "twinstub_get_session", title: "Get session", readOnly: true, idempotent: true,
		description: "Show a session: current state, transition history, virtual_now and queued webhooks.",
	},
	trigger: toolDef{
		name: "twinstub_trigger", title: "Force state",
		description: "Move a session to a state directly, as if its rule had matched. The state's webhooks are scheduled. Returns the new state and queued webhooks.",
	},
	advanceTime: toolDef{
		name: "twinstub_advance_time", title: "Advance session time",
		description: "Move one session's clock forward (30s, 2h, 30d) and deliver everything that becomes due, in order, including the transitions and retries it causes. Use it for events that arrive days later, like chargebacks. Only TwinStub's clock moves, not the app's.",
	},
	deliver: toolDef{
		name: "twinstub_deliver", title: "Deliver in order",
		description: "Send a session's queued webhooks now, in the given order, ignoring their schedule. Use it for out-of-order delivery, like completed before pending. Returns each delivery and the receiver's answer.",
	},
	listDeliveries: toolDef{
		name: "twinstub_list_deliveries", title: "List deliveries", readOnly: true, idempotent: true,
		description: "Webhook delivery journal, newest first: event, mode, attempts, URL, the receiver's status code and whether the signature was valid. Pass delivery to see one in full, including the bodies.",
	},
	replayDelivery: toolDef{
		name: "twinstub_replay_delivery", title: "Replay delivery",
		description: "Send a delivered webhook again. Modes: as_is (optionally with an edited body), redeliver (same event, new delivery id: the usual idempotency test), duplicate (byte-identical, same delivery id), bad_signature, no_signature. Waits for the receiver's answer by default.",
	},
	explainMiss: toolDef{
		name: "twinstub_explain_miss", title: "Explain 404", readOnly: true, idempotent: true,
		description: "Recent requests that matched nothing (TwinStub answered 404): the request, the closest rules and why each one rejected it. Call it on a 404 before editing YAML.",
	},
	reset: toolDef{
		name: "twinstub_reset", title: "Reset", destructive: true, idempotent: true,
		description: "Delete one session, or without a session end all sessions, cancel queued webhooks and clear the delivery journal and 404 log. Call it between independent test runs.",
	},
}

func (s *Server) register() {
	add(s, tools.status, func(ctx context.Context, _ noInput) (any, error) { return s.status(ctx) })
	add(s, tools.listScenarios, s.listScenarios)
	add(s, tools.validateConfig, func(_ context.Context, in validateArgs) (any, error) { return validateConfig(in) })
	add(s, tools.loadConfig, s.loadConfig)
	add(s, tools.startSession, s.startSession)
	add(s, tools.getSession, s.getSession)
	add(s, tools.trigger, s.trigger)
	add(s, tools.advanceTime, s.advanceTime)
	add(s, tools.deliver, s.deliver)
	add(s, tools.listDeliveries, s.listDeliveries)
	add(s, tools.replayDelivery, s.replayDelivery)
	add(s, tools.explainMiss, s.explainMiss)
	add(s, tools.reset, s.reset)
}

func (s *Server) status(ctx context.Context) (any, error) {
	cl, done := s.engine()
	defer done()
	h, err := cl.health(ctx)
	if err != nil {
		return nil, err
	}
	engineVersion, _ := h["version"].(string)
	if !versionOK(engineVersion) {
		return nil, fmt.Errorf("the engine at %s runs twinstub %s, but these tools need %s or newer. Upgrade the twinstub that runs the engine", cl.base, engineVersion, MinEngineVersion)
	}
	out := map[string]any{
		"mode":        s.mode,
		"admin_url":   cl.base,
		"version":     engineVersion,
		"mcp_version": version.Version,
		"config_path": h["config_path"],
		"public_port": h["public_port"],
		"time_scale":  h["time_scale"],
		"sessions":    h["sessions"],
	}
	if s.mode == ModeEmbedded {
		if s.configPath == "" {
			out["config_path"] = nil
			out["hint"] = "No config is loaded. Write a config with a scenario, check it with twinstub_validate_config, then call twinstub_load_config."
		}
		if s.configErr != "" {
			out["config_error"] = s.configErr
		}
		if s.portNote != "" {
			out["note"] = s.portNote
		}
	}
	if port, ok := h["public_port"].(float64); ok && port > 0 {
		out["app_base_url"] = fmt.Sprintf("http://localhost:%d", int(port))
	}
	return out, nil
}

func (s *Server) listScenarios(ctx context.Context, in scenarioArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	q := url.Values{}
	if in.Name != "" {
		q.Set("id", in.Name)
	}
	out, err := cl.do(ctx, http.MethodGet, "/scenarios", q, nil)
	if err != nil {
		return nil, err
	}
	if list, ok := out["scenarios"].([]any); ok {
		for _, sc := range list {
			if m, ok := sc.(map[string]any); ok {
				delete(m, "config_version")
			}
		}
	}
	return out, nil
}

func (s *Server) loadConfig(ctx context.Context, in loadArgs) (any, error) {
	if in.Path == "" {
		return nil, errors.New("path is required: the root config file to load")
	}
	abs, err := filepath.Abs(in.Path)
	if err != nil {
		return nil, err
	}
	check, err := validateConfig(validateArgs{Path: abs})
	if err != nil {
		return nil, err
	}
	if !check.Valid {
		return nil, fmt.Errorf("%s was not loaded: %s. Fix the file (twinstub_validate_config lists every error) and load it again", abs, firstErrors(check.Errors, 3))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == ModeExternal {
		h, err := s.cl.health(ctx)
		if err != nil {
			return nil, err
		}
		if served, _ := h["config_path"].(string); served != abs {
			return nil, fmt.Errorf("the external engine at %s serves %s and cannot switch to another file. Edit that file (it reloads on save), or restart the engine with `twinstub serve -c %s`", s.cl.base, served, abs)
		}
		if _, err := s.cl.do(ctx, http.MethodPost, "/reload", nil, nil); err != nil {
			return nil, err
		}
		return s.loadedLocked(ctx, false)
	}

	port := s.app.PublicPort()
	if err := s.stopEmbeddedLocked(ctx); err != nil {
		s.opts.Logger.Warn().Err(err).Msg("embedded engine did not stop cleanly")
	}
	if err := s.startEmbedded(abs, true, port); err != nil {
		return nil, err
	}
	if s.configErr != "" {
		return nil, fmt.Errorf("%s did not load: %s", abs, s.configErr)
	}
	return s.loadedLocked(ctx, true)
}

// loadedLocked summarises the engine after a load.
func (s *Server) loadedLocked(ctx context.Context, restarted bool) (any, error) {
	sc, err := s.cl.do(ctx, http.MethodGet, "/scenarios", nil, nil)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	if list, ok := sc["scenarios"].([]any); ok {
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				ids = append(ids, fmt.Sprint(m["id"]))
			}
		}
	}
	out := map[string]any{"loaded": true, "scenarios": ids, "endpoints": count(sc["endpoints"]), "targets": sc["targets"]}
	if restarted {
		out["sessions_reset"] = true
		out["public_port"] = s.app.PublicPort()
	}
	return out, nil
}

func (s *Server) startSession(ctx context.Context, in startArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	out, err := cl.do(ctx, http.MethodPost, "/sessions", nil, map[string]any{"scenario": in.Scenario, "key": in.Key, "state": in.InitialState})
	if err != nil {
		return nil, err
	}
	res := sessionSummary(out["session"])
	res["created"] = out["created"]
	res["virtual_now"] = out["virtual_now"]
	res["pending_webhooks"] = out["pending_webhooks"]
	return res, nil
}

func (s *Server) getSession(ctx context.Context, in sessionArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	out, err := cl.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(in.Session), nil, nil)
	if err != nil {
		return nil, err
	}
	res := sessionSummary(out["session"])
	res["virtual_now"] = out["virtual_now"]
	res["clock_offset"] = out["clock_offset"]
	res["pending_webhooks"] = out["pending_webhooks"]
	if h, ok := out["history"].([]any); ok && len(h) > historyLimit {
		res["history"] = h[len(h)-historyLimit:]
		res["history_omitted"] = len(h) - historyLimit
	} else {
		res["history"] = out["history"]
	}
	return res, nil
}

func (s *Server) trigger(ctx context.Context, in triggerArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	out, err := cl.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(in.Session)+"/advance", nil, map[string]any{"state": in.State})
	if err != nil {
		return nil, err
	}
	res := sessionSummary(out["session"])
	res["pending_webhooks"] = out["pending_webhooks"]
	return res, nil
}

func (s *Server) advanceTime(ctx context.Context, in advanceArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	out, err := cl.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(in.Session)+"/advance-time", nil,
		map[string]any{"duration": in.Duration, "timeout": s.opts.Timeout.String()})
	if err != nil {
		return nil, err
	}
	return firedResult(out, "virtual_now", "clock_offset"), nil
}

func (s *Server) deliver(ctx context.Context, in deliverArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	out, err := cl.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(in.Session)+"/deliver", nil,
		map[string]any{"events": in.Events, "timeout": s.opts.Timeout.String()})
	if err != nil {
		return nil, err
	}
	return firedResult(out), nil
}

// firedResult keeps the parts of an advance or deliver answer an agent acts
// on: what fired, whether the call finished, and where the session ended.
func firedResult(out map[string]any, keep ...string) map[string]any {
	res := map[string]any{"fired": out["fired"]}
	for _, k := range append(keep, "partial", "reason") {
		if v, ok := out[k]; ok {
			res[k] = v
		}
	}
	if sess, ok := out["session"].(map[string]any); ok {
		res["state"] = sess["state"]
	} else {
		res["state"] = nil
		res["note"] = "the session no longer exists"
	}
	return res
}

func (s *Server) listDeliveries(ctx context.Context, in listArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	if in.Delivery != "" {
		rec, err := cl.do(ctx, http.MethodGet, "/deliveries/"+url.PathEscape(in.Delivery), nil, nil)
		if err != nil {
			return nil, err
		}
		return fullDelivery(rec), nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	for k, v := range map[string]string{"session": in.Session, "status": in.Status, "cursor": in.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	out, err := cl.do(ctx, http.MethodGet, "/deliveries", q, nil)
	if err != nil {
		return nil, err
	}
	list, _ := out["deliveries"].([]any)
	items := make([]map[string]any, 0, len(list))
	for _, r := range list {
		if m, ok := r.(map[string]any); ok {
			items = append(items, deliverySummary(m))
		}
	}
	res := map[string]any{"deliveries": items, "count": len(items)}
	if next, ok := out["next_cursor"]; ok {
		res["next_cursor"] = next
	}
	return res, nil
}

func (s *Server) replayDelivery(ctx context.Context, in replayArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	wait := in.Wait == nil || *in.Wait
	body := map[string]any{"mode": in.Mode, "wait": wait, "timeout": s.opts.Timeout.String()}
	if in.Body != nil {
		body["body"] = *in.Body
	}
	out, err := cl.do(ctx, http.MethodPost, "/deliveries/"+url.PathEscape(in.Delivery)+"/replay", nil, body)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"delivery_id": out["delivery_id"], "replayed_from": out["replayed_from"], "mode": out["mode"]}
	if rec, ok := out["delivery"].(map[string]any); ok {
		res["delivery"] = deliverySummary(rec)
	} else if wait {
		res["note"] = "no answer from the receiver yet; check it later with twinstub_list_deliveries"
	}
	return res, nil
}

func (s *Server) explainMiss(ctx context.Context, in missArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	out, err := cl.do(ctx, http.MethodGet, "/misses", url.Values{"limit": {strconv.Itoa(min(limit, maxListLimit))}}, nil)
	if err != nil {
		return nil, err
	}
	if n, _ := out["count"].(float64); n == 0 {
		out["note"] = "no request has missed since the last reset"
	}
	return out, nil
}

func (s *Server) reset(ctx context.Context, in resetArgs) (any, error) {
	cl, done := s.engine()
	defer done()
	if in.Session != "" {
		return cl.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(in.Session), nil, nil)
	}
	return cl.do(ctx, http.MethodPost, "/reset", nil, map[string]any{})
}

func sessionSummary(v any) map[string]any {
	m, _ := v.(map[string]any)
	out := map[string]any{}
	for _, k := range []string{"id", "scenario", "key", "state", "vars", "expires"} {
		if x, ok := m[k]; ok {
			out[k] = x
		}
	}
	if id, ok := out["id"]; ok {
		delete(out, "id")
		out["session_id"] = id
	}
	return out
}

// deliverySummary is the list view of a journal record: what was sent,
// where, and how the receiver answered the last attempt.
func deliverySummary(rec map[string]any) map[string]any {
	out := pick(rec, "id", "session_id", "event", "target", "url", "status", "mode", "replayed_from", "header_id", "created_at", "next_attempt_at")
	attempts, _ := rec["attempts"].([]any)
	out["attempts"] = len(attempts)
	if len(attempts) > 0 {
		if last, ok := attempts[len(attempts)-1].(map[string]any); ok {
			la := pick(last, "at", "virtual_at", "status_code", "error", "signature", "duration_ms")
			clipField(la, last, "response_body", listBodyLimit)
			out["last_attempt"] = la
		}
	}
	clipField(out, rec, "request_body", listBodyLimit)
	return out
}

// fullDelivery is a whole journal record with long bodies cut.
func fullDelivery(rec map[string]any) map[string]any {
	clipField(rec, rec, "request_body", fullBodyLimit)
	if attempts, ok := rec["attempts"].([]any); ok {
		for _, a := range attempts {
			if m, ok := a.(map[string]any); ok {
				clipField(m, m, "response_body", fullBodyLimit)
			}
		}
	}
	return rec
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && v != "" {
			out[k] = v
		}
	}
	return out
}

// clipField copies src[key] into dst, cut to limit bytes. A cut sets
// truncated and the original size.
func clipField(dst, src map[string]any, key string, limit int) {
	s, ok := src[key].(string)
	if !ok || s == "" {
		return
	}
	if len(s) <= limit {
		dst[key] = s
		return
	}
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	dst[key] = s[:cut]
	dst["truncated"] = true
	dst[key+"_bytes"] = len(s)
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func count(v any) int {
	l, _ := v.([]any)
	return len(l)
}

func firstErrors(errs []configError, n int) string {
	parts := make([]string, 0, n)
	for i, e := range errs {
		if i == n {
			parts = append(parts, fmt.Sprintf("and %d more", len(errs)-n))
			break
		}
		parts = append(parts, e.String())
	}
	return strings.Join(parts, "; ")
}
