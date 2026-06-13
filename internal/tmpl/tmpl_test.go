package tmpl

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, e *Engine, text string, ctx *Context) string {
	t.Helper()
	tpl, err := e.Parse("test", text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out, err := Render(tpl, ctx)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return string(out)
}

func baseCtx() *Context {
	return &Context{
		Request: RequestCtx{
			Method:  "POST",
			Path:    "/v1/payments",
			Params:  map[string]string{"id": "pay_1"},
			Query:   map[string]string{"q": "1"},
			Headers: map[string]string{"X-Api-Key": "key_1"},
			Body:    map[string]any{"amount": float64(1999), "nested": map[string]any{"x": "y"}},
			RawBody: `{"amount":1999}`,
		},
		Session: &SessionCtx{
			ID: "01ABC", Key: "ord_1", Scenario: "payment.chargeback", State: "new",
			Vars: map[string]string{"amount": "1999"},
		},
	}
}

func TestContextFields(t *testing.T) {
	e := New(1, true)
	ctx := baseCtx()
	cases := map[string]string{
		`{{ .Request.Method }}`:                    "POST",
		`{{ .Request.Path }}`:                      "/v1/payments",
		`{{ .Request.Params.id }}`:                 "pay_1",
		`{{ .Request.Query.q }}`:                   "1",
		`{{ index .Request.Headers "X-Api-Key" }}`: "key_1",
		`{{ .Request.Body.amount }}`:               "1999",
		`{{ .Request.Body.nested.x }}`:             "y",
		`{{ .Request.RawBody }}`:                   `{"amount":1999}`,
		`{{ .Session.ID }}`:                        "01ABC",
		`{{ .Session.Key }}`:                       "ord_1",
		`{{ .Session.Scenario }}`:                  "payment.chargeback",
		`{{ .Session.State }}`:                     "new",
		`{{ .Session.Vars.amount }}`:               "1999",
	}
	for text, want := range cases {
		if got := render(t, e, text, ctx); got != want {
			t.Errorf("%s = %q, want %q", text, got, want)
		}
	}
}

func TestFunctions(t *testing.T) {
	e := New(42, true)
	ctx := baseCtx()

	uuid := render(t, e, `{{ uuid }}`, ctx)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(uuid) {
		t.Errorf("uuid format: %q", uuid)
	}
	ul := render(t, e, `{{ ulid }}`, ctx)
	if len(ul) != 26 {
		t.Errorf("ulid length: %q", ul)
	}
	now := render(t, e, `{{ now }}`, ctx)
	if _, err := time.Parse(time.RFC3339, now); err != nil {
		t.Errorf("now is not RFC3339: %q", now)
	}
	added := render(t, e, `{{ addTime "72h" }}`, ctx)
	at, err := time.Parse(time.RFC3339, added)
	if err != nil || time.Until(at) < 71*time.Hour {
		t.Errorf("addTime: %q (%v)", added, err)
	}
	for i := 0; i < 50; i++ {
		v, err := strconv.Atoi(render(t, e, `{{ randInt 5 10 }}`, ctx))
		if err != nil || v < 5 || v > 10 {
			t.Fatalf("randInt out of [5,10]: %d (%v)", v, err)
		}
	}
	rs := render(t, e, `{{ randString 12 }}`, ctx)
	if len(rs) != 12 {
		t.Errorf("randString length: %q", rs)
	}
	if got := render(t, e, `{{ upper "abc" }}{{ lower "DEF" }}`, ctx); got != "ABCdef" {
		t.Errorf("upper/lower: %q", got)
	}
	if got := render(t, e, `{{ json .Request.Body.nested }}`, ctx); got != `{"x":"y"}` {
		t.Errorf("json: %q", got)
	}
	if got := render(t, e, `{{ nowUnix }}`, ctx); len(got) < 10 {
		t.Errorf("nowUnix: %q", got)
	}
}

// Seed determinism is a spec requirement for CI reproducibility.
func TestSeedDeterminism(t *testing.T) {
	ctx := baseCtx()
	text := `{{ uuid }} {{ randInt 0 1000000 }} {{ randString 20 }}`
	a := render(t, New(7, true), text, ctx)
	b := render(t, New(7, true), text, ctx)
	if a != b {
		t.Errorf("same seed produced different output:\n%s\n%s", a, b)
	}
	c := render(t, New(8, true), text, ctx)
	if a == c {
		t.Error("different seeds produced identical output")
	}
}

func TestParseError(t *testing.T) {
	e := New(0, false)
	if _, err := e.Parse("bad", `{{ .Request.Method`); err == nil {
		t.Error("expected parse error")
	}
	if _, err := e.Parse("bad", `{{ unknownFunc }}`); err == nil {
		t.Error("expected unknown function error")
	}
}

func TestRenderError(t *testing.T) {
	e := New(0, false)
	tpl, err := e.Parse("bad", `{{ .Session.ID }}`)
	if err != nil {
		t.Fatal(err)
	}
	// Session is nil outside scenarios: render must fail loudly, not hide.
	if _, err := Render(tpl, &Context{}); err == nil {
		t.Error("expected render error on nil Session")
	}
	tpl2, _ := e.Parse("bad2", `{{ randInt 10 1 }}`)
	if _, err := Render(tpl2, baseCtx()); err == nil {
		t.Error("expected randInt range error")
	}
}

func TestRenderSizeLimit(t *testing.T) {
	e := New(0, false)
	tpl, err := e.Parse("big", `{{ randString 100 }}`)
	if err != nil {
		t.Fatal(err)
	}
	var b limitedBuffer
	huge := strings.Repeat("x", MaxRenderSize+1)
	if _, err := b.Write([]byte(huge)); err == nil {
		t.Error("expected size limit error")
	}
	if _, err := Render(tpl, baseCtx()); err != nil {
		t.Errorf("small render should pass: %v", err)
	}
}
