package mc

import (
	"bufio"
	"context"
	"log/slog"
	"os/exec"
	"sync"
)

// Backlog is how many recent lines a newly connected client receives.
const Backlog = 200

// Broadcaster owns a single `mc-ctl logs-follow` process and fans its output out
// to every subscriber. One journalctl per viewer would be wasteful on a box with
// 485MB free, and would multiply the privileged processes for no benefit.
type Broadcaster struct {
	ctl     *Ctl
	maxSubs int
	log     *slog.Logger

	mu     sync.Mutex
	subs   map[chan string]struct{}
	ring   []string
	stop   context.CancelFunc
	runner sync.WaitGroup
}

func NewBroadcaster(ctl *Ctl, maxSubs int, log *slog.Logger) *Broadcaster {
	return &Broadcaster{ctl: ctl, maxSubs: maxSubs, log: log, subs: make(map[chan string]struct{})}
}

// ErrTooManyStreams is returned when the subscriber cap is reached.
type ErrTooManyStreams struct{ Max int }

func (e ErrTooManyStreams) Error() string { return "too many concurrent log streams" }

// Subscribe returns the current backlog plus a channel of future lines. The
// returned cancel must be called by the caller when it goes away.
func (b *Broadcaster) Subscribe() (backlog []string, lines <-chan string, cancel func(), err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.subs) >= b.maxSubs {
		return nil, nil, nil, ErrTooManyStreams{Max: b.maxSubs}
	}

	ch := make(chan string, 256)
	b.subs[ch] = struct{}{}
	backlog = append(backlog, b.ring...)

	if b.stop == nil {
		b.startLocked()
	}

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subs[ch]; !ok {
				return
			}
			delete(b.subs, ch)
			close(ch)
			if len(b.subs) == 0 {
				b.stopLocked()
			}
		})
	}
	return backlog, ch, cancel, nil
}

// startLocked launches the tail. Caller holds b.mu.
func (b *Broadcaster) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	b.stop = cancel

	args := append(append([]string{}, b.ctl.BaseArgs...), "logs-follow")
	cmd := exec.CommandContext(ctx, b.ctl.Command, args...)
	// The tail is killed by cancelling ctx; give it a chance to exit cleanly.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		b.log.Error("log tail: stdout pipe", "err", err)
		cancel()
		b.stop = nil
		return
	}
	if err := cmd.Start(); err != nil {
		b.log.Error("log tail: start", "err", err)
		cancel()
		b.stop = nil
		return
	}

	b.runner.Add(1)
	go func() {
		defer b.runner.Done()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			b.publish(scanner.Text())
		}
		_ = cmd.Wait()
		if ctx.Err() == nil {
			b.log.Warn("log tail exited unexpectedly")
		}
	}()
}

// stopLocked terminates the tail. Caller holds b.mu.
func (b *Broadcaster) stopLocked() {
	if b.stop == nil {
		return
	}
	b.stop()
	b.stop = nil
}

func (b *Broadcaster) publish(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.ring = append(b.ring, line)
	if len(b.ring) > Backlog {
		b.ring = b.ring[len(b.ring)-Backlog:]
	}

	for ch := range b.subs {
		select {
		case ch <- line:
		default:
			// A client that cannot keep up loses lines rather than stalling
			// the tail for everyone else.
		}
	}
}

// Close shuts the tail down regardless of subscribers, for graceful exit.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	b.stopLocked()
	b.mu.Unlock()
	b.runner.Wait()
}
