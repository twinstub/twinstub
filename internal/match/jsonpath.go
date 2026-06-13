package match

import (
	"fmt"
	"strconv"
	"strings"
)

// The DSL needs only a tiny JSONPath subset (dot notation plus array
// indices), so a hand rolled parser avoids a heavyweight dependency.
//
// Supported: $.field, $.a.b, $.items[0].name, $[2]

type pathToken struct {
	key   string
	index int
	isKey bool
}

func parseJSONPath(expr string) ([]pathToken, error) {
	if !strings.HasPrefix(expr, "$") {
		return nil, fmt.Errorf("JSONPath must start with $ (got %q)", expr)
	}
	rest := expr[1:]
	var tokens []pathToken
	for len(rest) > 0 {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			end := strings.IndexAny(rest, ".[")
			if end == -1 {
				end = len(rest)
			}
			if end == 0 {
				return nil, fmt.Errorf("empty field name in JSONPath %q", expr)
			}
			tokens = append(tokens, pathToken{key: rest[:end], isKey: true})
			rest = rest[end:]
		case '[':
			close := strings.IndexByte(rest, ']')
			if close == -1 {
				return nil, fmt.Errorf("unclosed [ in JSONPath %q", expr)
			}
			idx, err := strconv.Atoi(rest[1:close])
			if err != nil || idx < 0 {
				return nil, fmt.Errorf("array index must be a non-negative integer in JSONPath %q", expr)
			}
			tokens = append(tokens, pathToken{index: idx})
			rest = rest[close+1:]
		default:
			return nil, fmt.Errorf("unexpected character %q in JSONPath %q", rest[0], expr)
		}
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("JSONPath %q selects nothing, expected $.field", expr)
	}
	return tokens, nil
}

// evalJSONPath walks a decoded JSON value. Returns the value and whether the
// path resolved.
func evalJSONPath(v any, tokens []pathToken) (any, bool) {
	cur := v
	for _, t := range tokens {
		if t.isKey {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = m[t.key]
			if !ok {
				return nil, false
			}
		} else {
			s, ok := cur.([]any)
			if !ok || t.index >= len(s) {
				return nil, false
			}
			cur = s[t.index]
		}
	}
	return cur, true
}

// EvalPath is the exported helper used by session key selectors.
func EvalPath(body any, expr string) (any, bool, error) {
	tokens, err := parseJSONPath(expr)
	if err != nil {
		return nil, false, err
	}
	v, ok := evalJSONPath(body, tokens)
	return v, ok, nil
}
