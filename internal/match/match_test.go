package match

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func req(method, path string, opts ...func(*Request)) *Request {
	u, _ := url.Parse(path)
	r := &Request{
		Method:  method,
		Path:    u.Path,
		Query:   u.Query(),
		Headers: http.Header{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

func withHeader(k, v string) func(*Request) {
	return func(r *Request) { r.Headers.Set(k, v) }
}

func withJSON(body any) func(*Request) {
	return func(r *Request) { r.Body = body }
}

func mustCompile(t *testing.T, s Spec) *Matcher {
	t.Helper()
	m, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return m
}

func TestPathParams(t *testing.T) {
	m := mustCompile(t, Spec{Method: "GET", Path: "/v1/payments/{id}"})
	params, ok := m.Match(req("GET", "/v1/payments/pay_123"))
	if !ok {
		t.Fatal("expected match")
	}
	if params["id"] != "pay_123" {
		t.Errorf("param id = %q", params["id"])
	}
	if _, ok := m.Match(req("GET", "/v1/payments")); ok {
		t.Error("matched wrong segment count")
	}
	if _, ok := m.Match(req("GET", "/v1/refunds/pay_123")); ok {
		t.Error("matched wrong literal segment")
	}
	if _, ok := m.Match(req("POST", "/v1/payments/pay_123")); ok {
		t.Error("matched wrong method")
	}
}

func TestRootPath(t *testing.T) {
	m := mustCompile(t, Spec{Method: "GET", Path: "/"})
	if _, ok := m.Match(req("GET", "/")); !ok {
		t.Error("expected / to match")
	}
	if _, ok := m.Match(req("GET", "/x")); ok {
		t.Error("/x should not match /")
	}
}

func TestQueryExactAndRegex(t *testing.T) {
	m := mustCompile(t, Spec{
		Method: "GET", Path: "/v1/rates",
		Query: map[string]string{"currency": "USD"},
	})
	if _, ok := m.Match(req("GET", "/v1/rates?currency=USD")); !ok {
		t.Error("exact query should match")
	}
	if _, ok := m.Match(req("GET", "/v1/rates?currency=EUR")); ok {
		t.Error("wrong query value matched")
	}
	if _, ok := m.Match(req("GET", "/v1/rates")); ok {
		t.Error("missing query param matched")
	}

	m = mustCompile(t, Spec{
		Method: "GET", Path: "/v1/rates",
		Query: map[string]string{"currency": "re:^(USD|EUR)$"},
	})
	if _, ok := m.Match(req("GET", "/v1/rates?currency=EUR")); !ok {
		t.Error("regex query should match")
	}
	if _, ok := m.Match(req("GET", "/v1/rates?currency=GBP")); ok {
		t.Error("regex query should not match GBP")
	}
}

func TestHeaderRegex(t *testing.T) {
	m := mustCompile(t, Spec{
		Method: "GET", Path: "/x",
		Headers: map[string]string{"X-Api-Key": "re:^key_.+"},
	})
	if _, ok := m.Match(req("GET", "/x", withHeader("X-Api-Key", "key_abc"))); !ok {
		t.Error("regex header should match")
	}
	if _, ok := m.Match(req("GET", "/x", withHeader("X-Api-Key", "nope"))); ok {
		t.Error("regex header should not match")
	}
	if _, ok := m.Match(req("GET", "/x")); ok {
		t.Error("missing header matched")
	}
}

func TestBodyOperators(t *testing.T) {
	body := map[string]any{
		"amount":   float64(150000),
		"currency": "EUR",
		"card":     map[string]any{"last4": "4242"},
		"items":    []any{map[string]any{"sku": "a1"}},
	}

	cases := []struct {
		name string
		cond BodyCond
		want bool
	}{
		{"gt true", BodyCond{Path: "$.amount", Op: "gt", Val: 100000}, true},
		{"gt false", BodyCond{Path: "$.amount", Op: "gt", Val: 200000}, false},
		{"lt true", BodyCond{Path: "$.amount", Op: "lt", Val: 200000}, true},
		{"equals string", BodyCond{Path: "$.currency", Op: "equals", Val: "EUR"}, true},
		{"equals number int vs float", BodyCond{Path: "$.amount", Op: "equals", Val: 150000}, true},
		{"equals nested", BodyCond{Path: "$.card.last4", Op: "equals", Val: "4242"}, true},
		{"equals array index", BodyCond{Path: "$.items[0].sku", Op: "equals", Val: "a1"}, true},
		{"exists true", BodyCond{Path: "$.card", Op: "exists", Val: true}, true},
		{"exists false on missing", BodyCond{Path: "$.missing", Op: "exists", Val: false}, true},
		{"matches", BodyCond{Path: "$.currency", Op: "matches", Val: "^EU"}, true},
		{"matches fail", BodyCond{Path: "$.currency", Op: "matches", Val: "^US"}, false},
		{"missing path gt", BodyCond{Path: "$.nope", Op: "gt", Val: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mustCompile(t, Spec{Method: "POST", Path: "/p", Body: []BodyCond{c.cond}})
			_, ok := m.Match(req("POST", "/p", withJSON(body)))
			if ok != c.want {
				t.Errorf("match = %v, want %v", ok, c.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	cases := []Spec{
		{Method: "GET", Path: "no-slash"},
		{Method: "GET", Path: "/x", Headers: map[string]string{"H": "re:["}},
		{Method: "GET", Path: "/x", Body: []BodyCond{{Path: "amount", Op: "gt", Val: 1}}},
		{Method: "GET", Path: "/x", Body: []BodyCond{{Path: "$.a", Op: "gt", Val: "abc"}}},
		{Method: "GET", Path: "/x", Body: []BodyCond{{Path: "$.a", Op: "matches", Val: "["}}},
		{Method: "GET", Path: "/x", Body: []BodyCond{{Path: "$.a[x]", Op: "exists", Val: true}}},
		{Method: "GET", Path: "/{bad-name}"},
	}
	for i, s := range cases {
		if _, err := Compile(s); err == nil {
			t.Errorf("case %d: expected compile error", i)
		}
	}
}

func TestExplain(t *testing.T) {
	m := mustCompile(t, Spec{Method: "POST", Path: "/v1/payments", Query: map[string]string{"k": "v"}})
	if got := m.Explain(req("GET", "/v1/payments")); !strings.Contains(got, "method") {
		t.Errorf("explain method: %q", got)
	}
	if got := m.Explain(req("POST", "/other")); !strings.Contains(got, "path") {
		t.Errorf("explain path: %q", got)
	}
	if got := m.Explain(req("POST", "/v1/payments")); !strings.Contains(got, "query param") {
		t.Errorf("explain query: %q", got)
	}
	if got := m.Explain(req("POST", "/v1/payments?k=v")); got != "" {
		t.Errorf("expected match, got %q", got)
	}
}

func TestEvalPath(t *testing.T) {
	body := map[string]any{"order": map[string]any{"id": "ord_1"}}
	v, ok, err := EvalPath(body, "$.order.id")
	if err != nil || !ok || v != "ord_1" {
		t.Errorf("EvalPath = %v %v %v", v, ok, err)
	}
	_, ok, _ = EvalPath(body, "$.nope")
	if ok {
		t.Error("missing path should not resolve")
	}
	if _, _, err := EvalPath(body, "order.id"); err == nil {
		t.Error("expected parse error without $")
	}
}
