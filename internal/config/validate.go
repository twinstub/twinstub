package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	selectorRe  = regexp.MustCompile(`^(body|header|query|path):(.+)$`)
	pathParamRe = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\}$`)
)

// validate runs structural and referential checks. Matcher regexes, JSONPath
// expressions and templates are compiled (and thus validated) by the
// snapshot package on top of this.
func validate(cfg *Config) ([]Warning, ValidationErrors) {
	var errs ValidationErrors
	var warns []Warning

	add := func(src Source, sub, msg string) {
		p := src.Path
		if sub != "" {
			p += "." + sub
		}
		errs = append(errs, ValidationError{File: src.File, Path: p, Message: msg})
	}

	validateTargets(cfg, &errs)
	validateLimits(cfg, &errs)

	endpointIDs := map[string]string{}
	for _, e := range cfg.Endpoints {
		if e.ID == "" {
			add(e.Source, "id", "endpoint id is required")
		} else if prev, dup := endpointIDs[e.ID]; dup {
			add(e.Source, "id", fmt.Sprintf("duplicate endpoint id %q (already defined in %s)", e.ID, prev))
		} else {
			endpointIDs[e.ID] = e.Source.File
		}
		validateWhen(&e.When, e.Source, "when", add)
		validateReply(e.Reply, e.Source, "reply", add)
	}

	scenarioIDs := map[string]string{}
	for _, s := range cfg.Scenarios {
		validateScenario(cfg, s, scenarioIDs, add, &warns)
	}

	return warns, errs
}

func validateTargets(cfg *Config, errs *ValidationErrors) {
	for name, t := range cfg.Targets {
		src := Source{Path: "targets." + name}
		if t == nil || t.URL == "" {
			*errs = append(*errs, ValidationError{Path: src.Path + ".url", Message: "target url is required"})
			continue
		}
		if !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") {
			*errs = append(*errs, ValidationError{Path: src.Path + ".url", Message: fmt.Sprintf("target url must start with http:// or https:// (got %q)", t.URL)})
		}
		if t.Signing != nil {
			if t.Signing.Algorithm != "hmac-sha256" {
				*errs = append(*errs, ValidationError{Path: src.Path + ".signing.algorithm", Message: fmt.Sprintf("only \"hmac-sha256\" is supported (got %q)", t.Signing.Algorithm)})
			}
			if t.Signing.Secret == "" {
				*errs = append(*errs, ValidationError{Path: src.Path + ".signing.secret", Message: "signing secret is required when signing is configured"})
			}
			if !strings.Contains(t.Signing.Format, "{signature}") {
				*errs = append(*errs, ValidationError{Path: src.Path + ".signing.format", Message: "signature format must contain the {signature} placeholder"})
			}
		}
		if t.Retry.Attempts < 1 {
			*errs = append(*errs, ValidationError{Path: src.Path + ".retry.attempts", Message: "retry attempts must be at least 1"})
		}
		if t.Retry.Backoff < 0 {
			*errs = append(*errs, ValidationError{Path: src.Path + ".retry.backoff", Message: "retry backoff cannot be negative"})
		}
	}
}

func validateLimits(cfg *Config, errs *ValidationErrors) {
	l := cfg.Limits
	if l.MaxPendingPerSession > HardMaxPendingPerSession {
		*errs = append(*errs, ValidationError{
			Path:    "limits.max_pending_webhooks_per_session",
			Message: fmt.Sprintf("cannot exceed the hard maximum of %d", HardMaxPendingPerSession),
		})
	}
	for path, v := range map[string]int{
		"limits.max_sessions":                     l.MaxSessions,
		"limits.max_pending_webhooks_per_session": l.MaxPendingPerSession,
		"limits.delivery_log_size":                l.DeliveryLogSize,
		"limits.webhook_workers":                  l.WebhookWorkers,
	} {
		if v < 0 {
			*errs = append(*errs, ValidationError{Path: path, Message: "must be a positive number"})
		}
	}
}

func validateWhen(w *When, src Source, sub string, add func(Source, string, string)) {
	if w.Method == "" {
		add(src, sub+".method", "method is required (GET, POST, ...)")
	}
	if w.Path == "" {
		add(src, sub+".path", "path is required")
	} else if !strings.HasPrefix(w.Path, "/") {
		add(src, sub+".path", fmt.Sprintf("path must start with / (got %q)", w.Path))
	}
	for i, c := range w.Body {
		p := fmt.Sprintf("%s.body[%d]", sub, i)
		if !strings.HasPrefix(c.Path, "$") {
			add(src, p+".path", fmt.Sprintf("JSONPath must start with $ (got %q)", c.Path))
		}
		ops := 0
		if c.Equals != nil {
			ops++
		}
		if c.Exists != nil {
			ops++
		}
		if c.Matches != nil {
			ops++
		}
		if c.GT != nil {
			ops++
		}
		if c.LT != nil {
			ops++
		}
		if ops != 1 {
			add(src, p, fmt.Sprintf("exactly one operator is required (equals, exists, matches, gt, lt), got %d", ops))
		}
	}
}

func validateReply(r *Reply, src Source, sub string, add func(Source, string, string)) {
	if r == nil {
		add(src, sub, "reply is required")
		return
	}
	if r.Status != nil && (*r.Status < 0 || *r.Status > 599) {
		add(src, sub+".status", fmt.Sprintf("status must be 0 (close connection) or 100..599 (got %d)", *r.Status))
	}
	if len(r.Body) > HardMaxBodySize {
		add(src, sub+".body", fmt.Sprintf("body exceeds the %d MB hard limit", HardMaxBodySize>>20))
	}
}

func validateScenario(cfg *Config, s *Scenario, seen map[string]string, add func(Source, string, string), warns *[]Warning) {
	if s.ID == "" {
		add(s.Source, "id", "scenario id is required")
	} else if prev, dup := seen[s.ID]; dup {
		add(s.Source, "id", fmt.Sprintf("duplicate scenario id %q (already defined in %s)", s.ID, prev))
	} else {
		seen[s.ID] = s.Source.File
	}

	if s.Session.Key == "" {
		add(s.Source, "session.key", "session key selector is required (body:$.field, header:X-Name, query:param, path:{param})")
	} else if m := selectorRe.FindStringSubmatch(s.Session.Key); m == nil {
		add(s.Source, "session.key", fmt.Sprintf("invalid selector %q, expected body:$.field, header:X-Name, query:param or path:{param}", s.Session.Key))
	} else {
		switch m[1] {
		case "body":
			if !strings.HasPrefix(m[2], "$") {
				add(s.Source, "session.key", fmt.Sprintf("body selector must use JSONPath starting with $ (got %q)", m[2]))
			}
		case "path":
			if !pathParamRe.MatchString(m[2]) {
				add(s.Source, "session.key", fmt.Sprintf("path selector must reference a path parameter like path:{order_id} (got %q)", m[2]))
			}
		}
	}

	ttl := time.Duration(s.Session.TTL)
	if ttl < 0 || ttl > HardMaxSessionTTL {
		add(s.Source, "session.ttl", fmt.Sprintf("session ttl must be between 0 and 24h (got %s)", ttl))
	}

	if len(s.States) == 0 {
		add(s.Source, "states", "scenario must define at least one state")
		return
	}
	if len(s.States) > HardMaxStates {
		add(s.Source, "states", fmt.Sprintf("scenario has %d states, the hard limit is %d", len(s.States), HardMaxStates))
	}
	if s.Initial == "" {
		add(s.Source, "initial", "initial state is required")
	} else if _, ok := s.States[s.Initial]; !ok {
		add(s.Source, "initial", fmt.Sprintf("initial state %q is not defined in states (known: %s)", s.Initial, stateNames(s)))
	}

	for name, st := range s.States {
		sp := "states." + name
		if st == nil {
			add(s.Source, sp, "state must be a mapping with enter and/or on")
			continue
		}
		for i, a := range st.Enter {
			ap := fmt.Sprintf("%s.enter[%d]", sp, i)
			if a.Webhook == nil {
				add(s.Source, ap, "v1.0 supports exactly one action type: webhook")
				continue
			}
			if a.Webhook.Target == "" {
				add(s.Source, ap+".webhook.target", "webhook target is required")
			} else if _, ok := cfg.Targets[a.Webhook.Target]; !ok {
				add(s.Source, ap+".webhook.target", fmt.Sprintf("unknown target %q (declare it under targets: in the root config)", a.Webhook.Target))
			}
			after := time.Duration(a.Webhook.After)
			if after < 0 || after > HardMaxAfter {
				add(s.Source, ap+".webhook.after", fmt.Sprintf("after must be between 0 and 7d (got %s)", after))
			}
			if len(a.Webhook.Body) > HardMaxBodySize {
				add(s.Source, ap+".webhook.body", fmt.Sprintf("body exceeds the %d MB hard limit", HardMaxBodySize>>20))
			}
			if a.Goto != "" {
				if _, ok := s.States[a.Goto]; !ok {
					add(s.Source, ap+".goto", fmt.Sprintf("goto points to unknown state %q (known: %s)", a.Goto, stateNames(s)))
				}
			}
		}
		for i, r := range st.On {
			rp := fmt.Sprintf("%s.on[%d]", sp, i)
			validateWhen(&r.When, s.Source, rp+".when", add)
			validateReply(r.Reply, s.Source, rp+".reply", add)
			if r.Goto != "" {
				if _, ok := s.States[r.Goto]; !ok {
					add(s.Source, rp+".goto", fmt.Sprintf("goto points to unknown state %q (known: %s)", r.Goto, stateNames(s)))
				}
			}
		}
	}

	for _, name := range unreachableStates(s) {
		*warns = append(*warns, Warning{
			File:    s.Source.File,
			Path:    s.Source.Path + ".states." + name,
			Message: fmt.Sprintf("state %q is unreachable from initial state %q", name, s.Initial),
		})
	}
}

func stateNames(s *Scenario) string {
	names := make([]string, 0, len(s.States))
	for n := range s.States {
		names = append(names, n)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

func unreachableStates(s *Scenario) []string {
	if _, ok := s.States[s.Initial]; !ok {
		return nil
	}
	visited := map[string]bool{}
	queue := []string{s.Initial}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		st := s.States[cur]
		if st == nil {
			continue
		}
		for _, a := range st.Enter {
			if a.Goto != "" && !visited[a.Goto] {
				queue = append(queue, a.Goto)
			}
		}
		for _, r := range st.On {
			if r.Goto != "" && !visited[r.Goto] {
				queue = append(queue, r.Goto)
			}
		}
	}
	var out []string
	for name := range s.States {
		if !visited[name] {
			out = append(out, name)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
