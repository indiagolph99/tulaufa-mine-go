package auth

import (
	"sync"
	"time"
)

// Limiter throttles login attempts per client. It is a fixed window rather than
// anything cleverer: the traffic here is one human, so the only job is making
// online guessing hopeless.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, hits: make(map[string][]time.Time)}
}

// Allow records an attempt from key and reports whether it may proceed.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := l.now().Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, l.now())
	return true
}

// Reset clears a client's history, called after a successful login so a correct
// password immediately restores a clean slate.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
