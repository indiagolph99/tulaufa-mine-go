// Command tulaufa-mine serves the control API behind /api/mc on tulaufa.ru.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
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
	fmt.Fprint(os.Stderr, "password: ")
	var pw string
	if _, err := fmt.Scanln(&pw); err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	if len(pw) < 12 {
		return errors.New("use at least 12 characters — this guards a root-adjacent endpoint")
	}
	encoded, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(encoded)
	return nil
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
