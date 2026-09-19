package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "pbkdf2-sha256$") {
		t.Fatalf("unexpected encoding: %q", encoded)
	}

	ok, err := VerifyPassword(encoded, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(encoded, "correct horse battery stapl")
	if err != nil || ok {
		t.Fatalf("wrong password accepted: ok=%v err=%v", ok, err)
	}
}

func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical — salt is not applied")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		"", "nonsense", "pbkdf2-sha256$x$y$z", "pbkdf2-sha256$600000$!!!$zzz",
		"bcrypt$600000$c2FsdA$aGFzaA", "pbkdf2-sha256$600000$c2FsdA",
	} {
		if _, err := VerifyPassword(bad, "whatever"); err == nil {
			t.Errorf("malformed hash %q accepted silently", bad)
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := NewSessions(time.Hour)
	token, err := s.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !s.Valid(token) {
		t.Fatal("fresh token rejected")
	}
	if s.Valid("some-other-token") {
		t.Fatal("unknown token accepted")
	}

	s.Revoke(token)
	if s.Valid(token) {
		t.Fatal("revoked token still valid")
	}
}

func TestSessionExpires(t *testing.T) {
	now := time.Now()
	s := NewSessions(time.Minute)
	s.now = func() time.Time { return now }

	token, _ := s.Create()
	now = now.Add(30 * time.Second)
	if !s.Valid(token) {
		t.Fatal("token expired early")
	}
	// Valid() slid the expiry forward, so only a full TTL of silence kills it.
	now = now.Add(2 * time.Minute)
	if s.Valid(token) {
		t.Fatal("token outlived its TTL")
	}
	if got := s.Count(); got != 0 {
		t.Fatalf("expired token not swept: count=%d", got)
	}
}

func TestLimiterBlocksAfterMax(t *testing.T) {
	now := time.Now()
	l := NewLimiter(3, time.Minute)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d blocked too early", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("4th attempt allowed past the limit of 3")
	}
	if !l.Allow("5.6.7.8") {
		t.Fatal("a different client was caught by another client's limit")
	}

	now = now.Add(2 * time.Minute)
	if !l.Allow("1.2.3.4") {
		t.Fatal("limit did not lift after the window passed")
	}
}

func TestLimiterResetOnSuccess(t *testing.T) {
	l := NewLimiter(2, time.Minute)
	l.Allow("ip")
	l.Allow("ip")
	if l.Allow("ip") {
		t.Fatal("limit not reached")
	}
	l.Reset("ip")
	if !l.Allow("ip") {
		t.Fatal("reset did not clear the client's history")
	}
}
