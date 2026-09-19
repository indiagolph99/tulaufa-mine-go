// Package mc talks to minecraft.service through the root-owned mc-ctl wrapper.
package mc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Actions the wrapper accepts. Anything outside this set is rejected here as
// well as inside mc-ctl, so a bug on either side alone is not enough to widen
// what can be run as root.
const (
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
)

func ValidAction(a string) bool {
	switch a {
	case ActionStart, ActionStop, ActionRestart:
		return true
	}
	return false
}

// ErrTooSoon is returned when an action arrives inside the debounce window.
var ErrTooSoon = errors.New("another action was issued moments ago")

// Status is the subset of systemd state the UI renders.
type Status struct {
	ActiveState string `json:"activeState"` // active, inactive, failed, activating...
	SubState    string `json:"subState"`    // running, dead, start-pre...
	Enabled     bool   `json:"enabled"`
	SinceUnix   int64  `json:"sinceUnix"`   // 0 when not running
	PID         int    `json:"pid"`         // 0 when not running
	MemoryBytes int64  `json:"memoryBytes"` // -1 when unknown
}

// Ctl runs the privileged wrapper. Sudo is not assumed: the configured command
// is whatever the unit is set up to call, which keeps local development honest
// (a stub script) without a build tag.
type Ctl struct {
	Command  string        // e.g. "sudo"
	BaseArgs []string      // e.g. ["-n", "/usr/local/bin/mc-ctl"]
	Timeout  time.Duration // for non-streaming calls
	Debounce time.Duration

	mu       sync.Mutex
	lastFire time.Time
}

func (c *Ctl) run(ctx context.Context, arg string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Command, append(append([]string{}, c.BaseArgs...), arg)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mc-ctl %s: %w: %s", arg, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Status queries systemd and parses the key=value block mc-ctl emits.
func (c *Ctl) Status(ctx context.Context) (Status, error) {
	out, err := c.run(ctx, "status")
	if err != nil {
		return Status{}, err
	}
	return parseStatus(string(out)), nil
}

// Do performs a state change, refusing one that arrives too soon after the last
// so a double-click cannot thrash the JVM.
func (c *Ctl) Do(ctx context.Context, action string) error {
	if !ValidAction(action) {
		return fmt.Errorf("unsupported action %q", action)
	}

	c.mu.Lock()
	if time.Since(c.lastFire) < c.Debounce {
		c.mu.Unlock()
		return ErrTooSoon
	}
	c.lastFire = time.Now()
	c.mu.Unlock()

	_, err := c.run(ctx, action)
	return err
}

func parseStatus(out string) Status {
	s := Status{MemoryBytes: -1}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			s.ActiveState = value
		case "SubState":
			s.SubState = value
		case "UnitFileState":
			s.Enabled = value == "enabled" || value == "enabled-runtime"
		case "MainPID":
			s.PID, _ = strconv.Atoi(value)
		case "MemoryCurrent":
			// systemd reports [not set] as a huge sentinel or the literal string
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 && n < 1<<62 {
				s.MemoryBytes = n
			}
		case "ActiveEnterTimestamp":
			if value == "" || value == "n/a" {
				continue
			}
			// systemd's default format, e.g. "Fri 2026-09-19 11:30:02 UTC"
			if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", value); err == nil {
				s.SinceUnix = t.Unix()
			}
		}
	}
	if s.ActiveState != "active" {
		s.SinceUnix, s.PID = 0, 0
	}
	return s
}
