// Package misslog keeps the most recent requests that matched nothing, so
// the admin API can explain a 404 after the fact instead of only in the
// response the caller may never have looked at.
package misslog

import (
	"maps"
	"sync"
	"time"
)

// DefaultSize is how many misses are kept.
const DefaultSize = 50

// Log is a fixed size ring buffer of 404 diagnostics.
type Log struct {
	mu    sync.Mutex
	ring  []map[string]any
	next  int
	count int
}

func New(size int) *Log {
	if size < 1 {
		size = DefaultSize
	}
	return &Log{ring: make([]map[string]any, size)}
}

// Add stores a 404 body with the time it happened. The body is copied, so
// the caller may keep using it.
func (l *Log) Add(at time.Time, body map[string]any) {
	entry := maps.Clone(body)
	entry["at"] = at
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring[l.next] = entry
	l.next = (l.next + 1) % len(l.ring)
	if l.count < len(l.ring) {
		l.count++
	}
}

// List returns up to limit misses, newest first; limit <= 0 means all.
func (l *Log) List(limit int) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > l.count {
		limit = l.count
	}
	out := make([]map[string]any, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (l.next - 1 - i + 2*len(l.ring)) % len(l.ring)
		out = append(out, l.ring[idx])
	}
	return out
}

// Clear empties the log and reports how many misses it held.
func (l *Log) Clear() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.count
	clear(l.ring)
	l.next, l.count = 0, 0
	return n
}
