// Package admin serves the management API on a separate port (spec 8).
// Paths are versioned under /api/v1 and are a stable contract: the future
// commercial layer drives the core exclusively through this API.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/engine"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/version"
	"github.com/twinstub/twinstub/internal/webhook"
	"github.com/twinstub/twinstub/internal/xtime"
)

const (
	// defaultAdvanceTimeout bounds one advance-time or deliver call: both
	// wait for the receiver to answer every delivery they send.
	defaultAdvanceTimeout = 60 * time.Second
	maxAdvanceTimeout     = 10 * time.Minute
)

type API struct {
	snap    func() *snapshot.Snapshot
	engine  *engine.Engine
	disp    *webhook.Dispatcher
	reload  func() error
	token   string
	started time.Time
	logger  zerolog.Logger
}

func New(snap func() *snapshot.Snapshot, eng *engine.Engine, disp *webhook.Dispatcher, reload func() error, token string, logger zerolog.Logger) *API {
	return &API{
		snap:    snap,
		engine:  eng,
		disp:    disp,
		reload:  reload,
		token:   token,
		started: time.Now(),
		logger:  logger,
	}
}

func (a *API) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(a.auth)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", a.health)
		r.Get("/scenarios", a.scenarios)
		r.Get("/sessions", a.sessions)
		r.Get("/sessions/{id}", a.sessionDetail)
		r.Delete("/sessions/{id}", a.sessionDelete)
		r.Post("/sessions/{id}/advance", a.sessionAdvance)
		r.Post("/sessions/{id}/advance-time", a.sessionAdvanceTime)
		r.Post("/sessions/{id}/deliver", a.sessionDeliver)
		r.Get("/deliveries", a.deliveries)
		r.Get("/deliveries/{id}", a.deliveryDetail)
		r.Post("/deliveries/{id}/replay", a.deliveryReplay)
		r.Post("/trigger", a.trigger)
		r.Post("/reload", a.reloadHandler)
	})
	return r
}

// auth enforces the optional bearer token. Empty token means no auth,
// which is the documented local-dev default.
func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" && r.Header.Get("Authorization") != "Bearer "+a.token {
			writeJSON(w, 401, map[string]any{"error": "unauthorized", "detail": "set the Authorization: Bearer <token> header"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"status":         "ok",
		"version":        version.Version,
		"commit":         version.Commit,
		"uptime_seconds": int(time.Since(a.started).Seconds()),
		"config_version": a.snap().Version,
	})
}

func (a *API) scenarios(w http.ResponseWriter, r *http.Request) {
	snap := a.snap()
	type scenario struct {
		ID            string   `json:"id"`
		Description   string   `json:"description,omitempty"`
		States        []string `json:"states"`
		Initial       string   `json:"initial"`
		SessionKey    string   `json:"session_key"`
		TTL           string   `json:"ttl"`
		ConfigVersion int      `json:"config_version"`
	}
	out := make([]scenario, 0, len(snap.Scenarios))
	for _, s := range snap.Scenarios {
		out = append(out, scenario{
			ID:            s.ID,
			Description:   s.Description,
			States:        s.StateNames,
			Initial:       s.Initial,
			SessionKey:    s.KeySelector.String(),
			TTL:           s.TTL.String(),
			ConfigVersion: snap.Version,
		})
	}
	writeJSON(w, 200, map[string]any{"scenarios": out})
}

func (a *API) sessions(w http.ResponseWriter, r *http.Request) {
	list := a.engine.Store.List()
	out := make([]engine.SessionView, 0, len(list))
	for _, s := range list {
		out = append(out, s.View())
	}
	writeJSON(w, 200, map[string]any{"sessions": out, "count": len(out)})
}

func (a *API) sessionDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess := a.engine.Store.GetByID(id)
	if sess == nil {
		writeJSON(w, 404, map[string]any{"error": "session_not_found", "id": id})
		return
	}
	off := a.disp.ClockOffset(id)
	writeJSON(w, 200, map[string]any{
		"session":          sess.View(),
		"virtual_now":      time.Now().Add(off),
		"clock_offset":     off.String(),
		"pending_webhooks": a.disp.PendingForSession(id),
	})
}

func (a *API) sessionDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !a.engine.DeleteSession(id) {
		writeJSON(w, 404, map[string]any{"error": "session_not_found", "id": id})
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (a *API) sessionAdvance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		State string `json:"state"`
	}
	if err := decodeBody(r, &body); err != nil || body.State == "" {
		writeJSON(w, 400, map[string]any{"error": "bad_request", "detail": "expected JSON body {\"state\": \"<name>\"}"})
		return
	}
	if err := a.engine.AdvanceTo(id, body.State); err != nil {
		status := 404
		writeJSON(w, status, map[string]any{"error": "advance_failed", "detail": err.Error()})
		return
	}
	sess := a.engine.Store.GetByID(id)
	writeJSON(w, 200, map[string]any{"session": sess.View()})
}

// sessionAdvanceTime moves one session's virtual clock forward and replays
// what becomes due inside the window, in order.
func (a *API) sessionAdvanceTime(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Duration string `json:"duration"`
		Timeout  string `json:"timeout"`
	}
	if err := decodeBody(r, &body); err != nil || body.Duration == "" {
		writeJSON(w, 400, map[string]any{"error": "bad_request", "detail": "expected JSON body {\"duration\": \"30d\"} with an optional \"timeout\": \"60s\""})
		return
	}
	by, err := xtime.ParseDuration(body.Duration)
	if err != nil || by < 0 {
		writeJSON(w, 400, map[string]any{"error": "bad_duration", "detail": "duration must be like 30s, 2h or 30d"})
		return
	}
	timeout, ok := parseTimeout(w, body.Timeout)
	if !ok {
		return
	}
	if !a.engine.Touch(id) {
		writeJSON(w, 404, map[string]any{"error": "session_not_found", "id": id})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := a.disp.Advance(ctx, id, by)
	switch {
	case errors.Is(err, webhook.ErrSessionBusy):
		writeJSON(w, 409, map[string]any{"error": "session_busy", "detail": err.Error()})
		return
	case err != nil:
		writeJSON(w, 400, map[string]any{"error": "advance_failed", "detail": err.Error()})
		return
	}

	out := map[string]any{
		"virtual_now":  res.VirtualNow,
		"clock_offset": res.ClockOffset.String(),
		"fired":        res.Fired,
	}
	if res.Partial {
		out["partial"] = true
		out["reason"] = res.Reason
	}
	// The session can be gone by now if it was reset during the call.
	if sess := a.engine.Store.GetByID(id); sess != nil {
		out["session"] = sess.View()
	}
	writeJSON(w, 200, out)
}

// sessionDeliver sends a session's queued webhooks now, in the given order,
// ignoring their schedule. The session clock does not move.
func (a *API) sessionDeliver(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Events  []string `json:"events"`
		Timeout string   `json:"timeout"`
	}
	if err := decodeBody(r, &body); err != nil || len(body.Events) == 0 {
		writeJSON(w, 400, map[string]any{"error": "bad_request", "detail": "expected JSON body {\"events\": [\"payment.completed\", \"payment.pending\"]}; items may also be delivery ids"})
		return
	}
	timeout, ok := parseTimeout(w, body.Timeout)
	if !ok {
		return
	}
	if !a.engine.Touch(id) {
		writeJSON(w, 404, map[string]any{"error": "session_not_found", "id": id})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := a.disp.DeliverNow(ctx, id, body.Events)
	var notPending *webhook.NotPendingError
	switch {
	case errors.Is(err, webhook.ErrSessionBusy):
		writeJSON(w, 409, map[string]any{"error": "session_busy", "detail": err.Error()})
		return
	case errors.As(err, &notPending):
		writeJSON(w, 422, map[string]any{
			"error":          "not_pending",
			"detail":         err.Error(),
			"item":           notPending.Item,
			"pending_events": notPending.Pending,
			"fired":          res.Fired,
			"hint":           "only queued webhooks can be sent early. An earlier item in the list may queue it, or the session may need a request from your app first. Resend an already delivered event with POST /api/v1/deliveries/{id}/replay.",
		})
		return
	case err != nil:
		writeJSON(w, 400, map[string]any{"error": "deliver_failed", "detail": err.Error()})
		return
	}

	out := map[string]any{"fired": res.Fired}
	if res.Partial {
		out["partial"] = true
		out["reason"] = res.Reason
	}
	if sess := a.engine.Store.GetByID(id); sess != nil {
		out["session"] = sess.View()
	}
	writeJSON(w, 200, out)
}

// parseTimeout reads an optional call budget, writing a 400 on bad input.
func parseTimeout(w http.ResponseWriter, raw string) (time.Duration, bool) {
	if raw == "" {
		return defaultAdvanceTimeout, true
	}
	t, err := xtime.ParseDuration(raw)
	if err != nil || t <= 0 {
		writeJSON(w, 400, map[string]any{"error": "bad_timeout", "detail": "timeout must be like 30s or 2m"})
		return 0, false
	}
	return min(t, maxAdvanceTimeout), true
}

func (a *API) deliveries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list := a.disp.Log().List(r.URL.Query().Get("session"), r.URL.Query().Get("target"), limit)
	writeJSON(w, 200, map[string]any{"deliveries": list, "count": len(list)})
}

func (a *API) deliveryDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, ok := a.disp.Log().Get(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "delivery_not_found", "id": id})
		return
	}
	writeJSON(w, 200, rec)
}

// deliveryReplay resends a logged delivery (Replay and Edit, spec 8). The
// mode picks what the receiver sees; wait returns the first attempt.
func (a *API) deliveryReplay(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rec, ok := a.disp.Log().Get(id)
	if !ok {
		writeJSON(w, 404, map[string]any{"error": "delivery_not_found", "id": id})
		return
	}
	var body struct {
		Body    *string `json:"body"`
		Mode    string  `json:"mode"`
		Wait    bool    `json:"wait"`
		Timeout string  `json:"timeout"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeJSON(w, 400, map[string]any{"error": "bad_request", "detail": err.Error()})
		return
	}
	target, ok := a.snap().Config.Targets[rec.Target]
	if !ok {
		writeJSON(w, 422, map[string]any{
			"error":  "target_not_found",
			"detail": "target " + rec.Target + " is no longer defined in the config",
		})
		return
	}
	req := webhook.ScheduleRequest{
		SessionID:    rec.SessionID,
		TargetName:   rec.Target,
		Target:       target,
		Event:        rec.Event,
		Body:         []byte(rec.RequestBody),
		ReplayedFrom: id,
	}
	if body.Body != nil {
		req.Body = []byte(*body.Body)
	}
	switch body.Mode {
	case "", "as_is":
		req.Mode = webhook.ModeReplay
	case webhook.ModeDuplicate:
		if body.Body != nil {
			writeJSON(w, 400, map[string]any{"error": "bad_request", "detail": "a duplicate resends the original bytes; drop \"body\" or use mode as_is"})
			return
		}
		// A duplicate of a duplicate still carries the first delivery's id.
		req.Mode, req.HeaderID = webhook.ModeDuplicate, rec.ID
		if rec.HeaderID != "" {
			req.HeaderID = rec.HeaderID
		}
	case webhook.ModeBadSignature:
		if target.Signing == nil {
			writeJSON(w, 422, map[string]any{"error": "signing_not_configured", "detail": "target " + rec.Target + " has no signing configured, so there is no signature to break"})
			return
		}
		req.Mode, req.BadSign = webhook.ModeBadSignature, true
	case webhook.ModeNoSignature:
		req.Mode, req.NoSign = webhook.ModeNoSignature, true
	default:
		writeJSON(w, 400, map[string]any{"error": "bad_mode", "detail": "mode must be as_is, duplicate, bad_signature or no_signature"})
		return
	}
	timeout, ok := parseTimeout(w, body.Timeout)
	if !ok {
		return
	}

	newID := a.disp.Schedule(req)
	out := map[string]any{"delivery_id": newID, "replayed_from": id, "mode": req.Mode}
	if !body.Wait {
		writeJSON(w, 202, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	done, ok := a.waitFirstAttempt(ctx, newID)
	if !ok {
		out["detail"] = "no attempt finished before the timeout; poll GET /api/v1/deliveries/" + newID
		writeJSON(w, 202, out)
		return
	}
	out["delivery"] = done
	writeJSON(w, 200, out)
}

// waitFirstAttempt polls the journal until a delivery has an attempt.
func (a *API) waitFirstAttempt(ctx context.Context, id string) (webhook.Record, bool) {
	for {
		if rec, ok := a.disp.Log().Get(id); ok && (len(rec.Attempts) > 0 || rec.Status == webhook.StatusCanceled) {
			return rec, true
		}
		select {
		case <-ctx.Done():
			return webhook.Record{}, false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// trigger sends a one-off webhook outside any scenario.
func (a *API) trigger(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string          `json:"target"`
		Body   json.RawMessage `json:"body"`
		Sign   *bool           `json:"sign"`
	}
	if err := decodeBody(r, &body); err != nil || body.Target == "" {
		writeJSON(w, 400, map[string]any{"error": "bad_request", "detail": "expected JSON body {\"target\": \"<name>\", \"body\": {...}, \"sign\": true}"})
		return
	}
	target, ok := a.snap().Config.Targets[body.Target]
	if !ok {
		writeJSON(w, 422, map[string]any{"error": "target_not_found", "detail": "target " + body.Target + " is not defined in the config"})
		return
	}
	id := a.disp.Schedule(webhook.ScheduleRequest{
		TargetName: body.Target,
		Target:     target,
		Body:       body.Body,
		NoSign:     body.Sign != nil && !*body.Sign,
	})
	writeJSON(w, 202, map[string]any{"delivery_id": id})
}

func (a *API) reloadHandler(w http.ResponseWriter, r *http.Request) {
	if err := a.reload(); err != nil {
		writeJSON(w, 422, map[string]any{"error": "reload_failed", "detail": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"reloaded": true, "config_version": a.snap().Version})
}

func decodeBody(r *http.Request, into any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, 6<<20))
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, into)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// The status line is already sent; an encode error has nowhere to go.
	_ = enc.Encode(v)
}
