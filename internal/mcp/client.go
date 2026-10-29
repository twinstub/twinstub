package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client talks to one engine's admin API. The embedded and the external
// engine look the same from here: a base URL and an optional token.
type client struct {
	base  string // e.g. http://127.0.0.1:9090
	token string
	http  *http.Client
}

func newClient(base, token string, timeout time.Duration) *client {
	return &client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: timeout},
	}
}

// unreachableError means nothing answered at the admin URL.
type unreachableError struct {
	url string
	err error
}

func (e *unreachableError) Error() string {
	return fmt.Sprintf("engine not reachable at %s: %v", e.url, e.err)
}

// apiError is a non-2xx answer from the admin API. Body keeps the decoded
// JSON so tools can turn known error codes into a next step.
type apiError struct {
	status int
	code   string
	detail string
	body   map[string]any
}

func (e *apiError) Error() string {
	if e.detail != "" {
		return e.detail
	}
	if e.code != "" {
		msg := strings.ReplaceAll(e.code, "_", " ")
		if id, ok := e.body["id"].(string); ok && id != "" {
			msg += ": " + id
		}
		return msg
	}
	return fmt.Sprintf("admin API answered HTTP %d", e.status)
}

// do sends a request and decodes the JSON answer into a map. A 2xx answer
// returns the map; anything else returns an *apiError.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body any) (map[string]any, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &unreachableError{url: c.base, err: unwrapURLError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &unreachableError{url: c.base, err: err}
	}
	out := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("%s did not answer like a TwinStub admin API (HTTP %d, not JSON); check the admin URL", c.base, resp.StatusCode)
		}
	}
	if resp.StatusCode/100 != 2 {
		e := &apiError{status: resp.StatusCode, body: out}
		e.code, _ = out["error"].(string)
		e.detail, _ = out["detail"].(string)
		return nil, e
	}
	return out, nil
}

// health probes the engine and returns its /health answer.
func (c *client) health(ctx context.Context) (map[string]any, error) {
	return c.do(ctx, http.MethodGet, "/health", nil, nil)
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
