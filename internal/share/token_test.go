package share

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSignParseRoundTrip(t *testing.T) {
	secret := []byte("super-secret-key")
	exp := time.Now().Add(1 * time.Hour)

	tok, err := Sign(secret, 42, exp)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if tok == "" {
		t.Fatal("Sign() returned empty token")
	}
	if !strings.Contains(tok, ".") {
		t.Fatalf("Sign() returned token without a dot separator: %q", tok)
	}

	routeID, err := Parse(secret, tok)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if routeID != 42 {
		t.Errorf("Parse() routeID = %d, want 42", routeID)
	}
}

func TestSignEmptySecret(t *testing.T) {
	_, err := Sign(nil, 1, time.Now().Add(1*time.Hour))
	if !errors.Is(err, ErrEmptySecret) {
		t.Errorf("Sign() empty-secret error = %v, want ErrEmptySecret", err)
	}
}

func TestParseEmptySecret(t *testing.T) {
	_, err := Parse(nil, "anything")
	if !errors.Is(err, ErrEmptySecret) {
		t.Errorf("Parse() empty-secret error = %v, want ErrEmptySecret", err)
	}
}

func TestParseExpired(t *testing.T) {
	secret := []byte("secret")
	expired := time.Now().Add(-1 * time.Hour)
	tok, err := Sign(secret, 7, expired)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	_, err = Parse(secret, tok)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("Parse() expired error = %v, want ErrExpired", err)
	}
}

func TestParseAtPastExpiry(t *testing.T) {
	secret := []byte("secret")
	exp := time.Unix(1_000_000, 0)
	tok, err := Sign(secret, 9, exp)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}

	// before expiry: valid
	before := time.Unix(999_999, 0)
	if _, err := ParseAt(secret, tok, before); err != nil {
		t.Errorf("ParseAt(before) error = %v, want nil", err)
	}

	// at expiry: still valid (now.After(exp) is false)
	if _, err := ParseAt(secret, tok, exp); err != nil {
		t.Errorf("ParseAt(at) error = %v, want nil", err)
	}

	// after expiry: ErrExpired
	after := time.Unix(1_000_001, 0)
	if _, err := ParseAt(secret, tok, after); !errors.Is(err, ErrExpired) {
		t.Errorf("ParseAt(after) error = %v, want ErrExpired", err)
	}
}

func TestParseTamperedSignature(t *testing.T) {
	secret := []byte("secret")
	tok, err := Sign(secret, 11, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}

	// Flip the last character of the signature segment.
	last := tok[len(tok)-1]
	var replacement byte = 'A'
	if last == 'A' {
		replacement = 'B'
	}
	tampered := tok[:len(tok)-1] + string(replacement)

	_, err = Parse(secret, tampered)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse() tampered sig error = %v, want ErrInvalidToken", err)
	}
}

func TestParseTamperedPayload(t *testing.T) {
	secret := []byte("secret")
	tok, err := Sign(secret, 13, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}

	// Replace the entire payload segment with an arbitrary (but valid
	// base64url) alternative. Signature will no longer match.
	parts := strings.SplitN(tok, ".", 2)
	tampered := "aGVsbG8." + parts[1]

	_, err = Parse(secret, tampered)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse() tampered payload error = %v, want ErrInvalidToken", err)
	}
}

func TestParseWrongSecret(t *testing.T) {
	tok, err := Sign([]byte("secret-a"), 3, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	_, err = Parse([]byte("secret-b"), tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse() wrong-secret error = %v, want ErrInvalidToken", err)
	}
}

func TestParseMalformed(t *testing.T) {
	secret := []byte("secret")
	cases := map[string]string{
		"empty":           "",
		"no dot":          "payload_with_no_separator",
		"three parts":     "a.b.c",
		"bad base64 sig":  "aGVsbG8.!!!not-valid!!!",
		"bad base64 body": "@@@.aGVsbG8",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(secret, tok)
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Parse(%q) error = %v, want ErrInvalidToken", tok, err)
			}
		})
	}
}

func TestParseZeroRouteIDRejected(t *testing.T) {
	// A signed token that claims route_id 0 is rejected as malformed; our
	// routes table is serial and starts at 1, and 0 is reserved for the
	// "unset" sentinel in the handler layer. This keeps the token contract
	// strict and prevents a caller from using the fallback value.
	secret := []byte("secret")
	tok, err := Sign(secret, 0, time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	_, err = Parse(secret, tok)
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Parse() route_id 0 error = %v, want ErrInvalidToken", err)
	}
}
