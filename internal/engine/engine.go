// Package engine implements the stateful scenario state machine
// (spec 5.3, 5.4): session resolution, request matching against the current
// state, transitions and enter actions.
package engine

import (
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/match"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
	"github.com/twinstub/twinstub/internal/webhook"
)

// Scheduler is the slice of the dispatcher the engine needs.
type Scheduler interface {
	Schedule(req webhook.ScheduleRequest) string
	CancelSession(sessionID string)
	PendingCount(sessionID string) int
}

type Engine struct {
	Store      *Store
	sched      Scheduler
	timeScale  float64
	newID      func() string
	maxPending int
	logger     zerolog.Logger
}

func New(store *Store, sched Scheduler, timeScale float64, maxPending int, newID func() string, logger zerolog.Logger) *Engine {
	if timeScale <= 0 {
		timeScale = 1.0
	}
	return &Engine{
		Store:      store,
		sched:      sched,
		timeScale:  timeScale,
		newID:      newID,
		maxPending: maxPending,
		logger:     logger,
	}
}

// ErrorReply is a structured engine-level error response (409, 422, ...).
type ErrorReply struct {
	Status int
	Body   map[string]any
}

// Outcome of routing one request through the engine.
type Outcome struct {
	Handled bool
	Reply   *snapshot.Rendered
	Err     *ErrorReply

	// Diagnostics for logging and the 404 body.
	SessionID  string
	ScenarioID string
	State      string
	// InSession is true when a session matched but none of its rules did,
	// so the request fell through to stateless endpoints.
	InSession bool
}

// Handle routes a request through the scenario engine. A nil-Handled
// outcome means the caller should try stateless endpoints next.
func (e *Engine) Handle(snap *snapshot.Snapshot, r *match.Request, scenarioHeader string) Outcome {
	sess := e.findSession(snap, r)

	if sess == nil && scenarioHeader == "" {
		return Outcome{}
	}

	if sess == nil {
		sc, ok := snap.ScenarioByID[scenarioHeader]
		if !ok {
			known := make([]string, 0, len(snap.ScenarioByID))
			for _, s := range snap.Scenarios {
				known = append(known, s.ID)
			}
			return Outcome{Err: &ErrorReply{Status: 404, Body: map[string]any{
				"error":           "unknown_scenario",
				"detail":          fmt.Sprintf("scenario %q is not defined in the loaded config", scenarioHeader),
				"known_scenarios": known,
			}}}
		}
		key, ok := sc.KeySelector.Extract(r, sc)
		if !ok {
			return Outcome{Err: &ErrorReply{Status: 422, Body: map[string]any{
				"error":    "session_key_not_found",
				"detail":   fmt.Sprintf("session key selector %q did not yield a value from this request", sc.KeySelector),
				"selector": sc.KeySelector.String(),
				"scenario": sc.ID,
			}}}
		}
		var created bool
		var err error
		sess, created, err = e.createSession(snap, sc, key)
		if err != nil {
			return Outcome{Err: &ErrorReply{Status: 429, Body: map[string]any{
				"error":  "session_limit_reached",
				"detail": "the configured limit of concurrent sessions is reached (limits.max_sessions)",
			}}}
		}
		if created {
			e.logger.Info().Str("session", sess.ID).Str("scenario", sc.ID).Str("key", key).Msg("session created")
		}
	}

	// A scenario header naming a different scenario while the key is bound
	// to a live session is a conflict (spec 5.4).
	if scenarioHeader != "" && scenarioHeader != sess.Scenario.ID {
		return Outcome{Err: &ErrorReply{Status: 409, Body: map[string]any{
			"error":             "session_conflict",
			"detail":            fmt.Sprintf("session key %q is already bound to scenario %q; delete the session or wait for its TTL before starting %q", sess.Key, sess.Scenario.ID, scenarioHeader),
			"session_id":        sess.ID,
			"existing_scenario": sess.Scenario.ID,
			"requested":         scenarioHeader,
		}}}
	}

	return e.handleInSession(sess, r)
}

func (e *Engine) findSession(snap *snapshot.Snapshot, r *match.Request) *Session {
	// Try selectors of currently loaded scenarios first, then selectors of
	// live sessions whose scenario version may be gone after a reload.
	for _, sc := range snap.Scenarios {
		if key, ok := sc.KeySelector.Extract(r, sc); ok {
			if sess := e.Store.GetByKey(key); sess != nil {
				return sess
			}
		}
	}
	for _, ref := range e.Store.LiveSelectors() {
		if key, ok := ref.Sel.Extract(r, ref.Scenario); ok {
			if sess := e.Store.GetByKey(key); sess != nil {
				return sess
			}
		}
	}
	return nil
}

func (e *Engine) createSession(snap *snapshot.Snapshot, sc *snapshot.Scenario, key string) (*Session, bool, error) {
	now := time.Now()
	sess := &Session{
		ID:            e.newID(),
		Key:           key,
		Scenario:      sc,
		ConfigVersion: snap.Version,
		State:         sc.Initial,
		Vars:          map[string]string{},
		CreatedAt:     now,
		ExpiresAt:     now.Add(sc.TTL),
	}
	existing, created, err := e.Store.Create(sess)
	if err != nil {
		return nil, false, err
	}
	if created {
		// Enter actions of the initial state run at session creation.
		sess.mu.Lock()
		e.runEnterLocked(sess, &tmpl.RequestCtx{})
		sess.mu.Unlock()
	}
	return existing, created, nil
}

func (e *Engine) handleInSession(sess *Session, r *match.Request) Outcome {
	sess.mu.Lock()
	defer sess.mu.Unlock()

	now := time.Now()
	sess.touchLocked(now)

	st, ok := sess.Scenario.States[sess.State]
	if !ok {
		return Outcome{Err: &ErrorReply{Status: 500, Body: map[string]any{
			"error":  "internal",
			"detail": fmt.Sprintf("session %s is in unknown state %q", sess.ID, sess.State),
		}}}
	}

	for _, rule := range st.On {
		params, ok := rule.Matcher.Match(r)
		if !ok {
			continue
		}
		ctx := buildContext(r, params, sess)
		rendered, err := rule.Reply.Render(ctx)
		if err != nil {
			return Outcome{
				Err:        templateError(err),
				SessionID:  sess.ID,
				ScenarioID: sess.Scenario.ID,
				State:      sess.State,
			}
		}
		for _, sv := range rule.Set {
			v, err := tmpl.Render(sv.Tmpl, ctx)
			if err != nil {
				return Outcome{
					Err:        templateError(err),
					SessionID:  sess.ID,
					ScenarioID: sess.Scenario.ID,
					State:      sess.State,
				}
			}
			sess.Vars[sv.Name] = string(v)
		}
		out := Outcome{
			Handled:    true,
			Reply:      rendered,
			SessionID:  sess.ID,
			ScenarioID: sess.Scenario.ID,
			State:      sess.State,
		}
		if rule.Goto != "" {
			e.enterStateLocked(sess, rule.Goto, &ctx.Request)
			out.State = sess.State
		}
		return out
	}

	// No rule in the current state matched: fall through to stateless
	// endpoints (spec 5.3), keeping diagnostics about the session.
	return Outcome{
		InSession:  true,
		SessionID:  sess.ID,
		ScenarioID: sess.Scenario.ID,
		State:      sess.State,
	}
}

func templateError(err error) *ErrorReply {
	return &ErrorReply{Status: 500, Body: map[string]any{
		"error":  "template_error",
		"detail": err.Error(),
	}}
}

// enterStateLocked transitions the session and schedules enter actions.
// Caller holds sess.mu.
func (e *Engine) enterStateLocked(sess *Session, state string, reqCtx *tmpl.RequestCtx) {
	sess.State = state
	sess.Epoch++
	e.logger.Debug().Str("session", sess.ID).Str("state", state).Msg("state entered")
	e.runEnterLocked(sess, reqCtx)
}

func (e *Engine) runEnterLocked(sess *Session, reqCtx *tmpl.RequestCtx) {
	st, ok := sess.Scenario.States[sess.State]
	if !ok {
		return
	}
	epoch := sess.Epoch
	for _, action := range st.Enter {
		ctx := &tmpl.Context{Request: *reqCtx, Session: sessionCtx(sess)}
		body, err := tmpl.Render(action.Body, ctx)
		if err != nil {
			e.logger.Error().Str("session", sess.ID).Str("state", sess.State).
				Err(err).Msg("webhook body template failed, action skipped")
			continue
		}
		if e.maxPending > 0 && e.sched.PendingCount(sess.ID) >= e.maxPending {
			e.logger.Error().Str("session", sess.ID).Int("limit", e.maxPending).
				Msg("pending webhook limit reached, action skipped")
			continue
		}
		var onFirst func()
		if g := action.Goto; g != "" {
			sid := sess.ID
			onFirst = func() { e.advance(sid, epoch, g) }
		}
		id := e.sched.Schedule(webhook.ScheduleRequest{
			SessionID:      sess.ID,
			TargetName:     action.TargetName,
			Target:         action.Target,
			Event:          action.Event,
			Body:           body,
			Delay:          time.Duration(float64(action.After) * e.timeScale),
			OnFirstAttempt: onFirst,
		})
		e.logger.Info().Str("session", sess.ID).Str("delivery", id).
			Str("target", action.TargetName).Dur("after", time.Duration(float64(action.After)*e.timeScale)).
			Msg("webhook scheduled")
	}
}

// advance applies a post-dispatch goto. It is a no-op when the session has
// moved to another state since the webhook was scheduled.
func (e *Engine) advance(sessionID string, epoch uint64, state string) {
	sess := e.Store.GetByID(sessionID)
	if sess == nil {
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.Epoch != epoch {
		return
	}
	e.enterStateLocked(sess, state, &tmpl.RequestCtx{})
}

// AdvanceTo is the admin API forced transition. Enter actions run.
func (e *Engine) AdvanceTo(sessionID, state string) error {
	sess := e.Store.GetByID(sessionID)
	if sess == nil {
		return fmt.Errorf("session %q not found", sessionID)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if _, ok := sess.Scenario.States[state]; !ok {
		return fmt.Errorf("scenario %q has no state %q", sess.Scenario.ID, state)
	}
	sess.touchLocked(time.Now())
	e.enterStateLocked(sess, state, &tmpl.RequestCtx{})
	return nil
}

// DeleteSession removes a session and cancels its queued webhooks.
func (e *Engine) DeleteSession(id string) bool {
	sess := e.Store.Delete(id)
	if sess == nil {
		return false
	}
	e.sched.CancelSession(id)
	e.logger.Info().Str("session", id).Msg("session deleted")
	return true
}

// SweepExpired is called periodically by the TTL sweeper.
func (e *Engine) SweepExpired() {
	for _, sess := range e.Store.Sweep(time.Now()) {
		e.sched.CancelSession(sess.ID)
		e.logger.Info().Str("session", sess.ID).Msg("session expired")
	}
}

// RunSweeper starts the background TTL sweeper. Returns a stop func.
func (e *Engine) RunSweeper(interval time.Duration) func() {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				e.SweepExpired()
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

func sessionCtx(sess *Session) *tmpl.SessionCtx {
	return &tmpl.SessionCtx{
		ID:       sess.ID,
		Key:      sess.Key,
		Scenario: sess.Scenario.ID,
		State:    sess.State,
		Vars:     sess.Vars,
	}
}

// buildContext assembles the template render context for a matched rule.
func buildContext(r *match.Request, params map[string]string, sess *Session) *tmpl.Context {
	return &tmpl.Context{
		Request: *NewRequestCtx(r, params),
		Session: sessionCtx(sess),
	}
}

// NewRequestCtx converts a matched request into the template context shape.
// Exported because the stateless server path reuses it.
func NewRequestCtx(r *match.Request, params map[string]string) *tmpl.RequestCtx {
	q := map[string]string{}
	for k, vs := range r.Query {
		if len(vs) > 0 {
			q[k] = vs[0]
		}
	}
	h := map[string]string{}
	for k := range r.Headers {
		h[k] = r.Headers.Get(k)
	}
	if params == nil {
		params = map[string]string{}
	}
	return &tmpl.RequestCtx{
		Method:  r.Method,
		Path:    r.Path,
		Params:  params,
		Query:   q,
		Headers: h,
		Body:    r.Body,
		RawBody: string(r.RawBody),
	}
}
