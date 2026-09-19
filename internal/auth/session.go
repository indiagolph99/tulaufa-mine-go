package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// Sessions is an in-memory session store. Restarting the daemon logs the admin
// out, which is acceptable for a single operator and gives revocation for free.
type Sessions struct {
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	live map[string]time.Time // token -> expiry
}

func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{ttl: ttl, now: time.Now, live: make(map[string]time.Time)}
}

// Create issues a new opaque token.
func (s *Sessions) Create() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.live[token] = s.now().Add(s.ttl)
	return token, nil
}

// Valid reports whether token is a live session, extending it on use so an
// active operator is not logged out mid-session.
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	expiry, ok := s.live[token]
	if !ok {
		return false
	}
	if s.now().After(expiry) {
		delete(s.live, token)
		return false
	}
	s.live[token] = s.now().Add(s.ttl)
	return true
}

func (s *Sessions) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, token)
}

func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	return len(s.live)
}

func (s *Sessions) sweepLocked() {
	now := s.now()
	for token, expiry := range s.live {
		if now.After(expiry) {
			delete(s.live, token)
		}
	}
}
