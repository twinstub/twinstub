// Package tmpl wraps Go text/template with the render context and the fixed
// v1.0 function set from the spec (section 6).
package tmpl

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	mathrand "math/rand/v2"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/twinstub/twinstub/internal/xtime"
)

// Context is the data visible to every template.
type Context struct {
	Request RequestCtx
	Session *SessionCtx // nil outside scenarios
}

type RequestCtx struct {
	Method  string
	Path    string
	Params  map[string]string
	Query   map[string]string
	Headers map[string]string
	Body    any
	RawBody string
}

type SessionCtx struct {
	ID       string
	Key      string
	Scenario string
	State    string
	Vars     map[string]string
}

// Engine owns the function map and the (optionally seeded) random source so
// that --seed makes uuid/randInt/randString reproducible.
type Engine struct {
	mu  sync.Mutex
	rng *mathrand.Rand
}

// New creates an engine. When seeded is false the source is initialized
// from crypto/rand.
func New(seed uint64, seeded bool) *Engine {
	var s1, s2 uint64
	if seeded {
		s1, s2 = seed, seed
	} else {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		s1 = binary.LittleEndian.Uint64(b[0:8])
		s2 = binary.LittleEndian.Uint64(b[8:16])
	}
	return &Engine{rng: mathrand.New(mathrand.NewPCG(s1, s2))}
}

func (e *Engine) randBytes(p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range p {
		p[i] = byte(e.rng.UintN(256))
	}
}

func (e *Engine) intN(n int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rng.IntN(n)
}

// ULID generates a session or delivery id.
func (e *Engine) ULID() string {
	var entropy [10]byte
	e.randBytes(entropy[:])
	ms := ulid.Timestamp(time.Now())
	id, err := ulid.New(ms, bytes.NewReader(entropy[:]))
	if err != nil {
		panic(err)
	}
	return id.String()
}

const randAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func (e *Engine) funcs() template.FuncMap {
	return template.FuncMap{
		"uuid": func() string {
			var b [16]byte
			e.randBytes(b[:])
			b[6] = (b[6] & 0x0f) | 0x40 // version 4
			b[8] = (b[8] & 0x3f) | 0x80 // variant 10
			return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		},
		"ulid": e.ULID,
		"now": func() string {
			return time.Now().UTC().Format(time.RFC3339)
		},
		"nowUnix": func() int64 {
			return time.Now().Unix()
		},
		"addTime": func(d string) (string, error) {
			dur, err := xtime.ParseDuration(d)
			if err != nil {
				return "", err
			}
			return time.Now().UTC().Add(dur).Format(time.RFC3339), nil
		},
		"randInt": func(min, max int) (int, error) {
			if max < min {
				return 0, fmt.Errorf("randInt: max %d < min %d", max, min)
			}
			return min + e.intN(max-min+1), nil
		},
		"randString": func(n int) (string, error) {
			if n < 0 {
				return "", fmt.Errorf("randString: negative length %d", n)
			}
			b := make([]byte, n)
			for i := range b {
				b[i] = randAlphabet[e.intN(len(randAlphabet))]
			}
			return string(b), nil
		},
		"upper": strings.ToUpper,
		"lower": strings.ToLower,
		"json": func(v any) (string, error) {
			out, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

// Parse compiles a template. The name shows up in render error messages, so
// callers pass something identifying like "endpoints[0].reply.body".
func (e *Engine) Parse(name, text string) (*template.Template, error) {
	t, err := template.New(name).Funcs(e.funcs()).Option("missingkey=zero").Parse(text)
	if err != nil {
		// Strip the "template: name:" prefix, callers add their own context.
		return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "template: "))
	}
	return t, nil
}

// MaxRenderSize guards against runaway template output (spec hard limit).
const MaxRenderSize = 5 << 20

// Render executes a parsed template with the output size cap.
func Render(t *template.Template, ctx *Context) ([]byte, error) {
	var buf limitedBuffer
	if err := t.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "template: "))
	}
	return buf.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxRenderSize {
		return 0, fmt.Errorf("rendered output exceeds the %d MB limit", MaxRenderSize>>20)
	}
	return b.Buffer.Write(p)
}
