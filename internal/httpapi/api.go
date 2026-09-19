// Package httpapi exposes the control endpoints consumed by /minecraft-admin.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/indiagolph99/tulaufa-mine-go/internal/auth"
	"github.com/indiagolph99/tulaufa-mine-go/internal/mc"
)

const (
	cookieName = "tulaufa_mine_session"
	cookiePath = "/api/mc"
)

type Config struct {
	PasswordHash string
	// Exact origins permitted to make state-changing requests. A list rather
	// than one value because local development legitimately has several:
	// localhost and 127.0.0.1 are distinct origins, and Vite moves to the next
	// free port when 5173 is taken.
	AllowedOrigins []string
	SessionTTL     time.Duration
	SecureCookie   bool // false only for local http development
}

type API struct {
	cfg   Config
	ctl   *mc.Ctl
	logs  *mc.Broadcaster
	sess  *auth.Sessions
	limit *auth.Limiter
	log   *slog.Logger
}

func New(cfg Config, ctl *mc.Ctl, logs *mc.Broadcaster, log *slog.Logger) *API {
	return &API{
		cfg:   cfg,
		ctl:   ctl,
		logs:  logs,
		sess:  auth.NewSessions(cfg.SessionTTL),
		limit: auth.NewLimiter(5, time.Minute),
		log:   log,
	}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/mc/login", a.handleLogin)
	mux.HandleFunc("POST /api/mc/logout", a.requireSession(a.handleLogout))
	mux.HandleFunc("GET /api/mc/session", a.handleSession)
	mux.HandleFunc("GET /api/mc/status", a.requireSession(a.handleStatus))
	mux.HandleFunc("POST /api/mc/action", a.requireSession(a.handleAction))
	mux.HandleFunc("GET /api/mc/logs/stream", a.requireSession(a.handleLogStream))
	return mux
}

// --- middleware ---------------------------------------------------------

func (a *API) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || !a.sess.Valid(c.Value) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
			return
		}
		next(w, r)
	}
}

// checkOrigin guards state-changing requests. Combined with SameSite=Strict this
// closes CSRF without a token round-trip. Matching stays exact — no prefix or
// suffix rules, which are the usual way origin checks get quietly defeated.
func (a *API) checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return errors.New("missing Origin header")
	}
	if slices.Contains(a.cfg.AllowedOrigins, origin) {
		return nil
	}
	// Naming the permitted values costs nothing — they are public URLs — and
	// turns a dead end into an obvious fix during local development.
	return fmt.Errorf("origin %q not allowed (allowed: %s)",
		origin, strings.Join(a.cfg.AllowedOrigins, ", "))
}

// --- handlers -----------------------------------------------------------

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := a.checkOrigin(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	client := clientIP(r)
	if !a.limit.Allow(client) {
		a.log.Warn("login throttled", "ip", client)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts, wait a minute"})
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}

	ok, err := auth.VerifyPassword(a.cfg.PasswordHash, body.Password)
	if err != nil {
		// A broken hash is a deployment fault, not a wrong password.
		a.log.Error("password hash unusable", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server misconfigured"})
		return
	}
	if !ok {
		a.log.Warn("login failed", "ip", client)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}

	token, err := a.sess.Create()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start session"})
		return
	}
	a.limit.Reset(client)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     cookiePath,
		HttpOnly: true,
		Secure:   a.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.cfg.SessionTTL.Seconds()),
	})
	a.log.Info("login ok", "ip", client)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		a.sess.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: cookiePath,
		HttpOnly: true, Secure: a.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func (a *API) handleSession(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(cookieName)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": err == nil && a.sess.Valid(c.Value)})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.ctl.Status(r.Context())
	if err != nil {
		a.log.Error("status failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read service status"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) handleAction(w http.ResponseWriter, r *http.Request) {
	if err := a.checkOrigin(r); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}
	if !mc.ValidAction(body.Action) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported action"})
		return
	}

	client := clientIP(r)
	if err := a.ctl.Do(r.Context(), body.Action); err != nil {
		if errors.Is(err, mc.ErrTooSoon) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow down — another action just ran"})
			return
		}
		a.log.Error("action failed", "action", body.Action, "ip", client, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "action failed"})
		return
	}

	a.log.Info("action ok", "action", body.Action, "ip", client)
	writeJSON(w, http.StatusOK, map[string]string{"action": body.Action, "result": "ok"})
}

func (a *API) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}

	backlog, lines, cancel, err := a.logs.Subscribe()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // belt and braces alongside proxy_buffering off
	w.WriteHeader(http.StatusOK)

	for _, line := range backlog {
		writeSSE(w, "line", line)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-lines:
			if !ok {
				return
			}
			writeSSE(w, "line", line)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// --- helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeSSE emits one event. Newlines inside data would break framing, so each
// line is sent as its own data field.
func writeSSE(w http.ResponseWriter, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, part := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", part)
	}
	fmt.Fprint(w, "\n")
}

// clientIP prefers the address nginx forwards, falling back to the socket peer.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Real-IP"); xff != "" {
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
