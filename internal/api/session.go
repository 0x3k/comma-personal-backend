package api

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/sessioncookie"
)

// BcryptCost is the cost factor used when hashing UI user passwords. 12 is
// the project-wide standard; it balances ~250ms/hash on modern hardware
// against resilience to offline cracking.
const BcryptCost = 12

// SessionCookieName is the HTTP cookie name that carries the signed token.
// It aliases sessioncookie.Name so api handlers and the session middleware
// agree on a single source of truth.
const SessionCookieName = sessioncookie.Name

// SessionTTL is how long a session remains valid after login. The cookie
// Max-Age and the signed expiresAt field both use this duration. It aliases
// sessioncookie.TTL for the same reason as SessionCookieName.
const SessionTTL = sessioncookie.TTL

// loginRateLimit defines the per-IP sliding-window cap on login attempts.
// We keep it small because there is only one admin account in this service.
const (
	loginRateMax    = 5
	loginRateWindow = 15 * time.Minute
)

// UserLookup is the subset of db.Queries that the session handler needs.
// Narrow interfaces keep the tests straightforward.
type UserLookup interface {
	GetUIUserByUsername(ctx context.Context, username string) (db.UiUser, error)
	GetUIUserByID(ctx context.Context, id int32) (db.UiUser, error)
}

// SessionHandler owns POST /v1/session/login and POST /v1/session/logout.
// A single instance is shared across requests; its mutex guards the
// in-memory rate limiter.
type SessionHandler struct {
	users         UserLookup
	sessionSecret []byte

	mu       sync.Mutex
	attempts map[string][]time.Time
	now      func() time.Time
}

// NewSessionHandler returns a handler configured with the given user lookup
// and the server's session-signing secret. The secret must be non-empty --
// callers should check cfg.UIAuthEnabled() before registering routes.
func NewSessionHandler(users UserLookup, sessionSecret string) *SessionHandler {
	return &SessionHandler{
		users:         users,
		sessionSecret: []byte(sessionSecret),
		attempts:      make(map[string][]time.Time),
		now:           time.Now,
	}
}

// loginRequest is the JSON body accepted by POST /v1/session/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse is the JSON body returned on a successful login. Intentionally
// minimal: the client only needs to know the login succeeded; the session
// cookie carries the authenticated state.
type loginResponse struct {
	Username string `json:"username"`
}

// RegisterRoutes wires up session endpoints on the given Echo instance. The
// routes are unauthenticated because they establish the session in the first
// place; CSRF risk is mitigated by SameSite=Lax + the per-IP rate limiter.
func (h *SessionHandler) RegisterRoutes(e *echo.Echo) {
	e.POST("/v1/session/login", h.Login)
	e.POST("/v1/session/logout", h.Logout)
}

// Login validates a username/password combination and, on success, sets a
// signed session cookie. On failure it returns a uniform 401 so callers
// cannot distinguish "unknown user" from "wrong password" by response.
func (h *SessionHandler) Login(c echo.Context) error {
	ip := clientIP(c.Request())
	if !h.allowAttempt(ip) {
		return c.JSON(http.StatusTooManyRequests, errorResponse{
			Error: "too many login attempts, try again later",
			Code:  http.StatusTooManyRequests,
		})
	}

	var req loginRequest
	if err := c.Bind(&req); err != nil {
		h.recordAttempt(ip)
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	if req.Username == "" || req.Password == "" {
		h.recordAttempt(ip)
		return unauthorizedLogin(c)
	}

	user, err := h.users.GetUIUserByUsername(c.Request().Context(), req.Username)
	if err != nil {
		h.recordAttempt(ip)
		if errors.Is(err, pgx.ErrNoRows) {
			// Still run a bcrypt comparison against a junk hash to keep the
			// response time similar between unknown-user and wrong-password
			// cases. bcrypt.CompareHashAndPassword rejects malformed hashes
			// quickly, so we do a real comparison against a dummy hash.
			_ = bcrypt.CompareHashAndPassword(getDummyHash(), []byte(req.Password))
			return unauthorizedLogin(c)
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up user",
			Code:  http.StatusInternalServerError,
		})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		h.recordAttempt(ip)
		return unauthorizedLogin(c)
	}

	expiresAt := h.now().Add(SessionTTL)
	token := SignSessionToken(h.sessionSecret, user.ID, expiresAt)
	c.SetCookie(newSessionCookie(c.Request(), token, expiresAt))

	return c.JSON(http.StatusOK, loginResponse{Username: user.Username})
}

// Logout clears the session cookie. It never fails: even an unauthenticated
// client should be able to call it idempotently.
func (h *SessionHandler) Logout(c echo.Context) error {
	c.SetCookie(newClearedSessionCookie(c.Request()))
	return c.NoContent(http.StatusNoContent)
}

// ParseSessionCookie verifies a cookie value against secret and returns the
// authenticated user ID if the signature matches and the expiry has not
// passed. It delegates to sessioncookie.Parse; see that package for the
// canonical implementation. The returned error is intentionally opaque:
// callers log it but do not expose it to clients.
func ParseSessionCookie(secret []byte, value string) (int32, error) {
	return sessioncookie.Parse(secret, value)
}

// SignSessionToken builds a signed session token. See sessioncookie.Sign
// for the canonical implementation.
func SignSessionToken(secret []byte, userID int32, expiresAt time.Time) string {
	return sessioncookie.Sign(secret, userID, expiresAt)
}

// BootstrapAdmin creates or updates the ui_users row for the operator's
// env-configured credentials. It is a no-op when either variable is empty.
// The caller is expected to guard on UIAuthEnabled before calling this so
// operators without SESSION_SECRET still get device auth but not UI auth.
func BootstrapAdmin(ctx context.Context, q *db.Queries, username, password string) error {
	if username == "" || password == "" {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return fmt.Errorf("failed to hash admin password: %w", err)
	}
	if _, err := q.UpsertUIUser(ctx, db.UpsertUIUserParams{
		Username:     username,
		PasswordHash: string(hash),
	}); err != nil {
		return fmt.Errorf("failed to upsert admin user: %w", err)
	}
	return nil
}

// HashPassword is a thin helper around bcrypt that fixes the cost at
// BcryptCost so every call site agrees on the work factor.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// allowAttempt returns true when the IP has fewer than loginRateMax attempts
// in the trailing loginRateWindow. It only checks -- recordAttempt is what
// mutates the counter, and it is called on every failed login.
func (h *SessionHandler) allowAttempt(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-loginRateWindow)
	attempts := h.attempts[ip]
	kept := attempts[:0]
	for _, t := range attempts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	h.attempts[ip] = kept
	return len(kept) < loginRateMax
}

// recordAttempt appends the current time to the IP's sliding-window history.
// Only failed logins are recorded; a successful login is not counted against
// the budget.
func (h *SessionHandler) recordAttempt(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-loginRateWindow)
	attempts := h.attempts[ip]
	kept := attempts[:0]
	for _, t := range attempts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	h.attempts[ip] = append(kept, h.now())
}

// newSessionCookie builds the cookie written on successful login. The
// Secure flag tracks the request scheme: tests over plaintext still work,
// and production deployments behind TLS get Secure automatically.
func newSessionCookie(r *http.Request, value string, expiresAt time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	}
}

// newClearedSessionCookie builds the cookie written on logout; it targets
// the same Path/Name as the live cookie and sets Max-Age=-1 so browsers
// remove it immediately.
func newClearedSessionCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsTLS(r),
		SameSite: http.SameSiteLaxMode,
	}
}

// requestIsTLS reports whether the inbound request came in over HTTPS. It
// checks r.TLS first and falls back to the X-Forwarded-Proto header that
// reverse proxies like Caddy and nginx set.
func requestIsTLS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

// clientIP returns the remote IP used for rate limiting. It honours the
// X-Forwarded-For header (first value, before any commas) when present so
// operators behind a reverse proxy get per-user limits rather than one
// bucket for the entire proxy.
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			xff = xff[:idx]
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// unauthorizedLogin writes a uniform 401 response. Keeping the body the same
// for unknown-user and wrong-password prevents username enumeration.
func unauthorizedLogin(c echo.Context) error {
	return c.JSON(http.StatusUnauthorized, errorResponse{
		Error: "invalid username or password",
		Code:  http.StatusUnauthorized,
	})
}

// dummyHash is a valid bcrypt hash of a random secret generated on demand.
// It is compared against in the unknown-user branch so wrong-password and
// unknown-user responses take roughly equal time, preventing easy username
// enumeration via timing. The hash is computed lazily on first use and then
// cached, so startup cost is paid only once and only when login is invoked.
var (
	dummyHashOnce  sync.Once
	dummyHashValue []byte
)

func getDummyHash() []byte {
	dummyHashOnce.Do(func() {
		var nonce [32]byte
		_, _ = rand.Read(nonce[:])
		hash, err := bcrypt.GenerateFromPassword(nonce[:], BcryptCost)
		if err != nil {
			// Fall back to a constant at the MinCost-compatible prefix: this
			// only affects timing fidelity, not security.
			dummyHashValue = []byte("$2a$12$" + strings.Repeat(".", 53))
			return
		}
		dummyHashValue = hash
	})
	return dummyHashValue
}
