// Package auth handles the single-admin password, sessions and login throttling.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Iterations for PBKDF2-HMAC-SHA256. OWASP's floor is 600k; there is one login
// on this service, so the ~0.2s cost is paid by an attacker far more than by us.
const Iterations = 600_000

const (
	saltLen = 16
	keyLen  = 32
	scheme  = "pbkdf2-sha256"
)

var ErrBadHash = errors.New("malformed password hash")

// HashPassword returns an encoded hash of the form
// pbkdf2-sha256$<iterations>$<salt-b64>$<key-b64>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, keyLen)
	if err != nil {
		return "", fmt.Errorf("derive key: %w", err)
	}
	return fmt.Sprintf("%s$%d$%s$%s", scheme, Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches encoded. The comparison is
// constant time, and a malformed hash is an error rather than a silent false so
// a misconfigured deployment fails loudly instead of rejecting every login.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return false, ErrBadHash
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false, ErrBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, ErrBadHash
	}

	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, fmt.Errorf("derive key: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
