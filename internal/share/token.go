// Package share contains the signed-token primitives that back the public
// read-only share links. A share token carries a route ID and an expiry
// timestamp, is signed with HMAC-SHA256 under the same SESSION_SECRET used
// by the web UI session cookie, and is verified via constant-time
// comparison before the public share handlers return any route data.
//
// Token format: base64url(JSON({route_id, exp})) + "." + base64url(hmac).
// This is intentionally symmetric with internal/sessioncookie so the two
// signing schemes are auditable side-by-side. The JSON payload (rather
// than the pipe-delimited payload used by sessioncookie) makes it
// trivial to add fields later without breaking existing tokens.
package share

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrEmptySecret is returned when Sign or Parse is called with no secret
// bytes. Public share links are disabled when SESSION_SECRET is unset, and
// callers use this sentinel to turn that into a 501 Not Implemented.
var ErrEmptySecret = errors.New("share: signing secret is empty")

// ErrExpired is returned by Parse when the token signature verifies but
// the exp timestamp is in the past. Callers typically turn this into an
// HTTP 410 Gone, which is distinct from the generic 401/403 used for
// malformed or forged tokens.
var ErrExpired = errors.New("share: token expired")

// ErrInvalidToken covers every other failure mode: malformed structure,
// invalid base64, unparseable JSON payload, or signature mismatch. We
// intentionally do not distinguish between these to an attacker; the
// underlying error is only logged internally.
var ErrInvalidToken = errors.New("share: invalid token")

// payload is the JSON body that sits inside a signed share token. The
// field names use snake_case so the on-wire format is easy to eyeball
// when debugging a token manually.
type payload struct {
	RouteID int32 `json:"route_id"`
	Exp     int64 `json:"exp"`
}

// Sign produces a URL-safe signed share token for the given route ID and
// expiry timestamp. The returned token is base64url(payload).base64url(sig)
// where payload is a compact JSON document and sig is the HMAC-SHA256 of
// that same base64url-encoded payload under the provided secret.
func Sign(secret []byte, routeID int32, expiresAt time.Time) (string, error) {
	if len(secret) == 0 {
		return "", ErrEmptySecret
	}
	body, err := json.Marshal(payload{RouteID: routeID, Exp: expiresAt.Unix()})
	if err != nil {
		return "", fmt.Errorf("share: marshal payload: %w", err)
	}
	encBody := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encBody))
	sig := mac.Sum(nil)
	return encBody + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Parse verifies a share token against secret and returns the route ID
// encoded in its payload. Parse uses the current wall-clock time to check
// the expiry; callers that need deterministic tests should use ParseAt.
//
// The returned error is one of ErrEmptySecret, ErrExpired, or
// ErrInvalidToken so callers can map each to a distinct HTTP status
// without having to inspect string messages. ErrInvalidToken is
// intentionally coarse: any malformed-or-forged token collapses to a
// single sentinel so attackers cannot distinguish "bad signature" from
// "bad base64" and fuzz the parser.
func Parse(secret []byte, token string) (int32, error) {
	return ParseAt(secret, token, time.Now())
}

// ParseAt is like Parse but takes an explicit reference time. Tests use
// this to hit the expiry path without sleeping.
func ParseAt(secret []byte, token string, now time.Time) (int32, error) {
	routeID, _, err := ParseFull(secret, token, now)
	return routeID, err
}

// ParseFull is like ParseAt but additionally returns the expiry time
// embedded in the token. Callers that need to display or propagate the
// expiry (the share API echoes it back so the client can render a
// "expires at <time>" hint) use this form; callers that only need to
// verify and extract the route ID use Parse/ParseAt.
func ParseFull(secret []byte, token string, now time.Time) (int32, time.Time, error) {
	if len(secret) == 0 {
		return 0, time.Time{}, ErrEmptySecret
	}
	if token == "" {
		return 0, time.Time{}, ErrInvalidToken
	}

	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return 0, time.Time{}, ErrInvalidToken
	}
	encBody := parts[0]
	encSig := parts[1]

	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return 0, time.Time{}, ErrInvalidToken
	}

	// Constant-time HMAC check. hmac.Equal internally uses
	// subtle.ConstantTimeCompare so the comparison does not short-circuit
	// on the first mismatched byte.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encBody))
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, sig) {
		return 0, time.Time{}, ErrInvalidToken
	}

	body, err := base64.RawURLEncoding.DecodeString(encBody)
	if err != nil {
		return 0, time.Time{}, ErrInvalidToken
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, time.Time{}, ErrInvalidToken
	}
	if p.RouteID == 0 {
		return 0, time.Time{}, ErrInvalidToken
	}
	if p.Exp <= 0 {
		return 0, time.Time{}, ErrInvalidToken
	}
	exp := time.Unix(p.Exp, 0)
	if now.After(exp) {
		return 0, time.Time{}, ErrExpired
	}
	return p.RouteID, exp, nil
}
