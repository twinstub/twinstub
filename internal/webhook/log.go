package webhook

import (
	"sync"
	"time"
)

// Delivery statuses.
const (
	StatusPending   = "pending"
	StatusRetrying  = "retrying"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// MaxStoredResponseBody caps response bodies kept in the log (spec 7.2).
const MaxStoredResponseBody = 64 << 10

// Record is one delivery in the log. The structure is intentionally
// complete: it is the contract for the future commercial delivery journal.
type Record struct {
	ID            string     `json:"id"`
	SessionID     string     `json:"session_id,omitempty"`
	Target        string     `json:"target"`
	URL           string     `json:"url"`
	Event         string     `json:"event,omitempty"`
	RequestBody   string     `json:"request_body"`
	Status        string     `json:"status"`
	Attempts      []Attempt  `json:"attempts"`
	CreatedAt     time.Time  `json:"created_at"`
	FireAt        time.Time  `json:"fire_at"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
}

type Attempt struct {
	Num             int               `json:"num"`
	At              time.Time         `json:"at"`
	RequestHeaders  map[string]string `json:"request_headers"`
	StatusCode      int               `json:"status_code,omitempty"`
	Error           string            `json:"error,omitempty"`
	ResponseBody    string            `json:"response_body,omitempty"`
	DurationMillis  int64             `json:"duration_ms"`
	ResponseTrimmed bool              `json:"response_trimmed,omitempty"`
}

// DeliveryLog is a fixed size ring buffer of delivery records.
type DeliveryLog struct {
	mu    sync.RWMutex
	size  int
	ring  []*Record
	next  int
	count int
	byID  map[string]*Record
}

func NewDeliveryLog(size int) *DeliveryLog {
	if size < 1 {
		size = 1
	}
	return &DeliveryLog{
		size: size,
		ring: make([]*Record, size),
		byID: map[string]*Record{},
	}
}

func (l *DeliveryLog) Add(r *Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if old := l.ring[l.next]; old != nil {
		delete(l.byID, old.ID)
	}
	l.ring[l.next] = r
	l.byID[r.ID] = r
	l.next = (l.next + 1) % l.size
	if l.count < l.size {
		l.count++
	}
}

// Get returns a deep enough copy that callers can serialize without racing
// the dispatcher.
func (l *DeliveryLog) Get(id string) (Record, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	r, ok := l.byID[id]
	if !ok {
		return Record{}, false
	}
	return copyRecord(r), true
}

// List returns records newest first, optionally filtered.
func (l *DeliveryLog) List(sessionID, target string, limit int) []Record {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if limit <= 0 || limit > l.count {
		limit = l.count
	}
	out := make([]Record, 0, limit)
	for i := 0; i < l.count && len(out) < limit; i++ {
		idx := (l.next - 1 - i + l.size*2) % l.size
		r := l.ring[idx]
		if r == nil {
			continue
		}
		if sessionID != "" && r.SessionID != sessionID {
			continue
		}
		if target != "" && r.Target != target {
			continue
		}
		out = append(out, copyRecord(r))
	}
	return out
}

func (l *DeliveryLog) update(id string, fn func(*Record)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.byID[id]; ok {
		fn(r)
	}
}

func copyRecord(r *Record) Record {
	c := *r
	c.Attempts = make([]Attempt, len(r.Attempts))
	copy(c.Attempts, r.Attempts)
	if r.NextAttemptAt != nil {
		t := *r.NextAttemptAt
		c.NextAttemptAt = &t
	}
	return c
}
