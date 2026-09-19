package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/indiagolph99/tulaufa-mine-go/internal/auth"
)

// runHashWith pipes input into `go run . hash` and returns its stdout. Piped
// stdin is not a terminal, so the confirmation prompt is skipped — which is
// also how the documented non-interactive usage works.
func runHashWith(t *testing.T, input string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperHash")
	cmd.Env = append(os.Environ(), "GO_HELPER_HASH=1")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func TestHelperHash(t *testing.T) {
	if os.Getenv("GO_HELPER_HASH") != "1" {
		t.Skip("helper")
	}
	if err := runHash(); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

// A passphrase with spaces must hash as a whole. fmt.Scanln used to stop at the
// first space, which rejected every multi-word password outright.
func TestHashAcceptsPassphraseWithSpaces(t *testing.T) {
	const phrase = "correct horse battery staple"

	encoded, err := runHashWith(t, phrase+"\n")
	if err != nil {
		t.Fatalf("hash failed for a passphrase with spaces: %v", err)
	}

	ok, err := auth.VerifyPassword(encoded, phrase)
	if err != nil || !ok {
		t.Fatalf("hash does not verify against the full phrase: ok=%v err=%v", ok, err)
	}
	if ok, _ := auth.VerifyPassword(encoded, "correct"); ok {
		t.Fatal("hash verifies against the first word alone — the phrase was truncated")
	}
}

func TestHashRejectsShortAndEmpty(t *testing.T) {
	for _, in := range []string{"short\n", "\n"} {
		if _, err := runHashWith(t, in); err == nil {
			t.Errorf("input %q was accepted", in)
		}
	}
}
