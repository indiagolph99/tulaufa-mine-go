package mc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParseStatusRunning(t *testing.T) {
	got := parseStatus(`ActiveState=active
SubState=running
UnitFileState=enabled
MainPID=1234
MemoryCurrent=4294967296
ActiveEnterTimestamp=Fri 2026-09-19 11:30:02 UTC
`)
	if got.ActiveState != "active" || got.SubState != "running" {
		t.Fatalf("state: %+v", got)
	}
	if !got.Enabled {
		t.Error("UnitFileState=enabled did not set Enabled")
	}
	if got.PID != 1234 {
		t.Errorf("PID = %d, want 1234", got.PID)
	}
	if got.MemoryBytes != 4294967296 {
		t.Errorf("MemoryBytes = %d", got.MemoryBytes)
	}
	if got.SinceUnix == 0 {
		t.Error("ActiveEnterTimestamp not parsed")
	}
}

func TestParseStatusStopped(t *testing.T) {
	got := parseStatus(`ActiveState=inactive
SubState=dead
UnitFileState=enabled
MainPID=0
MemoryCurrent=[not set]
ActiveEnterTimestamp=
`)
	if got.ActiveState != "inactive" {
		t.Fatalf("state: %+v", got)
	}
	if got.PID != 0 || got.SinceUnix != 0 {
		t.Errorf("stopped unit reported PID/uptime: %+v", got)
	}
	if got.MemoryBytes != -1 {
		t.Errorf("unset memory should be -1, got %d", got.MemoryBytes)
	}
}

func TestValidAction(t *testing.T) {
	for _, ok := range []string{"start", "stop", "restart"} {
		if !ValidAction(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "status", "logs-follow", "start; id", "START", "reload"} {
		if ValidAction(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// stubCtl points at the repo's fake mc-ctl so these tests need no systemd.
func stubCtl(t *testing.T) *Ctl {
	t.Helper()
	path, err := filepath.Abs("../../testdata/mc-ctl-stub")
	if err != nil {
		t.Fatal(err)
	}
	return &Ctl{Command: path, Timeout: 5 * time.Second, Debounce: 50 * time.Millisecond}
}

func TestCtlStatusThroughStub(t *testing.T) {
	st, err := stubCtl(t).Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.ActiveState != "active" {
		t.Fatalf("stub should report active, got %+v", st)
	}
}

func TestCtlRejectsUnknownAction(t *testing.T) {
	if err := stubCtl(t).Do(context.Background(), "rm -rf /"); err == nil {
		t.Fatal("unknown action was not rejected before reaching the wrapper")
	}
}

func TestCtlDebounces(t *testing.T) {
	c := stubCtl(t)
	c.Debounce = time.Minute
	if err := c.Do(context.Background(), "restart"); err != nil {
		t.Fatalf("first action: %v", err)
	}
	if err := c.Do(context.Background(), "restart"); err != ErrTooSoon {
		t.Fatalf("second action should be debounced, got %v", err)
	}
}

func TestBroadcasterFanOut(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := NewBroadcaster(stubCtl(t), 2, log)
	defer b.Close()

	_, lines1, cancel1, err := b.Subscribe()
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel1()

	_, lines2, cancel2, err := b.Subscribe()
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	defer cancel2()

	// Third subscriber exceeds the cap of 2.
	if _, _, _, err := b.Subscribe(); err == nil {
		t.Fatal("subscriber cap not enforced")
	}

	for i, ch := range []<-chan string{lines1, lines2} {
		select {
		case line := <-ch:
			if line == "" {
				t.Errorf("subscriber %d got an empty line", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("subscriber %d received nothing from the stub tail", i)
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// TestWrapperRejectsBadArguments exercises the real deploy/mc-ctl, not the stub.
// Only the rejection paths are checked, since the accept paths need systemd —
// but rejection is exactly the behaviour that keeps root narrow.
func TestWrapperRejectsBadArguments(t *testing.T) {
	wrapper, err := filepath.Abs("../../deploy/mc-ctl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wrapper); err != nil {
		t.Skipf("wrapper not present: %v", err)
	}

	for _, args := range [][]string{
		{},                  // no argument
		{"status", "extra"}, // more than one argument
		{"logs-follow; id"}, // shell metacharacters
		{"$(id)"},           // command substitution
		{"--user"},          // a systemctl flag
		{"nonsense"},        // unknown verb
		{"STATUS"},          // wrong case
	} {
		cmd := exec.Command("bash", append([]string{wrapper}, args...)...)
		err := cmd.Run()
		if err == nil {
			t.Errorf("wrapper accepted %q — it must refuse anything outside its whitelist", args)
			continue
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() != 64 {
			t.Errorf("wrapper rejected %q with exit %d, want 64 (usage)", args, exitErr.ExitCode())
		}
	}
}
