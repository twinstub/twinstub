package webhook

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/config"
)

func testDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	var n atomic.Int64
	d := NewDispatcher(Options{
		Workers:      2,
		LogSize:      100,
		AllowPrivate: true,
		NewID:        func() string { return fmt.Sprintf("dlv_%d", n.Add(1)) },
		Logger:       zerolog.Nop(),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		d.Stop(ctx)
	})
	return d
}

func target(url string, attempts int, backoff time.Duration) *config.Target {
	return &config.Target{
		URL:     url,
		Headers: map[string]string{"X-Source": "twinstub"},
		Signing: &config.Signing{
			Algorithm: "hmac-sha256",
			Secret:    "whsec_test",
			Header:    "X-TwinStub-Signature",
			Format:    "t={timestamp},v1={signature}",
		},
		Retry:   config.Retry{Attempts: attempts, Backoff: config.Duration(backoff)},
		Timeout: config.Duration(2 * time.Second),
	}
}

func waitStatus(t *testing.T, d *Dispatcher, id, want string) Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := d.Log().Get(id); ok && rec.Status == want {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec, _ := d.Log().Get(id)
	t.Fatalf("delivery %s never reached status %q (last: %+v)", id, want, rec)
	return Record{}
}

func TestDeliveryHeadersAndBody(t *testing.T) {
	var mu sync.Mutex
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{
		SessionID:  "sess_1",
		TargetName: "cb",
		Target:     target(srv.URL, 3, time.Second),
		Event:      "payment.succeeded",
		Body:       []byte(`{"ok":true}`),
	})
	waitStatus(t, d, id, StatusDelivered)

	mu.Lock()
	defer mu.Unlock()
	if body != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
	checks := map[string]string{
		"Content-Type":        "application/json",
		"X-Twinstub-Delivery": id,
		"X-Twinstub-Session":  "sess_1",
		"X-Twinstub-Event":    "payment.succeeded",
		"X-Source":            "twinstub",
	}
	for h, want := range checks {
		if v := got.Header.Get(h); v != want {
			t.Errorf("header %s = %q, want %q", h, v, want)
		}
	}
	if ua := got.Header.Get("User-Agent"); !strings.HasPrefix(ua, "twinstub/") {
		t.Errorf("user agent = %q", ua)
	}
	if sig := got.Header.Get("X-TwinStub-Signature"); !strings.Contains(sig, ",v1=") {
		t.Errorf("signature = %q", sig)
	}
}

func TestRetriesThenSuccess(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{
		TargetName: "cb",
		Target:     target(srv.URL, 5, 10*time.Millisecond),
		Body:       []byte(`{}`),
	})
	rec := waitStatus(t, d, id, StatusDelivered)
	if len(rec.Attempts) != 3 {
		t.Errorf("attempts = %d, want 3", len(rec.Attempts))
	}
	if rec.Attempts[0].StatusCode != 500 || rec.Attempts[2].StatusCode != 204 {
		t.Errorf("attempt codes: %+v", rec.Attempts)
	}
}

func TestFailsAfterMaxAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{
		TargetName: "cb",
		Target:     target(srv.URL, 2, 5*time.Millisecond),
		Body:       []byte(`{}`),
	})
	rec := waitStatus(t, d, id, StatusFailed)
	if len(rec.Attempts) != 2 {
		t.Errorf("attempts = %d, want 2", len(rec.Attempts))
	}
}

// The post-dispatch goto contract: fires once, after attempt 1, regardless
// of outcome.
func TestOnFirstAttemptFiresOnceEvenOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	var fired atomic.Int64
	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{
		TargetName:     "cb",
		Target:         target(srv.URL, 3, 5*time.Millisecond),
		Body:           []byte(`{}`),
		OnFirstAttempt: func() { fired.Add(1) },
	})
	waitStatus(t, d, id, StatusFailed)
	if got := fired.Load(); got != 1 {
		t.Errorf("OnFirstAttempt fired %d times, want 1", got)
	}
}

func TestCancelSession(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{
		SessionID:  "sess_c",
		TargetName: "cb",
		Target:     target(srv.URL, 3, time.Second),
		Body:       []byte(`{}`),
		Delay:      300 * time.Millisecond,
	})
	if n := d.PendingCount("sess_c"); n != 1 {
		t.Errorf("pending = %d, want 1", n)
	}
	d.CancelSession("sess_c")
	waitStatus(t, d, id, StatusCanceled)
	if n := d.PendingCount("sess_c"); n != 0 {
		t.Errorf("pending after cancel = %d", n)
	}
	time.Sleep(400 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("canceled delivery still fired")
	}
}

func TestDelayOrdering(t *testing.T) {
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Header.Get("X-TwinStub-Event"))
		mu.Unlock()
	}))
	defer srv.Close()

	d := testDispatcher(t)
	tg := target(srv.URL, 1, time.Second)
	// Scheduled out of order on purpose; the heap must reorder by fire time.
	second := d.Schedule(ScheduleRequest{TargetName: "cb", Target: tg, Event: "second", Body: []byte(`{}`), Delay: 120 * time.Millisecond})
	first := d.Schedule(ScheduleRequest{TargetName: "cb", Target: tg, Event: "first", Body: []byte(`{}`), Delay: 30 * time.Millisecond})
	waitStatus(t, d, first, StatusDelivered)
	waitStatus(t, d, second, StatusDelivered)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("delivery order = %v", order)
	}
}

func TestRedirectIsFailure(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	defer srv.Close()

	d := testDispatcher(t)
	id := d.Schedule(ScheduleRequest{
		TargetName: "cb",
		Target:     target(srv.URL, 1, time.Second),
		Body:       []byte(`{}`),
	})
	rec := waitStatus(t, d, id, StatusFailed)
	if rec.Attempts[0].StatusCode != 302 {
		t.Errorf("status = %d, want 302 recorded as failure", rec.Attempts[0].StatusCode)
	}
	if followed.Load() {
		t.Error("redirect was followed; spec forbids following webhook redirects")
	}
}

func TestSSRFBlocksPrivateTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("delivery to loopback must be blocked")
	}))
	defer srv.Close()

	var n atomic.Int64
	d := NewDispatcher(Options{
		Workers:      1,
		LogSize:      10,
		AllowPrivate: false,
		NewID:        func() string { return fmt.Sprintf("ssrf_%d", n.Add(1)) },
		Logger:       zerolog.Nop(),
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		d.Stop(ctx)
	}()

	id := d.Schedule(ScheduleRequest{
		TargetName: "cb",
		Target:     target(srv.URL, 1, time.Second),
		Body:       []byte(`{}`),
	})
	rec := waitStatus(t, d, id, StatusFailed)
	if !strings.Contains(rec.Attempts[0].Error, "blocked") {
		t.Errorf("expected SSRF block error, got %q", rec.Attempts[0].Error)
	}
}

func TestForbiddenIPs(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,
		"10.0.0.5":        true,
		"172.16.1.1":      true,
		"192.168.1.1":     true,
		"169.254.169.254": true, // cloud metadata
		"::1":             true,
		"0.0.0.0":         true,
		"8.8.8.8":         false,
		"93.184.216.34":   false,
	}
	for ip, want := range cases {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			t.Fatalf("bad ip %s", ip)
		}
		if got := isForbidden(parsed); got != want {
			t.Errorf("isForbidden(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestDeliveryLogRingBuffer(t *testing.T) {
	l := NewDeliveryLog(3)
	for i := 1; i <= 5; i++ {
		l.Add(&Record{ID: fmt.Sprintf("d%d", i), SessionID: "s1", Target: "cb"})
	}
	if _, ok := l.Get("d1"); ok {
		t.Error("oldest record should be evicted")
	}
	if _, ok := l.Get("d5"); !ok {
		t.Error("newest record missing")
	}
	list := l.List("", "", 0)
	if len(list) != 3 || list[0].ID != "d5" || list[2].ID != "d3" {
		t.Errorf("list = %+v", ids(list))
	}
	if got := l.List("s1", "", 2); len(got) != 2 {
		t.Errorf("limited list = %d", len(got))
	}
	if got := l.List("other", "", 0); len(got) != 0 {
		t.Errorf("filtered list = %d", len(got))
	}
}

func TestRetryDelayJitter(t *testing.T) {
	base := 2 * time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		want := base << (attempt - 1)
		for i := 0; i < 50; i++ {
			got := retryDelay(base, attempt)
			lo := time.Duration(float64(want) * 0.79)
			hi := time.Duration(float64(want) * 1.21)
			if got < lo || got > hi {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, got, lo, hi)
			}
		}
	}
}

func ids(rs []Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
