// Package webhook implements the outgoing webhook dispatcher: a single
// scheduler goroutine over a min-heap of due deliveries plus a fixed worker
// pool for HTTP delivery (spec 4.3, 7).
package webhook

import (
	"bytes"
	"container/heap"
	"context"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/twinstub/twinstub/internal/config"
	"github.com/twinstub/twinstub/internal/version"
)

// ScheduleRequest describes one webhook to deliver.
type ScheduleRequest struct {
	SessionID  string
	TargetName string
	Target     *config.Target
	Event      string
	Body       []byte
	Delay      time.Duration // already scaled by --time-scale
	NoSign     bool          // admin trigger with "sign": false
	// OnFirstAttempt fires exactly once after the first delivery attempt,
	// regardless of its outcome. The scenario engine uses it for the
	// post-dispatch goto.
	OnFirstAttempt func()
}

// PendingInfo is what the admin API shows for a session's queued webhooks.
type PendingInfo struct {
	DeliveryID string    `json:"delivery_id"`
	Target     string    `json:"target"`
	Event      string    `json:"event,omitempty"`
	FireAt     time.Time `json:"fire_at"`
	Attempts   int       `json:"attempts"`
}

type delivery struct {
	id          string
	sessionID   string
	targetName  string
	target      *config.Target
	event       string
	body        []byte
	noSign      bool
	onFirst     func()
	firstFired  bool
	attempts    int
	maxAttempts int
	backoff     time.Duration
	fireAt      time.Time
	canceled    atomic.Bool
	index       int // heap bookkeeping
}

type deliveryHeap []*delivery

func (h deliveryHeap) Len() int           { return len(h) }
func (h deliveryHeap) Less(i, j int) bool { return h[i].fireAt.Before(h[j].fireAt) }
func (h deliveryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *deliveryHeap) Push(x any)        { d := x.(*delivery); d.index = len(*h); *h = append(*h, d) }
func (h *deliveryHeap) Pop() any {
	old := *h
	n := len(old)
	d := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return d
}

type Options struct {
	Workers      int
	LogSize      int
	AllowPrivate bool
	NewID        func() string
	Logger       zerolog.Logger
	Transport    http.RoundTripper // tests may override
}

type Dispatcher struct {
	log    *DeliveryLog
	client *http.Client
	newID  func() string
	logger zerolog.Logger

	mu      sync.Mutex
	heap    deliveryHeap
	pending map[string]map[string]*delivery // session -> delivery id

	wake   chan struct{}
	workCh chan *delivery
	stop   chan struct{}
	done   chan struct{}
	wg     sync.WaitGroup
}

func NewDispatcher(opts Options) *Dispatcher {
	if opts.Workers < 1 {
		opts.Workers = 8
	}
	transport := opts.Transport
	if transport == nil {
		dialer := &net.Dialer{
			Timeout: 10 * time.Second,
			Control: dialGuard(opts.AllowPrivate),
		}
		transport = &http.Transport{
			DialContext:         dialer.DialContext,
			MaxIdleConnsPerHost: opts.Workers,
		}
	}
	d := &Dispatcher{
		log:   NewDeliveryLog(opts.LogSize),
		newID: opts.NewID,
		client: &http.Client{
			Transport: transport,
			// Redirects are never followed (spec 7.4): a 3xx response is
			// recorded as a failed attempt.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger:  opts.Logger,
		pending: map[string]map[string]*delivery{},
		wake:    make(chan struct{}, 1),
		workCh:  make(chan *delivery),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for i := 0; i < opts.Workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	go d.run()
	return d
}

func (d *Dispatcher) Log() *DeliveryLog { return d.log }

// Schedule queues a delivery. Returns the delivery id.
func (d *Dispatcher) Schedule(req ScheduleRequest) string {
	now := time.Now()
	del := &delivery{
		id:          d.newID(),
		sessionID:   req.SessionID,
		targetName:  req.TargetName,
		target:      req.Target,
		event:       req.Event,
		body:        req.Body,
		noSign:      req.NoSign,
		onFirst:     req.OnFirstAttempt,
		maxAttempts: req.Target.Retry.Attempts,
		backoff:     time.Duration(req.Target.Retry.Backoff),
		fireAt:      now.Add(req.Delay),
	}
	d.log.Add(&Record{
		ID:          del.id,
		SessionID:   del.sessionID,
		Target:      del.targetName,
		URL:         del.target.URL,
		Event:       del.event,
		RequestBody: string(del.body),
		Status:      StatusPending,
		CreatedAt:   now,
		FireAt:      del.fireAt,
	})

	d.mu.Lock()
	heap.Push(&d.heap, del)
	if del.sessionID != "" {
		m := d.pending[del.sessionID]
		if m == nil {
			m = map[string]*delivery{}
			d.pending[del.sessionID] = m
		}
		m[del.id] = del
	}
	d.mu.Unlock()
	d.kick()
	return del.id
}

func (d *Dispatcher) kick() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// PendingCount supports the per-session queue limit.
func (d *Dispatcher) PendingCount(sessionID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending[sessionID])
}

func (d *Dispatcher) PendingForSession(sessionID string) []PendingInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]PendingInfo, 0, len(d.pending[sessionID]))
	for _, del := range d.pending[sessionID] {
		out = append(out, PendingInfo{
			DeliveryID: del.id,
			Target:     del.targetName,
			Event:      del.event,
			FireAt:     del.fireAt,
			Attempts:   del.attempts,
		})
	}
	return out
}

// CancelSession drops all queued deliveries of a session (session reset).
func (d *Dispatcher) CancelSession(sessionID string) {
	d.mu.Lock()
	dels := d.pending[sessionID]
	delete(d.pending, sessionID)
	d.mu.Unlock()
	for id, del := range dels {
		del.canceled.Store(true)
		d.log.update(id, func(r *Record) { r.Status = StatusCanceled; r.NextAttemptAt = nil })
	}
}

// Stop drains in-flight deliveries, waiting up to the context deadline.
func (d *Dispatcher) Stop(ctx context.Context) {
	close(d.stop)
	<-d.done
	close(d.workCh)
	finished := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		d.logger.Warn().Msg("dispatcher drain timed out, abandoning in-flight deliveries")
	}
}

// run is the single scheduler goroutine: it pops due deliveries from the
// heap and hands them to the worker pool.
func (d *Dispatcher) run() {
	defer close(d.done)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		var due []*delivery
		d.mu.Lock()
		now := time.Now()
		wait := time.Hour
		for d.heap.Len() > 0 {
			top := d.heap[0]
			if top.canceled.Load() {
				heap.Pop(&d.heap)
				continue
			}
			if top.fireAt.After(now) {
				wait = top.fireAt.Sub(now)
				break
			}
			due = append(due, heap.Pop(&d.heap).(*delivery))
		}
		d.mu.Unlock()

		for _, del := range due {
			select {
			case d.workCh <- del:
			case <-d.stop:
				return
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-d.wake:
		case <-timer.C:
		case <-d.stop:
			return
		}
	}
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for del := range d.workCh {
		d.attempt(del)
	}
}

func (d *Dispatcher) attempt(del *delivery) {
	if del.canceled.Load() {
		return
	}
	del.attempts++
	num := del.attempts

	body := bytes.NewReader(del.body)
	req, err := http.NewRequest(http.MethodPost, del.target.URL, body)
	var resp *http.Response
	started := time.Now()
	headers := map[string]string{}
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", version.UserAgent())
		req.Header.Set("X-TwinStub-Delivery", del.id)
		req.Header.Set("X-TwinStub-Session", del.sessionID)
		if del.event != "" {
			req.Header.Set("X-TwinStub-Event", del.event)
		}
		for k, v := range del.target.Headers {
			req.Header.Set(k, v)
		}
		if del.target.Signing != nil && !del.noSign {
			req.Header.Set(del.target.Signing.Header, Sign(del.target.Signing, del.body, started))
		}
		for k := range req.Header {
			headers[k] = req.Header.Get(k)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(del.target.Timeout))
		resp, err = d.client.Do(req.WithContext(ctx))
		defer cancel()
	}

	att := Attempt{
		Num:            num,
		At:             started,
		RequestHeaders: headers,
		DurationMillis: time.Since(started).Milliseconds(),
	}
	success := false
	if err != nil {
		att.Error = err.Error()
	} else {
		att.StatusCode = resp.StatusCode
		limited := io.LimitReader(resp.Body, MaxStoredResponseBody+1)
		respBody, _ := io.ReadAll(limited)
		_ = resp.Body.Close()
		if len(respBody) > MaxStoredResponseBody {
			respBody = respBody[:MaxStoredResponseBody]
			att.ResponseTrimmed = true
		}
		att.ResponseBody = string(respBody)
		success = resp.StatusCode >= 200 && resp.StatusCode < 300
	}

	var status string
	var next *time.Time
	switch {
	case success:
		status = StatusDelivered
	case num < del.maxAttempts:
		status = StatusRetrying
		t := time.Now().Add(retryDelay(del.backoff, num))
		next = &t
	default:
		status = StatusFailed
	}

	d.log.update(del.id, func(r *Record) {
		r.Attempts = append(r.Attempts, att)
		r.Status = status
		r.NextAttemptAt = next
	})

	evt := d.logger.Info()
	if !success {
		evt = d.logger.Warn()
	}
	evt.Str("delivery", del.id).Str("target", del.targetName).
		Int("attempt", num).Int("status", att.StatusCode).
		Str("error", att.Error).Str("result", status).Msg("webhook attempt")

	if status == StatusRetrying {
		del.fireAt = *next
		d.mu.Lock()
		heap.Push(&d.heap, del)
		d.mu.Unlock()
		d.kick()
	} else {
		d.mu.Lock()
		if m := d.pending[del.sessionID]; m != nil {
			delete(m, del.id)
			if len(m) == 0 {
				delete(d.pending, del.sessionID)
			}
		}
		d.mu.Unlock()
	}

	// Post-dispatch goto fires after the first attempt no matter the
	// outcome; retries continue in the background (spec 5.3).
	if num == 1 && del.onFirst != nil && !del.firstFired {
		del.firstFired = true
		del.onFirst()
	}
}

// retryDelay implements exponential backoff with +-20% jitter.
func retryDelay(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}
