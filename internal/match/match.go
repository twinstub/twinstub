// Package match compiles and evaluates when-clauses from the DSL.
// It is config-agnostic: the caller converts DSL structs into Spec.
package match

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Spec is the matcher input, mirroring the when-clause of the DSL.
type Spec struct {
	Method  string
	Path    string
	Query   map[string]string
	Headers map[string]string
	Body    []BodyCond
}

// BodyCond ops: equals, exists, matches, gt, lt.
type BodyCond struct {
	Path string
	Op   string
	Val  any
}

// Request is the normalized incoming request the matcher evaluates against.
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Headers http.Header
	Body    any    // decoded JSON or nil
	RawBody []byte // raw bytes, always set
}

type segment struct {
	literal string
	param   string // non-empty for {name} segments
}

type valueMatcher struct {
	key   string
	exact string
	re    *regexp.Regexp
}

func (v *valueMatcher) match(s string) bool {
	if v.re != nil {
		return v.re.MatchString(s)
	}
	return s == v.exact
}

func (v *valueMatcher) want() string {
	if v.re != nil {
		return "re:" + v.re.String()
	}
	return v.exact
}

type bodyCond struct {
	expr   string
	tokens []pathToken
	op     string
	val    any
	re     *regexp.Regexp
	num    float64
}

// Matcher is a compiled when-clause.
type Matcher struct {
	Method   string
	PathSpec string
	segments []segment
	query    []valueMatcher
	headers  []valueMatcher
	body     []bodyCond
}

// Compile validates and compiles a Spec. Errors mention the offending field.
func Compile(s Spec) (*Matcher, error) {
	m := &Matcher{Method: strings.ToUpper(s.Method), PathSpec: s.Path}

	segs, err := compilePath(s.Path)
	if err != nil {
		return nil, err
	}
	m.segments = segs

	m.query, err = compileValues(s.Query, "query")
	if err != nil {
		return nil, err
	}
	m.headers, err = compileValues(s.Headers, "headers")
	if err != nil {
		return nil, err
	}

	for _, c := range s.Body {
		cc := bodyCond{expr: c.Path, op: c.Op, val: c.Val}
		cc.tokens, err = parseJSONPath(c.Path)
		if err != nil {
			return nil, err
		}
		switch c.Op {
		case "equals", "exists":
		case "matches":
			pat, ok := c.Val.(string)
			if !ok {
				return nil, fmt.Errorf("body condition %q: matches needs a string pattern", c.Path)
			}
			cc.re, err = regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("body condition %q: bad regex: %v", c.Path, err)
			}
		case "gt", "lt":
			n, ok := toFloat(c.Val)
			if !ok {
				return nil, fmt.Errorf("body condition %q: %s needs a number", c.Path, c.Op)
			}
			cc.num = n
		default:
			return nil, fmt.Errorf("body condition %q: unknown operator %q", c.Path, c.Op)
		}
		m.body = append(m.body, cc)
	}
	return m, nil
}

var paramRe = regexp.MustCompile(`^\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

func compilePath(path string) ([]segment, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("path must start with / (got %q)", path)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if path == "/" {
		return nil, nil
	}
	segs := make([]segment, 0, len(parts))
	for _, p := range parts {
		if m := paramRe.FindStringSubmatch(p); m != nil {
			segs = append(segs, segment{param: m[1]})
		} else if strings.ContainsAny(p, "{}") {
			return nil, fmt.Errorf("bad path segment %q, parameters look like {id}", p)
		} else {
			segs = append(segs, segment{literal: p})
		}
	}
	return segs, nil
}

func compileValues(src map[string]string, kind string) ([]valueMatcher, error) {
	out := make([]valueMatcher, 0, len(src))
	for k, v := range src {
		vm := valueMatcher{key: k}
		if pat, ok := strings.CutPrefix(v, "re:"); ok {
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("%s %q: bad regex %q: %v", kind, k, pat, err)
			}
			vm.re = re
		} else {
			vm.exact = v
		}
		out = append(out, vm)
	}
	// Deterministic evaluation order keeps Explain output stable.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].key < out[j-1].key; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// MatchPath checks only the path pattern and extracts params. Used by
// session key selectors of kind path:{param}.
func (m *Matcher) MatchPath(path string) (map[string]string, bool) {
	parts := splitPath(path)
	if len(parts) != len(m.segments) {
		return nil, false
	}
	var params map[string]string
	for i, seg := range m.segments {
		if seg.param != "" {
			if params == nil {
				params = map[string]string{}
			}
			params[seg.param] = parts[i]
		} else if seg.literal != parts[i] {
			return nil, false
		}
	}
	if params == nil {
		params = map[string]string{}
	}
	return params, true
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// Match evaluates the full when-clause. Params are returned on success.
func (m *Matcher) Match(r *Request) (map[string]string, bool) {
	params, reason := m.evaluate(r)
	return params, reason == ""
}

// Explain returns "" on match or a short human readable reason for the
// first failing check. Powers the 404 diagnostics.
func (m *Matcher) Explain(r *Request) string {
	_, reason := m.evaluate(r)
	return reason
}

func (m *Matcher) evaluate(r *Request) (map[string]string, string) {
	if !strings.EqualFold(r.Method, m.Method) {
		return nil, fmt.Sprintf("method is %s, expected %s", r.Method, m.Method)
	}
	params, ok := m.MatchPath(r.Path)
	if !ok {
		return nil, fmt.Sprintf("path %s does not match pattern %s", r.Path, m.PathSpec)
	}
	for _, q := range m.query {
		got := ""
		if r.Query != nil {
			got = r.Query.Get(q.key)
		}
		if !q.match(got) {
			return nil, fmt.Sprintf("query param %q is %q, expected %q", q.key, got, q.want())
		}
	}
	for _, h := range m.headers {
		got := ""
		if r.Headers != nil {
			got = r.Headers.Get(h.key)
		}
		if !h.match(got) {
			return nil, fmt.Sprintf("header %q is %q, expected %q", h.key, got, h.want())
		}
	}
	for _, c := range m.body {
		if reason := c.check(r.Body); reason != "" {
			return nil, reason
		}
	}
	return params, ""
}

func (c *bodyCond) check(body any) string {
	v, found := evalJSONPath(body, c.tokens)
	switch c.op {
	case "exists":
		want := c.val == true
		if found != want {
			return fmt.Sprintf("body %s: exists is %v, expected %v", c.expr, found, want)
		}
		return ""
	case "equals":
		if !found {
			return fmt.Sprintf("body %s not found", c.expr)
		}
		if !looseEqual(v, c.val) {
			return fmt.Sprintf("body %s is %v, expected %v", c.expr, v, c.val)
		}
		return ""
	case "matches":
		if !found {
			return fmt.Sprintf("body %s not found", c.expr)
		}
		s := stringify(v)
		if !c.re.MatchString(s) {
			return fmt.Sprintf("body %s is %q, expected to match %q", c.expr, s, c.re.String())
		}
		return ""
	case "gt", "lt":
		if !found {
			return fmt.Sprintf("body %s not found", c.expr)
		}
		n, ok := toFloat(v)
		if !ok {
			return fmt.Sprintf("body %s is not a number (%v)", c.expr, v)
		}
		if c.op == "gt" && !(n > c.num) {
			return fmt.Sprintf("body %s is %v, expected > %v", c.expr, n, c.num)
		}
		if c.op == "lt" && !(n < c.num) {
			return fmt.Sprintf("body %s is %v, expected < %v", c.expr, n, c.num)
		}
		return ""
	}
	return "unknown operator " + c.op
}

// looseEqual compares scalars the way a YAML author expects: numbers
// compare numerically regardless of int/float representation.
func looseEqual(a, b any) bool {
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			return as == bs
		}
	}
	if an, ok := toFloat(a); ok {
		if bn, ok := toFloat(b); ok {
			return an == bn
		}
	}
	return stringify(a) == stringify(b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func stringify(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%v", v)
	}
}
