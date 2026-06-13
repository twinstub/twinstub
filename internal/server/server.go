// Package server is the public mock server: scenario engine first, then
// stateless endpoints, then a diagnostic 404 (spec 4.2).
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/engine"
	"github.com/twinstub/twinstub/internal/match"
	"github.com/twinstub/twinstub/internal/snapshot"
	"github.com/twinstub/twinstub/internal/tmpl"
)

// maxRequestBody caps incoming request bodies, mirroring the reply limit.
const maxRequestBody = config.HardMaxBodySize

type Server struct {
	snap   func() *snapshot.Snapshot
	engine *engine.Engine
	logger zerolog.Logger
}

func New(snap func() *snapshot.Snapshot, eng *engine.Engine, logger zerolog.Logger) *Server {
	return &Server{snap: snap, engine: eng, logger: logger}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.HandleFunc("/*", s.handle)
	return r
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	snap := s.snap()

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": "body_read_failed", "detail": err.Error()})
		return
	}
	if len(raw) > maxRequestBody {
		writeJSON(w, 413, map[string]any{"error": "body_too_large", "detail": "request body exceeds the 5 MB limit"})
		return
	}

	mreq := &match.Request{
		Method:  r.Method,
		Path:    r.URL.Path,
		Query:   r.URL.Query(),
		Headers: r.Header,
		RawBody: raw,
	}
	if len(raw) > 0 {
		var body any
		if json.Unmarshal(raw, &body) == nil {
			mreq.Body = body
		}
	}

	scenarioHeader := r.Header.Get(snap.ScenarioHeader)
	out := s.engine.Handle(snap, mreq, scenarioHeader)

	log := s.logger.Info().Str("method", r.Method).Str("path", r.URL.Path)
	if out.SessionID != "" {
		log = log.Str("session", out.SessionID).Str("scenario", out.ScenarioID).Str("state", out.State)
	}

	switch {
	case out.Err != nil:
		writeJSON(w, out.Err.Status, out.Err.Body)
		log.Int("status", out.Err.Status).Dur("dur", time.Since(started)).Msg("request")
		return
	case out.Handled:
		s.writeReply(w, out.Reply)
		log.Int("status", out.Reply.Status).Dur("dur", time.Since(started)).Msg("request")
		return
	}

	// Stateless endpoints: declaration order wins, no specificity scoring.
	for _, ep := range snap.Endpoints {
		params, ok := ep.Matcher.Match(mreq)
		if !ok {
			continue
		}
		ctx := &tmpl.Context{Request: *engine.NewRequestCtx(mreq, params)}
		rendered, err := ep.Reply.Render(ctx)
		if err != nil {
			writeJSON(w, 500, map[string]any{"error": "template_error", "endpoint": ep.ID, "detail": err.Error()})
			s.logger.Error().Str("endpoint", ep.ID).Err(err).Msg("template render failed")
			return
		}
		s.writeReply(w, rendered)
		log.Str("endpoint", ep.ID).Int("status", rendered.Status).Dur("dur", time.Since(started)).Msg("request")
		return
	}

	s.write404(w, snap, mreq, &out)
	log.Int("status", 404).Dur("dur", time.Since(started)).Msg("request (no match)")
}

func (s *Server) writeReply(w http.ResponseWriter, rep *snapshot.Rendered) {
	if rep.Latency > 0 {
		time.Sleep(rep.Latency)
	}
	if rep.CloseConn {
		// Chaos mode: status 0 drops the connection without a response.
		hj, ok := w.(http.Hijacker)
		if ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		// Fallback for transports that cannot hijack.
		panic(http.ErrAbortHandler)
	}
	hasCT := false
	for k, v := range rep.Headers {
		w.Header().Set(k, v)
		if http.CanonicalHeaderKey(k) == "Content-Type" {
			hasCT = true
		}
	}
	if !hasCT && looksLikeJSON(rep.Body) {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(rep.Status)
	_, _ = w.Write(rep.Body)
}

func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// write404 builds the diagnostic body: what came in plus the closest
// matchers and why each one rejected the request. This is a deliberate DX
// feature (spec 4.2 item 5).
func (s *Server) write404(w http.ResponseWriter, snap *snapshot.Snapshot, mreq *match.Request, out *engine.Outcome) {
	type candidate struct {
		Endpoint string `json:"endpoint,omitempty"`
		Scenario string `json:"scenario,omitempty"`
		State    string `json:"state,omitempty"`
		Method   string `json:"method"`
		Path     string `json:"path"`
		Reason   string `json:"reason"`
	}
	var candidates []candidate

	if out.InSession {
		sc := snap.ScenarioByID[out.ScenarioID]
		if sess := s.engine.Store.GetByID(out.SessionID); sess != nil {
			sc = sess.Scenario
		}
		if sc != nil {
			if st, ok := sc.States[out.State]; ok {
				for _, rule := range st.On {
					candidates = append(candidates, candidate{
						Scenario: out.ScenarioID,
						State:    out.State,
						Method:   rule.Matcher.Method,
						Path:     rule.Matcher.PathSpec,
						Reason:   rule.Matcher.Explain(mreq),
					})
				}
			}
		}
	}
	for _, ep := range snap.Endpoints {
		if len(candidates) >= 10 {
			break
		}
		candidates = append(candidates, candidate{
			Endpoint: ep.ID,
			Method:   ep.Matcher.Method,
			Path:     ep.Matcher.PathSpec,
			Reason:   ep.Matcher.Explain(mreq),
		})
	}

	body := map[string]any{
		"error":  "no_match",
		"detail": "no endpoint or scenario rule matched this request",
		"method": mreq.Method,
		"path":   mreq.Path,
	}
	if out.InSession {
		body["session"] = map[string]any{
			"id":       out.SessionID,
			"scenario": out.ScenarioID,
			"state":    out.State,
			"note":     "request matched an active session but no rule of the current state; it then fell through to stateless endpoints",
		}
	}
	if len(candidates) > 0 {
		body["candidates"] = candidates
	}
	writeJSON(w, 404, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
