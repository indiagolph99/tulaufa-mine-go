// Command tulaufa-mine serves the control API behind /api/mc on tulaufa.ru.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/indiagolph99/tulaufa-mine-go/internal/auth"
	"github.com/indiagolph99/tulaufa-mine-go/internal/httpapi"
	"github.com/indiagolph99/tulaufa-mine-go/internal/mc"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash" {
		if err := runHash(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// runHash turns a password read from stdin into the encoded hash for
// ADMIN_PASSWORD_HASH, so the plaintext never has to travel anywhere.
func runHash() error {
	interactive := isTerminal(os.Stdin)

	pw, err := readSecret("password: ", interactive)
	if err != nil {
		return err
	}
	if len([]rune(pw)) < 12 {
		return errors.New("use at least 12 characters — this guards a root-adjacent endpoint")
	}

	// With echo off a typo is invisible, so confirm before committing to a hash
	// that would lock the operator out.
	if interactive {
		again, err := readSecret("again: ", true)
		if err != nil {
			return err
		}
		if again != pw {
			return errors.New("passwords did not match")
		}
	}

	encoded, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(encoded)
	return nil
}

// readSecret reads one whole line, spaces included, so passphrases work. Echo is
// suppressed via stty when there is a terminal to suppress it on.
func readSecret(prompt string, hideEcho bool) (string, error) {
	fmt.Fprint(os.Stderr, prompt)

	if hideEcho {
		if restore, err := setEcho(false); err == nil {
			defer func() {
				_, _ = restore()
				fmt.Fprintln(os.Stderr)
			}()
		} else {
			fmt.Fprintln(os.Stderr, "\n(warning: could not disable echo; the password will be visible)")
			fmt.Fprint(os.Stderr, prompt)
		}
	}

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty password")
	}
	return line, nil
}

// setEcho toggles terminal echo and returns a function restoring the previous
// state. stty keeps this dependency-free; x/term is not reachable through the
// proxy this project builds behind.
func setEcho(on bool) (restore func() ([]byte, error), err error) {
	saved, err := exec.Command("stty", "-F", "/dev/tty", "-g").Output()
	if err != nil {
		// macOS stty has no -F; it takes the terminal on stdin instead.
		saved, err = sttyStdin("-g")
		if err != nil {
			return nil, err
		}
	}
	state := strings.TrimSpace(string(saved))

	arg := "-echo"
	if on {
		arg = "echo"
	}
	if _, err := sttyStdin(arg); err != nil {
		return nil, err
	}
	return func() ([]byte, error) { return sttyStdin(state) }, nil
}

func sttyStdin(args ...string) ([]byte, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Output()
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := httpapi.Config{
		PasswordHash:  os.Getenv("ADMIN_PASSWORD_HASH"),
		AllowedOrigin: envOr("ALLOWED_ORIGIN", "https://tulaufa.ru"),
		SessionTTL:    envDuration("SESSION_TTL", 12*time.Hour),
		SecureCookie:  envBool("SECURE_COOKIE", true),
	}
	if cfg.PasswordHash == "" {
		return errors.New("ADMIN_PASSWORD_HASH is not set — run `tulaufa-mine hash` to generate one")
	}
	// Fail at boot rather than on the first login attempt.
	if _, err := auth.VerifyPassword(cfg.PasswordHash, "probe"); err != nil {
		return fmt.Errorf("ADMIN_PASSWORD_HASH is unusable: %w", err)
	}

	ctlCmd, ctlArgs := splitCommand(envOr("MC_CTL", "sudo -n /usr/local/bin/mc-ctl"))
	ctl := &mc.Ctl{
		Command:  ctlCmd,
		BaseArgs: ctlArgs,
		Timeout:  envDuration("CTL_TIMEOUT", 30*time.Second),
		Debounce: envDuration("ACTION_DEBOUNCE", 10*time.Second),
	}

	logs := mc.NewBroadcaster(ctl, envInt("MAX_LOG_STREAMS", 4), log)
	defer logs.Close()

	api := httpapi.New(cfg, ctl, logs, log)
	addr := envOr("LISTEN_ADDR", "127.0.0.1:8787")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would guillotine the SSE stream.
		IdleTimeout: 2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr, "origin", cfg.AllowedOrigin)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// splitCommand turns "sudo -n /usr/local/bin/mc-ctl" into its program and args.
// Fields, not a shell: nothing here is interpreted by /bin/sh.
func splitCommand(s string) (string, []string) {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return "", nil
	}
	return parts[0], parts[1:]
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
