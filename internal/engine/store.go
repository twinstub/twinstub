package engine

import (
	"hash/fnv"
	"sync"
	"time"

	"github.com/twinstub/twinstub/internal/snapshot"
)

// Session is one live scenario run. The scenario definition is pinned at
// creation time so hot reload never changes semantics mid-session.
type Session struct {
	mu sync.Mutex

	ID            string
	Key           string
	Scenario      *snapshot.Scenario
	ConfigVersion int
	State         string
	Vars          map[string]string
	// Epoch increments on every state entry. Scheduled post-dispatch gotos
	// capture it and become no-ops if the session moved on meanwhile.
	Epoch     uint64
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (s *Session) touchLocked(now time.Time) {
	s.ExpiresAt = now.Add(s.Scenario.TTL)
}

// SessionView is an immutable copy for the admin API.
type SessionView struct {
	ID        string            `json:"id"`
	Key       string            `json:"key"`
	Scenario  string            `json:"scenario"`
	State     string            `json:"state"`
	Vars      map[string]string `json:"vars,omitempty"`
	ConfigVer int               `json:"config_version"`
	CreatedAt time.Time         `json:"created"`
	ExpiresAt time.Time         `json:"expires"`
}

func (s *Session) View() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	vars := make(map[string]string, len(s.Vars))
	for k, v := range s.Vars {
		vars[k] = v
	}
	return SessionView{
		ID:        s.ID,
		Key:       s.Key,
		Scenario:  s.Scenario.ID,
		State:     s.State,
		Vars:      vars,
		ConfigVer: s.ConfigVersion,
		CreatedAt: s.CreatedAt,
		ExpiresAt: s.ExpiresAt,
	}
}

const shardCount = 16

type shard struct {
	mu    sync.RWMutex
	byKey map[string]*Session
}

// Store is the in-memory SessionStore implementation: sharded map by
// session key plus a global id index (spec 4.3). It is the only
// implementation in v1.0; the interface boundary for future persistent
// stores is the Store API itself.
type Store struct {
	shards [shardCount]*shard

	idMu sync.RWMutex
	byID map[string]*Session

	selMu     sync.Mutex
	selectors map[string]*selectorEntry

	maxSessions int
	lenMu       sync.Mutex
	length      int
}

type selectorEntry struct {
	sel      *snapshot.KeySelector
	scenario *snapshot.Scenario
	refs     int
}

func NewStore(maxSessions int) *Store {
	s := &Store{
		byID:        map[string]*Session{},
		selectors:   map[string]*selectorEntry{},
		maxSessions: maxSessions,
	}
	for i := range s.shards {
		s.shards[i] = &shard{byKey: map[string]*Session{}}
	}
	return s
}

func (s *Store) shardFor(key string) *shard {
	h := fnv.New32a()
	h.Write([]byte(key))
	return s.shards[h.Sum32()%shardCount]
}

// ErrSessionLimit is returned when the configured session cap is reached.
type ErrSessionLimit struct{ Max int }

func (e ErrSessionLimit) Error() string { return "session limit reached" }

// Create inserts a new session unless the key is already taken; in that
// case the existing session is returned and created is false.
func (s *Store) Create(sess *Session) (existing *Session, created bool, err error) {
	sh := s.shardFor(sess.Key)
	sh.mu.Lock()
	if cur, ok := sh.byKey[sess.Key]; ok {
		sh.mu.Unlock()
		return cur, false, nil
	}
	s.lenMu.Lock()
	if s.maxSessions > 0 && s.length >= s.maxSessions {
		s.lenMu.Unlock()
		sh.mu.Unlock()
		return nil, false, ErrSessionLimit{Max: s.maxSessions}
	}
	s.length++
	s.lenMu.Unlock()
	sh.byKey[sess.Key] = sess
	sh.mu.Unlock()

	s.idMu.Lock()
	s.byID[sess.ID] = sess
	s.idMu.Unlock()

	s.selMu.Lock()
	selKey := sess.Scenario.KeySelector.String()
	if e, ok := s.selectors[selKey]; ok {
		e.refs++
	} else {
		s.selectors[selKey] = &selectorEntry{sel: sess.Scenario.KeySelector, scenario: sess.Scenario, refs: 1}
	}
	s.selMu.Unlock()
	return sess, true, nil
}

func (s *Store) GetByKey(key string) *Session {
	sh := s.shardFor(key)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.byKey[key]
}

func (s *Store) GetByID(id string) *Session {
	s.idMu.RLock()
	defer s.idMu.RUnlock()
	return s.byID[id]
}

// Delete removes a session by id and returns it, or nil.
func (s *Store) Delete(id string) *Session {
	s.idMu.Lock()
	sess, ok := s.byID[id]
	if ok {
		delete(s.byID, id)
	}
	s.idMu.Unlock()
	if !ok {
		return nil
	}

	sh := s.shardFor(sess.Key)
	sh.mu.Lock()
	if sh.byKey[sess.Key] == sess {
		delete(sh.byKey, sess.Key)
	}
	sh.mu.Unlock()

	s.lenMu.Lock()
	s.length--
	s.lenMu.Unlock()

	s.selMu.Lock()
	selKey := sess.Scenario.KeySelector.String()
	if e, ok := s.selectors[selKey]; ok {
		e.refs--
		if e.refs <= 0 {
			delete(s.selectors, selKey)
		}
	}
	s.selMu.Unlock()
	return sess
}

func (s *Store) Len() int {
	s.lenMu.Lock()
	defer s.lenMu.Unlock()
	return s.length
}

func (s *Store) List() []*Session {
	s.idMu.RLock()
	defer s.idMu.RUnlock()
	out := make([]*Session, 0, len(s.byID))
	for _, sess := range s.byID {
		out = append(out, sess)
	}
	return out
}

// SelectorRef pairs a live selector with a scenario that can evaluate it.
// Used to find sessions whose scenario is gone from the current config.
type SelectorRef struct {
	Sel      *snapshot.KeySelector
	Scenario *snapshot.Scenario
}

func (s *Store) LiveSelectors() []SelectorRef {
	s.selMu.Lock()
	defer s.selMu.Unlock()
	out := make([]SelectorRef, 0, len(s.selectors))
	for _, e := range s.selectors {
		out = append(out, SelectorRef{Sel: e.sel, Scenario: e.scenario})
	}
	return out
}

// Sweep removes expired sessions and returns them so the caller can cancel
// their pending webhooks.
func (s *Store) Sweep(now time.Time) []*Session {
	var expired []*Session
	s.idMu.RLock()
	for _, sess := range s.byID {
		sess.mu.Lock()
		if now.After(sess.ExpiresAt) {
			expired = append(expired, sess)
		}
		sess.mu.Unlock()
	}
	s.idMu.RUnlock()
	for _, sess := range expired {
		s.Delete(sess.ID)
	}
	return expired
}
