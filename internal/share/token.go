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
//
// RedactPlates is owned by the alpr-video-redaction-export feature and
// instructs the public share-link media handlers to serve the cached
// plate-blurred HLS variant instead of the unredacted source. Tokens
// minted before this field existed decode to RedactPlates=false, which
// preserves backward compatibility -- the new field is omitempty so old
// tokens still verify against the same HMAC. New tokens minted by the
// share-create handler default the field to true (privacy-respecting
// outbound default); the modal UI flips it off only when the operator
// explicitly opts out. The HMAC covers the entire payload (including
// this field), so a token holder cannot bypass the flag client-side.
type payload struct {
	RouteID      int32 `json:"route_id"`
	Exp          int64 `json:"exp"`
	RedactPlates bool  `json:"redact_plates,omitempty"`
}

// Options carries the optional knobs Sign accepts. Today only
// RedactPlates is configurable; future fields go here without breaking
// callers of Sign that pass no options.
type Options struct {
	// RedactPlates, when true, instructs the public share-link media
	// handlers to serve plate-blurred HLS instead of the unredacted
	// source. The flag is signed into the token so the recipient cannot
	// flip it client-side.
	RedactPlates bool
}

// Sign produces a URL-safe signed share token for the given route ID and
// expiry timestamp. The returned token is base64url(payload).base64url(sig)
// where payload is a compact JSON document and sig is the HMAC-SHA256 of
// that same base64url-encoded payload under the provided secret.
//
// Sign defaults all optional payload fields to their zero value
// (RedactPlates=false). Use SignWithOptions to set them.
func Sign(secret []byte, routeID int32, expiresAt time.Time) (string, error) {
	return SignWithOptions(secret, routeID, expiresAt, Options{})
}

// SignWithOptions is like Sign but accepts an Options struct so callers
// can mint tokens with RedactPlates=true (the share-create handler's
// default) without threading additional positional arguments through
// every existing call site.
func SignWithOptions(secret []byte, routeID int32, expiresAt time.Time, opts Options) (string, error) {
	if len(secret) == 0 {
		return "", ErrEmptySecret
	}
	body, err := json.Marshal(payload{
		RouteID:      routeID,
		Exp:          expiresAt.Unix(),
		RedactPlates: opts.RedactPlates,
	})
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
	p, exp, err := parseAt(secret, token, now)
	if err != nil {
		return 0, time.Time{}, err
	}
	return p.RouteID, exp, nil
}

// ParsedToken is the full validated payload decoded from a share token,
// surfaced to callers that need fields beyond the route ID and expiry.
// ParsedToken intentionally hides the wire-level json struct so callers
// do not need to know whether a particular field was present in the
// original payload or defaulted on decode.
type ParsedToken struct {
	RouteID      int32
	ExpiresAt    time.Time
	RedactPlates bool
}

// ParseToken is the full-payload variant of Parse. It returns the
// decoded ParsedToken so callers can branch on optional fields like
// RedactPlates. Used by the share-link media handlers to decide
// whether to serve the redacted variant.
func ParseToken(secret []byte, token string) (ParsedToken, error) {
	return ParseTokenAt(secret, token, time.Now())
}

// ParseTokenAt is like ParseToken but takes an explicit reference time.
// Tests use this to hit the expiry path without sleeping.
func ParseTokenAt(secret []byte, token string, now time.Time) (ParsedToken, error) {
	p, exp, err := parseAt(secret, token, now)
	if err != nil {
		return ParsedToken{}, err
	}
	return ParsedToken{
		RouteID:      p.RouteID,
		ExpiresAt:    exp,
		RedactPlates: p.RedactPlates,
	}, nil
}

// parseAt is the shared verification path used by ParseFull, ParseAt,
// ParseToken, and ParseTokenAt. It returns the full payload (so the
// payload-aware callers can read optional fields) plus the verified
// expiry time. The error taxonomy matches the public Parse* functions:
// ErrEmptySecret, ErrExpired, ErrInvalidToken.
func parseAt(secret []byte, token string, now time.Time) (payload, time.Time, error) {
	if len(secret) == 0 {
		return payload{}, time.Time{}, ErrEmptySecret
	}
	if token == "" {
		return payload{}, time.Time{}, ErrInvalidToken
	}

	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return payload{}, time.Time{}, ErrInvalidToken
	}
	encBody := parts[0]
	encSig := parts[1]

	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return payload{}, time.Time{}, ErrInvalidToken
	}

	// Constant-time HMAC check. hmac.Equal internally uses
	// subtle.ConstantTimeCompare so the comparison does not short-circuit
	// on the first mismatched byte.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encBody))
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, sig) {
		return payload{}, time.Time{}, ErrInvalidToken
	}

	body, err := base64.RawURLEncoding.DecodeString(encBody)
	if err != nil {
		return payload{}, time.Time{}, ErrInvalidToken
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return payload{}, time.Time{}, ErrInvalidToken
	}
	if p.RouteID == 0 {
		return payload{}, time.Time{}, ErrInvalidToken
	}
	if p.Exp <= 0 {
		return payload{}, time.Time{}, ErrInvalidToken
	}
	exp := time.Unix(p.Exp, 0)
	if now.After(exp) {
		return payload{}, time.Time{}, ErrExpired
	}
	return p, exp, nil
}
