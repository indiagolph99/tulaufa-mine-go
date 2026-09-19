package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/indiagolph99/tulaufa-mine-go/internal/auth"
	"github.com/indiagolph99/tulaufa-mine-go/internal/mc"
)

const (
	testPassword = "a-sufficiently-long-password"
	testOrigin   = "https://tulaufa.ru"
)

func newTestAPI(t *testing.T) http.Handler {
	t.Helper()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	stub, err := filepath.Abs("../../testdata/mc-ctl-stub")
	if err != nil {
		t.Fatal(err)
	}
	ctl := &mc.Ctl{Command: stub, Timeout: 5 * time.Second, Debounce: time.Millisecond}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return New(Config{
		PasswordHash:  hash,
		AllowedOrigin: testOrigin,
		SessionTTL:    time.Hour,
		SecureCookie:  false,
	}, ctl, mc.NewBroadcaster(ctl, 2, log), log).Handler()
}

func post(t *testing.T, h http.Handler, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := post(t, h, "/api/mc/login", `{"password":"`+testPassword+`"}`, testOrigin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestProtectedEndpointsRejectAnonymous(t *testing.T) {
	h := newTestAPI(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/mc/status"},
		{http.MethodGet, "/api/mc/logs/stream"},
		{http.MethodPost, "/api/mc/action"},
		{http.MethodPost, "/api/mc/logout"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"action":"start"}`))
		req.Header.Set("Origin", testOrigin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h := newTestAPI(t)
	rec := post(t, h, "/api/mc/login", `{"password":"wrong"}`, testOrigin, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a failed login handed out a cookie")
	}
}

func TestLoginRejectsForeignOrigin(t *testing.T) {
	h := newTestAPI(t)
	rec := post(t, h, "/api/mc/login", `{"password":"`+testPassword+`"}`, "https://evil.example", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

func TestLoginRequiresOriginHeader(t *testing.T) {
	h := newTestAPI(t)
	if rec := post(t, h, "/api/mc/login", `{"password":"`+testPassword+`"}`, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 for a missing Origin", rec.Code)
	}
}

func TestLoginThrottled(t *testing.T) {
	h := newTestAPI(t)
	var last int
	for range 8 {
		last = post(t, h, "/api/mc/login", `{"password":"nope"}`, testOrigin, nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after 8 bad logins got %d, want 429", last)
	}
}

func TestSessionFlow(t *testing.T) {
	h := newTestAPI(t)
	cookie := login(t, h)

	req := httptest.NewRequest(http.MethodGet, "/api/mc/status", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with session = %d: %s", rec.Code, rec.Body.String())
	}

	var st mc.Status
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.ActiveState != "active" {
		t.Errorf("unexpected status payload: %+v", st)
	}

	if rec := post(t, h, "/api/mc/logout", "", testOrigin, cookie); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d", rec.Code)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/api/mc/status", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("session still worked after logout: %d", rec2.Code)
	}
}

func TestActionValidation(t *testing.T) {
	h := newTestAPI(t)
	cookie := login(t, h)

	if rec := post(t, h, "/api/mc/action", `{"action":"start"}`, testOrigin, cookie); rec.Code != http.StatusOK {
		t.Fatalf("valid action = %d: %s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{`{"action":"status"}`, `{"action":"start; id"}`, `{"action":""}`, `{}`} {
		if rec := post(t, h, "/api/mc/action", bad, testOrigin, cookie); rec.Code != http.StatusBadRequest {
			t.Errorf("action %s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestActionRejectsForeignOrigin(t *testing.T) {
	h := newTestAPI(t)
	cookie := login(t, h)
	rec := post(t, h, "/api/mc/action", `{"action":"stop"}`, "https://evil.example", cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin action = %d, want 403", rec.Code)
	}
}

func TestSessionEndpointIsPublic(t *testing.T) {
	h := newTestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/mc/session", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session probe = %d, want 200", rec.Code)
	}
	var body map[string]bool
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["authenticated"] {
		t.Fatal("anonymous probe reported authenticated")
	}
}

func TestSSEStreamsLines(t *testing.T) {
	h := newTestAPI(t)
	cookie := login(t, h)

	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/mc/logs/stream", nil)
	req.AddCookie(cookie)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read stream: %v", err)
	}
	if !bytes.Contains(buf[:n], []byte("event: line")) {
		t.Fatalf("stream did not start with an SSE event: %q", buf[:n])
	}
}
