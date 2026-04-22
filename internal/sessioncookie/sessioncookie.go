// Package sessioncookie contains the low-level primitives for the web UI
// session cookie: its name, TTL, signing, and verification. Both the
// api.SessionHandler (which issues cookies on login) and the session auth
// middleware (which validates them on every protected request) depend on
// this package; housing the shared code here avoids an import cycle between
// internal/api and internal/api/middleware.
package sessioncookie

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Name is the HTTP cookie name that carries the signed session token.
const Name = "session"

// TTL is how long a session remains valid after login. The cookie Max-Age
// and the signed expiresAt field both use this duration.
const TTL = 7 * 24 * time.Hour

// Sign builds a signed token of the form base64(payload).base64(sig) where
// payload is "userID|expiresAtUnix" and sig is HMAC-SHA256(payload) under
// secret.
func Sign(secret []byte, userID int32, expiresAt time.Time) string {
	payload := fmt.Sprintf("%d|%d", userID, expiresAt.Unix())
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// Parse verifies a cookie value against secret and returns the authenticated
// user ID if the signature matches and the expiry has not passed. The
// returned error is intentionally opaque: callers log it but should not
// surface it to clients (it never contains secret material, but it may help
// an attacker distinguish failure modes).
func Parse(secret []byte, value string) (int32, error) {
	return ParseAt(secret, value, time.Now())
}

// ParseAt is like Parse but uses the provided reference time for the expiry
// check. It exists so tests can deterministically exercise the expiry path
// without sleeping.
func ParseAt(secret []byte, value string, now time.Time) (int32, error) {
	if len(secret) == 0 {
		return 0, errors.New("session secret is empty")
	}
	if value == "" {
		return 0, errors.New("session cookie is empty")
	}

	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return 0, errors.New("malformed session cookie")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, fmt.Errorf("failed to decode session payload: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, fmt.Errorf("failed to decode session signature: %w", err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, sig) {
		return 0, errors.New("session signature mismatch")
	}

	fields := strings.SplitN(string(payload), "|", 2)
	if len(fields) != 2 {
		return 0, errors.New("malformed session payload")
	}
	userID64, err := strconv.ParseInt(fields[0], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse user id: %w", err)
	}
	expUnix, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse expiry: %w", err)
	}
	if now.After(time.Unix(expUnix, 0)) {
		return 0, errors.New("session expired")
	}
	return int32(userID64), nil
}
