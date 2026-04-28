package crypto

import (
	"errors"
	"fmt"
)

// ProbeString is the fixed plaintext used by the startup self-check to
// verify the keyring round-trips. Kept as a package-level constant so
// the test asserting startup behaviour and the actual call site agree.
const ProbeString = "alpr-probe"

// VerifyRoundtrip encrypts and decrypts ProbeString through both the
// plate and label code paths and returns nil if both succeed and the
// plaintext matches. Used by the server bootstrap to fail fast with a
// clear error if the loaded keyring is in any way unusable.
func VerifyRoundtrip(k *Keyring) error {
	if k == nil {
		return errors.New("alpr/crypto: VerifyRoundtrip called with nil keyring")
	}

	// Plate domain.
	ct, err := k.Encrypt(ProbeString)
	if err != nil {
		return fmt.Errorf("alpr/crypto: probe encrypt failed: %w", err)
	}
	got, err := k.Decrypt(ct)
	if err != nil {
		return fmt.Errorf("alpr/crypto: probe decrypt failed: %w", err)
	}
	if got != ProbeString {
		return fmt.Errorf("alpr/crypto: probe roundtrip mismatch: got %q want %q", got, ProbeString)
	}

	// Label domain.
	lct, err := k.EncryptLabel(ProbeString)
	if err != nil {
		return fmt.Errorf("alpr/crypto: probe label encrypt failed: %w", err)
	}
	lgot, err := k.DecryptLabel(lct)
	if err != nil {
		return fmt.Errorf("alpr/crypto: probe label decrypt failed: %w", err)
	}
	if lgot != ProbeString {
		return fmt.Errorf("alpr/crypto: probe label roundtrip mismatch: got %q want %q", lgot, ProbeString)
	}

	return nil
}
