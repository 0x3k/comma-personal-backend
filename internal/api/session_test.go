package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"comma-personal-backend/internal/db"
)

// fakeUserLookup is a minimal UserLookup implementation driven by a map keyed
// on username. Production code calls GetUIUserByUsername; GetUIUserByID exists
// only to satisfy the interface and is not exercised from the handler.
type fakeUserLookup struct {
	users map[string]db.UiUser
	err   error
}

func (f *fakeUserLookup) GetUIUserByUsername(_ context.Context, username string) (db.UiUser, error) {
	if f.err != nil {
		return db.UiUser{}, f.err
	}
	u, ok := f.users[username]
	if !ok {
		return db.UiUser{}, pgx.ErrNoRows
	}
	return u, nil
}

func (f *fakeUserLookup) GetUIUserByID(_ context.Context, id int32) (db.UiUser, error) {
	for _, u := range f.users {
		if u.ID == id {
			return u, nil
		}
	}
	return db.UiUser{}, pgx.ErrNoRows
}

// newTestUser builds a ui_users row with the password hashed at a low cost.
// Real bootstrap uses BcryptCost (12), but tests use the minimum (4) so the
// suite stays fast; correctness of the cost factor is asserted separately.
func newTestUser(t *testing.T, id int32, username, password string) db.UiUser {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	return db.UiUser{
		ID:           id,
		Username:     username,
		PasswordHash: string(hash),
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

// doLogin runs a POST /v1/session/login request against the handler and
// returns the response recorder. It is pulled out so the rate-limit test can
// issue many identical calls without rebuilding the fixture each time.
func doLogin(h *SessionHandler, body string, remoteAddr string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Login(c)
	return rec
}

func TestLoginSuccess(t *testing.T) {
	user := newTestUser(t, 1, "admin", "correct horse battery staple")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"admin": user}}
	h := NewSessionHandler(lookup, "test-secret")

	rec := doLogin(h, `{"username":"admin","password":"correct horse battery staple"}`, "192.0.2.1:1111")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Username != "admin" {
		t.Errorf("username = %q, want %q", resp.Username, "admin")
	}

	cookies := rec.Result().Cookies()
	var session *http.Cookie
	for _, c := range cookies {
		if c.Name == SessionCookieName {
			session = c
			break
		}
	}
	if session == nil {
		t.Fatal("expected session cookie to be set")
	}
	if !session.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("cookie path = %q, want %q", session.Path, "/")
	}
	if session.MaxAge <= 0 {
		t.Errorf("cookie Max-Age = %d, want > 0", session.MaxAge)
	}

	userID, err := ParseSessionCookie([]byte("test-secret"), session.Value)
	if err != nil {
		t.Fatalf("failed to parse session cookie: %v", err)
	}
	if userID != user.ID {
		t.Errorf("parsed user id = %d, want %d", userID, user.ID)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	user := newTestUser(t, 1, "admin", "correct horse")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"admin": user}}
	h := NewSessionHandler(lookup, "test-secret")

	rec := doLogin(h, `{"username":"admin","password":"wrong"}`, "192.0.2.2:2222")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}

	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			t.Error("did not expect a session cookie on failed login")
		}
	}
}

func TestLoginUnknownUser(t *testing.T) {
	lookup := &fakeUserLookup{users: map[string]db.UiUser{}}
	h := NewSessionHandler(lookup, "test-secret")

	rec := doLogin(h, `{"username":"nobody","password":"whatever"}`, "192.0.2.3:3333")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	var errResp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	// The same message is returned for wrong-password and unknown-user to
	// prevent username enumeration.
	if errResp.Error != "invalid username or password" {
		t.Errorf("error = %q, want %q", errResp.Error, "invalid username or password")
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	h := NewSessionHandler(&fakeUserLookup{}, "test-secret")

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/session/logout", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.Logout(c); err != nil {
		t.Fatalf("logout returned error: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}

	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			cleared = c
			break
		}
	}
	if cleared == nil {
		t.Fatal("expected Set-Cookie clearing the session cookie")
	}
	if cleared.Value != "" {
		t.Errorf("cleared cookie value = %q, want empty", cleared.Value)
	}
	if cleared.MaxAge >= 0 {
		t.Errorf("cleared cookie MaxAge = %d, want < 0", cleared.MaxAge)
	}
}

func TestParseSessionCookieTampered(t *testing.T) {
	secret := []byte("test-secret")
	expiresAt := time.Now().Add(1 * time.Hour)
	token := SignSessionToken(secret, 42, expiresAt)

	// Sanity check: the untampered token parses cleanly.
	if _, err := ParseSessionCookie(secret, token); err != nil {
		t.Fatalf("untampered token failed to parse: %v", err)
	}

	// Flip one bit inside the base64 payload to simulate a client-side edit
	// and verify the HMAC rejects it.
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("unexpected token format: %q", token)
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}
	// Change the user id from whatever it is to "99" without re-signing.
	tampered := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("99|%d", expiresAt.Unix()))) + "." + parts[1]
	if _, err := ParseSessionCookie(secret, tampered); err == nil {
		t.Fatal("expected ParseSessionCookie to reject tampered payload")
	}

	// And that a completely bogus signature also fails.
	fakeSig := base64.RawURLEncoding.EncodeToString([]byte("notarealsignature"))
	if _, err := ParseSessionCookie(secret, parts[0]+"."+fakeSig); err == nil {
		t.Fatal("expected ParseSessionCookie to reject forged signature")
	}

	// Wrong secret must also fail, even for a genuinely signed token.
	if _, err := ParseSessionCookie([]byte("other-secret"), token); err == nil {
		t.Fatal("expected ParseSessionCookie to reject wrong secret")
	}
}

func TestParseSessionCookieExpired(t *testing.T) {
	secret := []byte("test-secret")
	token := SignSessionToken(secret, 7, time.Now().Add(-1*time.Minute))
	if _, err := ParseSessionCookie(secret, token); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestLoginRateLimit(t *testing.T) {
	user := newTestUser(t, 1, "admin", "right")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"admin": user}}
	h := NewSessionHandler(lookup, "test-secret")

	// Pin the clock so every failed attempt lands inside the same window.
	now := time.Now()
	h.now = func() time.Time { return now }

	remote := "192.0.2.9:9999"
	for i := 0; i < loginRateMax; i++ {
		rec := doLogin(h, `{"username":"admin","password":"wrong"}`, remote)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}

	// The next attempt -- still inside the window -- must be rate limited.
	rec := doLogin(h, `{"username":"admin","password":"wrong"}`, remote)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited attempt: status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}

	// A request from a different IP is unaffected; the limiter is per-IP.
	recOther := doLogin(h, `{"username":"admin","password":"wrong"}`, "192.0.2.10:1010")
	if recOther.Code != http.StatusUnauthorized {
		t.Fatalf("different-IP attempt: status = %d, want 401", recOther.Code)
	}

	// After the window slides forward, the original IP gets a fresh budget.
	h.now = func() time.Time { return now.Add(loginRateWindow + time.Second) }
	recAfter := doLogin(h, `{"username":"admin","password":"right"}`, remote)
	if recAfter.Code != http.StatusOK {
		t.Fatalf("post-window attempt: status = %d, want 200; body = %s", recAfter.Code, recAfter.Body.String())
	}
}

func TestLoginRateLimitHonorsXForwardedFor(t *testing.T) {
	user := newTestUser(t, 1, "admin", "right")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"admin": user}}
	h := NewSessionHandler(lookup, "test-secret")

	now := time.Now()
	h.now = func() time.Time { return now }

	fire := func(xff string) int {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/v1/session/login", strings.NewReader(`{"username":"admin","password":"nope"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", xff)
		req.RemoteAddr = "10.0.0.1:5555" // same proxy, different client IPs
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		_ = h.Login(c)
		return rec.Code
	}

	// Client A uses up its budget.
	for i := 0; i < loginRateMax; i++ {
		if code := fire("203.0.113.5"); code != http.StatusUnauthorized {
			t.Fatalf("client A attempt %d: status = %d, want 401", i+1, code)
		}
	}
	if code := fire("203.0.113.5"); code != http.StatusTooManyRequests {
		t.Fatalf("client A limit: status = %d, want 429", code)
	}

	// Client B (different X-Forwarded-For) still has a full budget even
	// though the TCP peer is the same reverse proxy.
	if code := fire("203.0.113.6"); code != http.StatusUnauthorized {
		t.Fatalf("client B first attempt: status = %d, want 401", code)
	}
}

func TestLoginMalformedJSON(t *testing.T) {
	user := newTestUser(t, 1, "admin", "right")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"admin": user}}
	h := NewSessionHandler(lookup, "test-secret")

	rec := doLogin(h, `not json`, "192.0.2.50:5050")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: status = %d, want 400", rec.Code)
	}
}

func TestSessionRegisterRoutes(t *testing.T) {
	h := NewSessionHandler(&fakeUserLookup{}, "test-secret")

	e := echo.New()
	h.RegisterRoutes(e)

	want := map[string]bool{
		"POST /v1/session/login":  true,
		"POST /v1/session/logout": true,
	}
	for _, r := range e.Routes() {
		delete(want, r.Method+" "+r.Path)
	}
	for route := range want {
		t.Errorf("expected route %s to be registered", route)
	}
}

func TestHashPasswordUsesProjectCost(t *testing.T) {
	hash, err := HashPassword("some-secret")
	if err != nil {
		t.Fatalf("HashPassword returned error: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("bcrypt.Cost returned error: %v", err)
	}
	if cost != BcryptCost {
		t.Errorf("bcrypt cost = %d, want %d", cost, BcryptCost)
	}
}
