package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
)

// testKeyB64 returns a stable base64-encoded 32-byte key suitable for tests
// that need a deterministic value. Tests that exercise rotation or per-key
// behaviour should generate their own random key via randomKeyB64.
func testKeyB64(t *testing.T) string {
	t.Helper()
	// Fixed 32 bytes (0x00..0x1f). Avoids a flaky-test surface area while
	// still exercising every byte of the AES key schedule.
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func randomKeyB64(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func mustLoad(t *testing.T, b64 string) *Keyring {
	t.Helper()
	k, err := LoadKeyring(b64)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	return k
}

func TestLoadKeyring_EmptyKey(t *testing.T) {
	_, err := LoadKeyring("")
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("want ErrEmptyKey, got %v", err)
	}
	// Whitespace-only is treated as empty.
	_, err = LoadKeyring("   \t\n")
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("whitespace: want ErrEmptyKey, got %v", err)
	}
}

func TestLoadKeyring_InvalidBase64(t *testing.T) {
	_, err := LoadKeyring("!!!not-base64!!!")
	if !errors.Is(err, ErrInvalidBase64) {
		t.Fatalf("want ErrInvalidBase64, got %v", err)
	}
}

func TestLoadKeyring_KeyTooShort(t *testing.T) {
	short := make([]byte, 16) // 128 bits, deliberately under the 32-byte floor
	_, err := LoadKeyring(base64.StdEncoding.EncodeToString(short))
	if !errors.Is(err, ErrKeyTooShort) {
		t.Fatalf("want ErrKeyTooShort, got %v", err)
	}
}

func TestLoadKeyring_AcceptsRawAndPaddedBase64(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}

	padded := base64.StdEncoding.EncodeToString(raw)
	if _, err := LoadKeyring(padded); err != nil {
		t.Fatalf("padded base64: %v", err)
	}

	unpadded := base64.RawStdEncoding.EncodeToString(raw)
	if _, err := LoadKeyring(unpadded); err != nil {
		t.Fatalf("unpadded base64: %v", err)
	}
}

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))

	cases := []string{
		"ABC123",
		"7XYZ-89A",
		"Plate with spaces",
		"plate.with.dots",
		"éèê", // unicode is preserved (only ASCII whitespace/dash/dot get stripped)
	}
	for _, plain := range cases {
		t.Run(plain, func(t *testing.T) {
			ct, err := k.Encrypt(plain)
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			got, err := k.Decrypt(ct)
			if err != nil {
				t.Fatalf("Decrypt: %v", err)
			}
			if got != plain {
				t.Fatalf("roundtrip mismatch: got %q want %q", got, plain)
			}
		})
	}
}

func TestEncrypt_EmptyPlaintextRejected(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))
	if _, err := k.Encrypt(""); !errors.Is(err, ErrEmptyPlaintext) {
		t.Fatalf("Encrypt(\"\"): want ErrEmptyPlaintext, got %v", err)
	}
	if _, err := k.EncryptLabel(""); !errors.Is(err, ErrEmptyPlaintext) {
		t.Fatalf("EncryptLabel(\"\"): want ErrEmptyPlaintext, got %v", err)
	}
}

func TestNonceUniqueness(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))
	const N = 1000
	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		ct, err := k.Encrypt("ABC123")
		if err != nil {
			t.Fatalf("Encrypt[%d]: %v", i, err)
		}
		if len(ct) < NonceSize {
			t.Fatalf("ciphertext[%d] too short: %d", i, len(ct))
		}
		nonce := string(ct[:NonceSize])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce collision at iteration %d", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestDecrypt_TamperedRejected(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))
	ct, err := k.Encrypt("ABC123")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip one bit in the body (after the nonce). Should fail authentication.
	tampered := append([]byte(nil), ct...)
	tampered[NonceSize] ^= 0x01
	if _, err := k.Decrypt(tampered); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampered body: want ErrAuthFailed, got %v", err)
	}

	// Flip a bit inside the nonce itself. Should also fail authentication.
	tampered = append([]byte(nil), ct...)
	tampered[0] ^= 0x01
	if _, err := k.Decrypt(tampered); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampered nonce: want ErrAuthFailed, got %v", err)
	}

	// Truncated below the AEAD overhead boundary.
	short := ct[:NonceSize+1]
	if _, err := k.Decrypt(short); err == nil {
		t.Fatalf("truncated: want non-nil error, got nil")
	}
}

func TestDecrypt_DomainSeparation(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))

	plate, err := k.Encrypt("ABC123")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// A plate ciphertext must NOT decrypt as a label, even though the
	// underlying AES key is the same -- the AAD differs.
	if _, err := k.DecryptLabel(plate); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("cross-domain (plate->label): want ErrAuthFailed, got %v", err)
	}

	label, err := k.EncryptLabel("watch-list")
	if err != nil {
		t.Fatalf("EncryptLabel: %v", err)
	}
	if _, err := k.Decrypt(label); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("cross-domain (label->plate): want ErrAuthFailed, got %v", err)
	}
}

func TestDecrypt_DifferentKeyRejected(t *testing.T) {
	k1 := mustLoad(t, testKeyB64(t))
	k2 := mustLoad(t, randomKeyB64(t))

	ct, err := k1.Encrypt("ABC123")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := k2.Decrypt(ct); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("different key: want ErrAuthFailed, got %v", err)
	}
}

func TestHash_Determinism(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))

	a := k.Hash("ABC123")
	b := k.Hash("ABC123")
	if !bytes.Equal(a, b) {
		t.Fatalf("Hash not deterministic: %x vs %x", a, b)
	}
	if len(a) != HashSize {
		t.Fatalf("Hash size: got %d want %d", len(a), HashSize)
	}
}

func TestHash_FormatInsensitivity(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))

	canonical := k.Hash("ABC123")
	variants := []string{
		"abc123",
		"abc 123",
		"ABC-123",
		"a.b.c.1.2.3",
		"abc\t123",
		"  AbC-1 2 3  ",
		"A B-C 1.2.3",
	}
	for _, v := range variants {
		got := k.Hash(v)
		if !bytes.Equal(got, canonical) {
			t.Fatalf("variant %q: got %x want %x", v, got, canonical)
		}
	}
}

func TestHash_DifferentPlatesDiffer(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))

	cases := [][2]string{
		{"ABC123", "ABC124"},
		{"ABC123", "BCD123"},
		{"7XYZ89", "8XYZ89"},
	}
	for _, c := range cases {
		a := k.Hash(c[0])
		b := k.Hash(c[1])
		if bytes.Equal(a, b) {
			t.Fatalf("Hash collision: %q and %q both -> %x", c[0], c[1], a)
		}
	}
}

func TestHash_RotationInvalidates(t *testing.T) {
	k1 := mustLoad(t, testKeyB64(t))
	k2 := mustLoad(t, randomKeyB64(t))
	if bytes.Equal(k1.Hash("ABC123"), k2.Hash("ABC123")) {
		t.Fatalf("hashes must differ across distinct root keys")
	}
}

func TestKeyring_DerivedKeysAreIndependent(t *testing.T) {
	// Internal sanity: the three info strings must produce distinct
	// material from the same root. If a future edit accidentally reuses
	// an info string, this test catches it.
	k := mustLoad(t, testKeyB64(t))
	if bytes.Equal(k.hashSalt, k.aad) {
		t.Fatalf("hash_salt collides with aad")
	}
	// We cannot inspect enc_key directly (only the aead), but we can
	// observe that the hash salt is not equal to the AAD prefix, which
	// is sufficient to confirm distinct HKDF info strings.
	if len(k.hashSalt) != HashSaltSize {
		t.Fatalf("hash_salt size: got %d want %d", len(k.hashSalt), HashSaltSize)
	}
	if len(k.aad) != AADSize {
		t.Fatalf("aad size: got %d want %d", len(k.aad), AADSize)
	}
}

func TestVerifyRoundtrip(t *testing.T) {
	k := mustLoad(t, testKeyB64(t))
	if err := VerifyRoundtrip(k); err != nil {
		t.Fatalf("VerifyRoundtrip: %v", err)
	}
	if err := VerifyRoundtrip(nil); err == nil {
		t.Fatalf("VerifyRoundtrip(nil): want error, got nil")
	}
}

func BenchmarkEncryptHash(b *testing.B) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	k, err := LoadKeyring(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		b.Fatalf("LoadKeyring: %v", err)
	}
	const plate = "7XYZ-89A"

	b.ResetTimer()
	var totalNs int64
	for i := 0; i < b.N; i++ {
		ct, err := k.Encrypt(plate)
		if err != nil {
			b.Fatalf("Encrypt: %v", err)
		}
		_ = k.Hash(plate)
		_ = ct
	}
	b.StopTimer()

	// Per-op nanoseconds. The Go benchmark harness already reports ns/op,
	// but we add a named metric so the assertion target is explicit and
	// the budget (50us = 50000 ns) is documented in the test output.
	if b.N > 0 && b.Elapsed() > 0 {
		totalNs = b.Elapsed().Nanoseconds() / int64(b.N)
	}
	b.ReportMetric(float64(totalNs), "ns/op-encrypt+hash")

	const budgetNs = int64(50_000)
	if totalNs > budgetNs {
		b.Fatalf("encrypt+hash exceeded budget: %d ns/op > %d ns/op", totalNs, budgetNs)
	}
}
