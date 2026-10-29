package webhook

import (
	"errors"
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

// Signature states of an attempt.
const (
	SignatureValid   = "valid"   // signed with the target's secret
	SignatureInvalid = "invalid" // deliberately signed with a wrong key
	SignatureNone    = "none"    // no signature header sent
)

// Replay modes recorded on a delivery made by an admin replay. Deliveries
// scheduled by scenarios or the trigger endpoint have no mode.
const (
	ModeReplay       = "replay"        // same body (or an edited one), new delivery id
	ModeRedeliver    = "redeliver"     // same body, new delivery id: a provider retry
	ModeDuplicate    = "duplicate"     // byte-identical, including X-TwinStub-Delivery
	ModeBadSignature = "bad_signature" // signature that does not verify
	ModeNoSignature  = "no_signature"  // signature header omitted
)

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
	Mode          string     `json:"mode,omitempty"`
	ReplayedFrom  string     `json:"replayed_from,omitempty"`
	// HeaderID is the X-TwinStub-Delivery value sent when it differs from
	// ID (a duplicate carries the original delivery's id).
	HeaderID string `json:"header_id,omitempty"`
}

type Attempt struct {
	Num             int               `json:"num"`
	At              time.Time         `json:"at"`
	VirtualAt       *time.Time        `json:"virtual_at,omitempty"` // session time, set when the session clock was advanced
	RequestHeaders  map[string]string `json:"request_headers"`
	StatusCode      int               `json:"status_code,omitempty"`
	Error           string            `json:"error,omitempty"`
	ResponseBody    string            `json:"response_body,omitempty"`
	DurationMillis  int64             `json:"duration_ms"`
	ResponseTrimmed bool              `json:"response_trimmed,omitempty"`
	Signature       string            `json:"signature,omitempty"` // valid | invalid | none
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

// Query selects journal records. Empty fields match everything.
type Query struct {
	SessionID string
	Target    string
	Status    string
	Limit     int    // <= 0 means no limit
	Cursor    string // id of the last record of the previous page
}

// ErrCursorExpired means the cursor's record has rotated out of the ring.
var ErrCursorExpired = errors.New("the cursor's record is no longer in the journal; start again without a cursor")

// Page returns matching records newest first and the cursor of the next
// page, which is empty when nothing more matches.
func (l *DeliveryLog) Page(q Query) ([]Record, string, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Record, 0)
	started := q.Cursor == ""
	next := ""
	for i := 0; i < l.count; i++ {
		r := l.ring[(l.next-1-i+l.size*2)%l.size]
		if r == nil {
			continue
		}
		if !started {
			started = r.ID == q.Cursor
			continue
		}
		if (q.SessionID != "" && r.SessionID != q.SessionID) ||
			(q.Target != "" && r.Target != q.Target) ||
			(q.Status != "" && r.Status != q.Status) {
			continue
		}
		if q.Limit > 0 && len(out) == q.Limit {
			next = out[len(out)-1].ID
			break
		}
		out = append(out, copyRecord(r))
	}
	if !started {
		return nil, "", ErrCursorExpired
	}
	return out, next, nil
}

// List returns records newest first, optionally filtered.
func (l *DeliveryLog) List(sessionID, target string, limit int) []Record {
	out, _, _ := l.Page(Query{SessionID: sessionID, Target: target, Limit: limit})
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

// Clear empties the journal and reports how many records it held.
func (l *DeliveryLog) Clear() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.count
	clear(l.ring)
	l.byID = map[string]*Record{}
	l.next, l.count = 0, 0
	return n
}
