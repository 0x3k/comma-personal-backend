package main

import (
	"encoding/base64"
	"testing"

	"comma-personal-backend/internal/config"
)

// TestCheckALPRKeyring_NilAndUnset confirms the startup self-check is a
// no-op when ALPR is not opted into (cfg nil or empty key). Disabled
// deployments must not pay any startup cost or risk a spurious failure.
func TestCheckALPRKeyring_NilAndUnset(t *testing.T) {
	if err := checkALPRKeyring(nil); err != nil {
		t.Fatalf("nil cfg: want nil, got %v", err)
	}
	if err := checkALPRKeyring(&config.ALPRConfig{}); err != nil {
		t.Fatalf("empty key: want nil, got %v", err)
	}
}

// TestCheckALPRKeyring_Valid confirms a real key passes the round-trip.
func TestCheckALPRKeyring_Valid(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	cfg := &config.ALPRConfig{EncryptionKeyB64: base64.StdEncoding.EncodeToString(raw)}
	if err := checkALPRKeyring(cfg); err != nil {
		t.Fatalf("valid key: want nil, got %v", err)
	}
}

// TestCheckALPRKeyring_BadKey confirms malformed keys surface as errors
// rather than silent boots into a broken state.
func TestCheckALPRKeyring_BadKey(t *testing.T) {
	cases := []struct {
		name string
		b64  string
	}{
		{"not-base64", "!!!not-base64!!!"},
		{"too-short", base64.StdEncoding.EncodeToString(make([]byte, 16))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.ALPRConfig{EncryptionKeyB64: c.b64}
			if err := checkALPRKeyring(cfg); err == nil {
				t.Fatalf("%s: want non-nil error", c.name)
			}
		})
	}
}
