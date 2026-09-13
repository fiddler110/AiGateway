package pseudonymizer

import (
	"sync"
	"time"
)

const sessionExpiry = 3600 * time.Second

// sessionEntry holds one client's real->fake substitution map plus the
// inverse for response-side reversal, and when it was last touched (for
// idle pruning).
type sessionEntry struct {
	forward map[string]string // real -> fake
	reverse map[string]string // fake -> real
	touched time.Time
}

// sessionStore is the in-memory (per-process) pseudonymization session
// store, keyed by client_id. A Redis-backed implementation sharing state
// across replicas is added in a later phase.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry
	now      func() time.Time
	seen     int // opportunistic-pruning counter
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]*sessionEntry{}, now: time.Now}
}

// getOrCreate returns copies of the client's current forward/reverse maps
// (safe for the caller to read/extend without a lock), creating an empty
// session if none exists yet.
func (s *sessionStore) getOrCreate(clientID string) (forward, reverse map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.seen++
	if s.seen%100 == 0 {
		s.pruneIdleLocked(now)
	}

	e, ok := s.sessions[clientID]
	if !ok {
		e = &sessionEntry{forward: map[string]string{}, reverse: map[string]string{}}
		s.sessions[clientID] = e
	}
	e.touched = now

	fwd := make(map[string]string, len(e.forward))
	rev := make(map[string]string, len(e.reverse))
	for k, v := range e.forward {
		fwd[k] = v
	}
	for k, v := range e.reverse {
		rev[k] = v
	}
	return fwd, rev
}

// merge stores the full merged forward/reverse maps back for a client
// (called after new sensitive values are found and assigned fakes).
func (s *sessionStore) merge(clientID string, forward, reverse map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[clientID]
	if !ok {
		e = &sessionEntry{}
		s.sessions[clientID] = e
	}
	e.forward = forward
	e.reverse = reverse
	e.touched = s.now()
}

// existingFakesLocked-free snapshot of all fakes currently assigned to ANY
// client, used for cross-client collision avoidance within one client's own
// map generation (matching the reference's per-session collision check —
// collisions are checked against the requesting client's own existing
// fakes, which is what forward/reverse above already provide).
func (s *sessionStore) pruneIdleLocked(now time.Time) {
	cutoff := now.Add(-sessionExpiry)
	for id, e := range s.sessions {
		if e.touched.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
}
