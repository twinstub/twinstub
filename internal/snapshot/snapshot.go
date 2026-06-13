// Package snapshot compiles a validated config into an immutable, versioned
// runtime snapshot. Hot reload swaps snapshots atomically; sessions pin the
// scenario they started with, so old sessions survive a reload.
package snapshot

import (
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/match"
	"github.com/twinstub/twinstub/internal/tmpl"
)

type Snapshot struct {
	Version        int // increments on every successful reload
	Config         *config.Config
	Endpoints      []*Endpoint
	Scenarios      []*Scenario
	ScenarioByID   map[string]*Scenario
	ScenarioHeader string
	DefaultLatency time.Duration
}

type Endpoint struct {
	ID      string
	Matcher *match.Matcher
	Reply   *Reply
	Source  config.Source
}

type Scenario struct {
	ID          string
	Description string
	KeySelector *KeySelector
	TTL         time.Duration
	Initial     string
	States      map[string]*State
	StateNames  []string
	Source      config.Source
}

type State struct {
	Name  string
	Enter []*Action
	On    []*Rule
}

// Terminal per spec: a state with no enter transitions.
func (s *State) Terminal() bool {
	for _, a := range s.Enter {
		if a.Goto != "" {
			return false
		}
	}
	return len(s.Enter) == 0
}

type Action struct {
	TargetName string
	Target     *config.Target
	Event      string
	After      time.Duration
	Body       *template.Template
	Goto       string
}

type Rule struct {
	Matcher *match.Matcher
	Reply   *Reply
	Set     []SetVar
	Goto    string
}

type SetVar struct {
	Name string
	Tmpl *template.Template
}

type Reply struct {
	Status  int // 0 means close the connection without responding
	Headers []HeaderTmpl
	Latency time.Duration
	Body    *template.Template
}

type HeaderTmpl struct {
	Name string
	Tmpl *template.Template
}

// Rendered is a reply after template execution, ready to be written.
type Rendered struct {
	Status    int
	Headers   map[string]string
	Body      []byte
	Latency   time.Duration
	CloseConn bool
}

// Render executes all templates of a reply against the given context.
func (r *Reply) Render(ctx *tmpl.Context) (*Rendered, error) {
	out := &Rendered{Status: r.Status, Latency: r.Latency, CloseConn: r.Status == 0}
	out.Headers = make(map[string]string, len(r.Headers))
	for _, h := range r.Headers {
		v, err := tmpl.Render(h.Tmpl, ctx)
		if err != nil {
			return nil, err
		}
		out.Headers[h.Name] = string(v)
	}
	if r.Body != nil {
		b, err := tmpl.Render(r.Body, ctx)
		if err != nil {
			return nil, err
		}
		out.Body = b
	}
	return out, nil
}

// Compile builds a snapshot from a structurally valid config. Compilation
// errors (bad regex, bad template syntax, bad JSONPath) are validation
// errors and carry file plus field path.
func Compile(cfg *config.Config, eng *tmpl.Engine, version int) (*Snapshot, error) {
	snap := &Snapshot{
		Version:        version,
		Config:         cfg,
		ScenarioByID:   map[string]*Scenario{},
		ScenarioHeader: cfg.Defaults.ScenarioHeader,
		DefaultLatency: time.Duration(cfg.Defaults.Latency),
	}
	var errs config.ValidationErrors

	for _, e := range cfg.Endpoints {
		ce, err := compileEndpoint(e, eng, snap.DefaultLatency)
		if err != nil {
			errs = append(errs, asValidation(err, e.Source))
			continue
		}
		snap.Endpoints = append(snap.Endpoints, ce)
	}
	for _, s := range cfg.Scenarios {
		cs, serrs := compileScenario(cfg, s, eng, snap.DefaultLatency)
		if len(serrs) > 0 {
			errs = append(errs, serrs...)
			continue
		}
		snap.Scenarios = append(snap.Scenarios, cs)
		snap.ScenarioByID[cs.ID] = cs
	}
	if err := errs.OrNil(); err != nil {
		return nil, err
	}
	return snap, nil
}

type fieldError struct {
	field string
	err   error
}

func (f fieldError) Error() string { return f.field + ": " + f.err.Error() }

func asValidation(err error, src config.Source) config.ValidationError {
	if fe, ok := err.(fieldError); ok {
		return config.ValidationError{File: src.File, Path: src.Path + "." + fe.field, Message: fe.err.Error()}
	}
	return config.ValidationError{File: src.File, Path: src.Path, Message: err.Error()}
}

func compileEndpoint(e *config.Endpoint, eng *tmpl.Engine, defLatency time.Duration) (*Endpoint, error) {
	m, err := compileWhen(&e.When)
	if err != nil {
		return nil, fieldError{"when", err}
	}
	reply, err := compileReply(e.Reply, eng, fmt.Sprintf("endpoint %s reply", e.ID), defLatency)
	if err != nil {
		return nil, fieldError{"reply", err}
	}
	return &Endpoint{ID: e.ID, Matcher: m, Reply: reply, Source: e.Source}, nil
}

func compileWhen(w *config.When) (*match.Matcher, error) {
	spec := match.Spec{
		Method:  w.Method,
		Path:    w.Path,
		Query:   w.Query,
		Headers: w.Headers,
	}
	for _, c := range w.Body {
		bc := match.BodyCond{Path: c.Path}
		switch {
		case c.Equals != nil:
			bc.Op, bc.Val = "equals", c.Equals.Value
		case c.Exists != nil:
			bc.Op, bc.Val = "exists", *c.Exists
		case c.Matches != nil:
			bc.Op, bc.Val = "matches", *c.Matches
		case c.GT != nil:
			bc.Op, bc.Val = "gt", *c.GT
		case c.LT != nil:
			bc.Op, bc.Val = "lt", *c.LT
		}
		spec.Body = append(spec.Body, bc)
	}
	return match.Compile(spec)
}

func compileReply(r *config.Reply, eng *tmpl.Engine, name string, defLatency time.Duration) (*Reply, error) {
	out := &Reply{Status: r.StatusOrDefault(), Latency: defLatency}
	if r.Status != nil && *r.Status == 0 {
		out.Status = 0
	}
	if r.Latency != nil {
		out.Latency = time.Duration(*r.Latency)
	}
	for h, v := range r.Headers {
		t, err := eng.Parse(name+" header "+h, v)
		if err != nil {
			return nil, fmt.Errorf("header %q: %v", h, err)
		}
		out.Headers = append(out.Headers, HeaderTmpl{Name: h, Tmpl: t})
	}
	sortHeaders(out.Headers)
	if r.Body != "" {
		t, err := eng.Parse(name+" body", r.Body)
		if err != nil {
			return nil, fmt.Errorf("body: %v", err)
		}
		out.Body = t
	}
	return out, nil
}

func sortHeaders(hs []HeaderTmpl) {
	for i := 1; i < len(hs); i++ {
		for j := i; j > 0 && hs[j].Name < hs[j-1].Name; j-- {
			hs[j], hs[j-1] = hs[j-1], hs[j]
		}
	}
}

func compileScenario(cfg *config.Config, s *config.Scenario, eng *tmpl.Engine, defLatency time.Duration) (*Scenario, config.ValidationErrors) {
	var errs config.ValidationErrors
	add := func(sub, msg string) {
		errs = append(errs, config.ValidationError{File: s.Source.File, Path: s.Source.Path + "." + sub, Message: msg})
	}

	sel, err := ParseKeySelector(s.Session.Key)
	if err != nil {
		add("session.key", err.Error())
	}

	cs := &Scenario{
		ID:          s.ID,
		Description: s.Description,
		KeySelector: sel,
		TTL:         s.Session.TTLOrDefault(),
		Initial:     s.Initial,
		States:      map[string]*State{},
		Source:      s.Source,
	}

	for name, st := range s.States {
		if st == nil {
			continue
		}
		state := &State{Name: name}
		for i, a := range st.Enter {
			if a.Webhook == nil {
				continue
			}
			t, err := eng.Parse(fmt.Sprintf("scenario %s state %s enter[%d] webhook body", s.ID, name, i), a.Webhook.Body)
			if err != nil {
				add(fmt.Sprintf("states.%s.enter[%d].webhook.body", name, i), err.Error())
				continue
			}
			state.Enter = append(state.Enter, &Action{
				TargetName: a.Webhook.Target,
				Target:     cfg.Targets[a.Webhook.Target],
				Event:      a.Webhook.Event,
				After:      time.Duration(a.Webhook.After),
				Body:       t,
				Goto:       a.Goto,
			})
		}
		for i, r := range st.On {
			m, err := compileWhen(&r.When)
			if err != nil {
				add(fmt.Sprintf("states.%s.on[%d].when", name, i), err.Error())
				continue
			}
			reply, err := compileReply(r.Reply, eng, fmt.Sprintf("scenario %s state %s on[%d] reply", s.ID, name, i), defLatency)
			if err != nil {
				add(fmt.Sprintf("states.%s.on[%d].reply", name, i), err.Error())
				continue
			}
			rule := &Rule{Matcher: m, Reply: reply, Goto: r.Goto}
			for k, v := range r.Set {
				t, err := eng.Parse(fmt.Sprintf("scenario %s state %s on[%d] set %s", s.ID, name, i, k), v)
				if err != nil {
					add(fmt.Sprintf("states.%s.on[%d].set.%s", name, i, k), err.Error())
					continue
				}
				rule.Set = append(rule.Set, SetVar{Name: k, Tmpl: t})
			}
			sortSetVars(rule.Set)
			state.On = append(state.On, rule)
		}
		cs.States[name] = state
		cs.StateNames = append(cs.StateNames, name)
	}
	sortStrings(cs.StateNames)

	if len(errs) > 0 {
		return nil, errs
	}
	return cs, nil
}

func sortSetVars(s []SetVar) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Name < s[j-1].Name; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// KeySelector extracts the value that groups requests into one session.
type KeySelector struct {
	Kind string // body, header, query, path
	Arg  string
}

func (k *KeySelector) String() string { return k.Kind + ":" + k.Arg }

func ParseKeySelector(s string) (*KeySelector, error) {
	kind, arg, ok := strings.Cut(s, ":")
	if !ok || arg == "" {
		return nil, fmt.Errorf("invalid session key selector %q", s)
	}
	switch kind {
	case "body", "header", "query":
	case "path":
		arg = strings.TrimSuffix(strings.TrimPrefix(arg, "{"), "}")
		if arg == "" {
			return nil, fmt.Errorf("invalid path selector %q", s)
		}
	default:
		return nil, fmt.Errorf("unknown session key selector kind %q", kind)
	}
	return &KeySelector{Kind: kind, Arg: arg}, nil
}

// Extract pulls the session key from a request. The scenario is needed for
// path selectors: its rule patterns are tried until one matches the path.
func (k *KeySelector) Extract(r *match.Request, sc *Scenario) (string, bool) {
	switch k.Kind {
	case "header":
		v := r.Headers.Get(k.Arg)
		return v, v != ""
	case "query":
		v := r.Query.Get(k.Arg)
		return v, v != ""
	case "body":
		v, found, err := match.EvalPath(r.Body, k.Arg)
		if err != nil || !found {
			return "", false
		}
		s := fmt.Sprintf("%v", v)
		return s, s != ""
	case "path":
		for _, name := range sc.StateNames {
			for _, rule := range sc.States[name].On {
				if params, ok := rule.Matcher.MatchPath(r.Path); ok {
					if v, ok := params[k.Arg]; ok && v != "" {
						return v, true
					}
				}
			}
		}
		return "", false
	}
	return "", false
}
