package pseudonymizer

import (
	"errors"
	"sync"
	"time"
)

const sessionExpiry = 3600 * time.Second

// maxSalts bounds re-salting when a candidate fake is already issued to a
// different real value in the same session.
const maxSalts = 512

var errFakeSpaceExhausted = errors.New("context_pseudonymizer: could not assign a unique pseudonym")

// session holds one identity's real->fake substitution map plus the inverse
// for response-side reversal. All reads and writes go through its lock, and
// assign is the only way to add a mapping, so the two maps always stay a
// bijection even under concurrent requests (P0.11).
type session struct {
	mu      sync.Mutex
	forward map[string]string // real -> fake
	reverse map[string]string // fake -> real
	touched time.Time         // guarded by sessionStore.mu
}

func newSession() *session {
	return &session{forward: map[string]string{}, reverse: map[string]string{}}
}

// assign returns the fake for real, issuing one if real has none yet. gen
// produces the candidate fake for a given salt; a candidate already issued
// to a different real value is skipped. Checking and inserting under one
// lock means every committed fake is visible to the collision check.
func (s *session) assign(real string, gen func(salt int) string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fake, ok := s.forward[real]; ok {
		return fake, nil
	}
	for salt := 0; salt < maxSalts; salt++ {
		candidate := gen(salt)
		if owner, taken := s.reverse[candidate]; taken && owner != real {
			continue
		}
		s.forward[real] = candidate
		s.reverse[candidate] = real
		return candidate, nil
	}
	return "", errFakeSpaceExhausted
}

// snapshot returns copies of the forward and reverse maps, safe to use
// without the lock.
func (s *session) snapshot() (forward, reverse map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	forward = make(map[string]string, len(s.forward))
	reverse = make(map[string]string, len(s.reverse))
	for k, v := range s.forward {
		forward[k] = v
	}
	for k, v := range s.reverse {
		reverse[k] = v
	}
	return forward, reverse
}

// sessionStore is the in-memory (per-process) store of persistent
// pseudonymization sessions. A Redis-backed implementation sharing state
// across replicas is added in a later phase (P2.2/P2.3).
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
	now      func() time.Time
	seen     int // opportunistic-pruning counter
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]*session{}, now: time.Now}
}

// get returns the session stored under key, creating it when create is
// true. It returns nil if the session doesn't exist and create is false.
func (s *sessionStore) get(key string, create bool) *session {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.seen++
	if s.seen%100 == 0 {
		s.pruneIdleLocked(now)
	}

	e, ok := s.sessions[key]
	if !ok {
		if !create {
			return nil
		}
		e = newSession()
		s.sessions[key] = e
	}
	e.touched = now
	return e
}

func (s *sessionStore) pruneIdleLocked(now time.Time) {
	cutoff := now.Add(-sessionExpiry)
	for id, e := range s.sessions {
		if e.touched.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
}
