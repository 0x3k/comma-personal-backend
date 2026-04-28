// Package crypto provides authenticated encryption and stable-identity
// hashing for ALPR plate text and watchlist labels.
//
// The single root secret is loaded from the ALPR_ENCRYPTION_KEY environment
// variable (base64-encoded, at least 32 bytes of raw material). LoadKeyring
// derives three independent subkeys from that root via HKDF-SHA256:
//
//   - enc_key   (32 B): AES-256-GCM encryption key for plate/label text.
//   - hash_salt (32 B): HMAC-SHA256 salt used to produce a stable identity
//     hash that survives across re-encryption.
//   - aad       (16 B): additional authenticated data prefix; combined with
//     a domain tag ("plate" or "label") it binds ciphertext to its purpose
//     so a plate ciphertext cannot be successfully decrypted as a label.
//
// All ALPR plate-text writers MUST go through this package. Mixing in
// independent uses of aes.NewCipher / hmac.New for plate identifiers would
// break the single-key-rotation invariant.
//
// # Key rotation = data loss
//
// Rotating ALPR_ENCRYPTION_KEY invalidates ALL prior ciphertexts AND all
// prior hashes coherently: the AES key changes (so existing ciphertexts no
// longer decrypt) and the HMAC salt changes (so historical plate_hash rows
// no longer match the new keyring's Hash output). This is intentional --
// it means a single rotation moves the entire dataset to a fresh identity
// space without any partial-rotation state to reason about. From the
// operator's POV this is a destructive operation: existing plate reads
// are not recoverable after rotation. Document this clearly in any UI
// that exposes the key.
//
// # CLI helper for generating a key
//
//	go run ./cmd/alpr-keygen
//
// prints a base64-encoded 32-byte random value suitable for use as
// ALPR_ENCRYPTION_KEY.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// Sizes of derived material. Exposed as constants so tests can assert on
// them without re-deriving the values.
const (
	// EncKeySize is the size of the AES-256-GCM key derived from the root.
	EncKeySize = 32
	// HashSaltSize is the size of the HMAC-SHA256 salt derived from the root.
	HashSaltSize = 32
	// AADSize is the size of the additional-authenticated-data prefix.
	AADSize = 16
	// NonceSize is the AES-GCM nonce size in bytes.
	NonceSize = 12
	// HashSize is the size of the HMAC-SHA256 output in bytes.
	HashSize = 32

	// MinRootKeyBytes is the minimum number of raw bytes required of the
	// decoded ALPR_ENCRYPTION_KEY. 32 bytes (256 bits) is the floor.
	MinRootKeyBytes = 32
)

// HKDF info strings. These are versioned so that, if a future revision
// needs to derive different material, callers can migrate by introducing
// e.g. "alpr-encrypt-v2" rather than reusing the same info string for
// new material (which would silently produce identical bytes).
const (
	infoEncKey   = "alpr-encrypt-v1"
	infoHashSalt = "alpr-hash-v1"
	infoAAD      = "alpr-aad-v1"
)

// AAD domain tags. Combined with the AAD prefix to bind ciphertext to its
// purpose so a plate ciphertext cannot be successfully decrypted as a label.
const (
	domainPlate = "plate"
	domainLabel = "label"
)

// Errors returned by the keyring helpers. ErrAuthFailed is the generic
// error returned for any GCM open failure; we deliberately do not
// distinguish between a malformed nonce, a truncated tag, or active
// tampering, because the caller cannot do anything useful with that
// distinction and surfacing it leaks attacker-useful signal.
var (
	ErrAuthFailed         = errors.New("alpr/crypto: authentication failed (tampered ciphertext or wrong key)")
	ErrEmptyKey           = errors.New("alpr/crypto: ALPR_ENCRYPTION_KEY is empty")
	ErrKeyTooShort        = fmt.Errorf("alpr/crypto: ALPR_ENCRYPTION_KEY must decode to at least %d bytes", MinRootKeyBytes)
	ErrInvalidBase64      = errors.New("alpr/crypto: ALPR_ENCRYPTION_KEY is not valid base64")
	ErrEmptyPlaintext     = errors.New("alpr/crypto: refusing to encrypt empty plaintext")
	ErrCiphertextTooSmall = errors.New("alpr/crypto: ciphertext too small to contain nonce and tag")
)

// Keyring holds the derived encryption key, hash salt, and AAD prefix for
// the lifetime of the process. It is safe for concurrent use: the AEAD
// itself is stateless after construction and the salt/AAD are immutable
// byte slices we never write to.
type Keyring struct {
	aead     cipher.AEAD
	hashSalt []byte // 32 B
	aad      []byte // 16 B
}

// LoadKeyring decodes the base64 root key, validates length, and derives
// the three subkeys via HKDF-SHA256. Returns a typed error so callers can
// distinguish "not configured" from "misconfigured" -- the former is
// expected when ALPR is disabled; the latter is fatal at startup.
//
// CAUTION: rotating the input value invalidates all previously stored
// plate_ciphertext / label_ciphertext / plate_hash data. There is no
// supported migration path; the rotation is whole-dataset.
func LoadKeyring(b64 string) (*Keyring, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, ErrEmptyKey
	}

	root, err := decodeBase64Flexible(b64)
	if err != nil {
		return nil, ErrInvalidBase64
	}
	if len(root) < MinRootKeyBytes {
		return nil, ErrKeyTooShort
	}

	encKey, err := hkdfDerive(root, []byte(infoEncKey), EncKeySize)
	if err != nil {
		return nil, fmt.Errorf("alpr/crypto: derive enc_key: %w", err)
	}
	hashSalt, err := hkdfDerive(root, []byte(infoHashSalt), HashSaltSize)
	if err != nil {
		return nil, fmt.Errorf("alpr/crypto: derive hash_salt: %w", err)
	}
	aad, err := hkdfDerive(root, []byte(infoAAD), AADSize)
	if err != nil {
		return nil, fmt.Errorf("alpr/crypto: derive aad: %w", err)
	}

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("alpr/crypto: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("alpr/crypto: cipher.NewGCM: %w", err)
	}

	return &Keyring{
		aead:     aead,
		hashSalt: hashSalt,
		aad:      aad,
	}, nil
}

// Encrypt returns nonce || ciphertext_with_tag, AES-256-GCM, with AAD
// = aad_prefix || "plate". Empty plaintext is rejected as a caller bug:
// every call site has a non-empty plate string, and silently encrypting
// "" would mask logic errors upstream.
func (k *Keyring) Encrypt(plaintext string) ([]byte, error) {
	return k.encryptWithDomain(plaintext, domainPlate)
}

// Decrypt is the inverse of Encrypt. Returns ErrAuthFailed for any
// authentication failure (tamper, wrong key, wrong domain).
func (k *Keyring) Decrypt(ciphertext []byte) (string, error) {
	return k.decryptWithDomain(ciphertext, domainPlate)
}

// EncryptLabel is the watchlist-label variant of Encrypt. AAD domain is
// "label" instead of "plate" so a plate ciphertext written by Encrypt
// cannot be decrypted as a label by DecryptLabel and vice versa, which
// catches accidental cross-column writes at the AEAD layer.
func (k *Keyring) EncryptLabel(plaintext string) ([]byte, error) {
	return k.encryptWithDomain(plaintext, domainLabel)
}

// DecryptLabel is the inverse of EncryptLabel.
func (k *Keyring) DecryptLabel(ciphertext []byte) (string, error) {
	return k.decryptWithDomain(ciphertext, domainLabel)
}

// Hash returns HMAC-SHA256(hash_salt, normalize(plaintext)). The output
// is 32 bytes. Same input -> same output for the lifetime of the keyring.
// Different formatting of the same plate ("abc 123" vs "ABC-123" vs
// "abc.123") yields the same hash, so cross-trip plate matching works
// even when OCR returns inconsistent punctuation.
func (k *Keyring) Hash(plaintext string) []byte {
	mac := hmac.New(sha256.New, k.hashSalt)
	mac.Write([]byte(normalize(plaintext)))
	return mac.Sum(nil)
}

func (k *Keyring) encryptWithDomain(plaintext, domain string) ([]byte, error) {
	if plaintext == "" {
		return nil, ErrEmptyPlaintext
	}
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("alpr/crypto: read nonce: %w", err)
	}
	aad := k.aadFor(domain)
	// Pre-allocate a single buffer that holds nonce || ciphertext so we
	// avoid an extra copy after Seal.
	out := make([]byte, NonceSize, NonceSize+len(plaintext)+k.aead.Overhead())
	copy(out, nonce)
	out = k.aead.Seal(out, nonce, []byte(plaintext), aad)
	return out, nil
}

func (k *Keyring) decryptWithDomain(ciphertext []byte, domain string) (string, error) {
	if len(ciphertext) < NonceSize+k.aead.Overhead() {
		return "", ErrCiphertextTooSmall
	}
	nonce := ciphertext[:NonceSize]
	body := ciphertext[NonceSize:]
	aad := k.aadFor(domain)
	plaintext, err := k.aead.Open(nil, nonce, body, aad)
	if err != nil {
		return "", ErrAuthFailed
	}
	return string(plaintext), nil
}

// aadFor returns aad_prefix || domain. We allocate a fresh slice each
// time because cipher.AEAD.Seal/Open does not mutate the AAD slice, but
// returning a shared backing array would invite future bugs if a caller
// ever mutated it.
func (k *Keyring) aadFor(domain string) []byte {
	out := make([]byte, 0, len(k.aad)+len(domain))
	out = append(out, k.aad...)
	out = append(out, domain...)
	return out
}

// normalize folds case and strips characters that OCR engines commonly
// emit inconsistently around plate digits and letters. Keep this list
// SHORT and stable: every change invalidates historical hashes.
//
// Stripped: ASCII space, '-', '.', and tab. Keep all other characters
// (including unicode whitespace) so we do not mask oddities that may
// indicate genuinely different reads.
func normalize(s string) string {
	upper := strings.ToUpper(s)
	// Avoid strings.Builder for such a small input -- a single allocation
	// of cap=len(upper) is plenty.
	out := make([]byte, 0, len(upper))
	for i := 0; i < len(upper); i++ {
		c := upper[i]
		switch c {
		case ' ', '-', '.', '\t':
			continue
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// decodeBase64Flexible accepts either standard or unpadded standard
// base64. (URL-encoded variants are deliberately not accepted -- the env
// var is documented as standard base64.)
func decodeBase64Flexible(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// hkdfDerive runs HKDF-SHA256 with an empty salt (the root key is the
// IKM and we rely on info-string domain separation, which is the standard
// recipe for "single PRK, multiple labelled outputs"). Returns out bytes.
func hkdfDerive(root, info []byte, out int) ([]byte, error) {
	r := hkdf.New(sha256.New, root, nil, info)
	buf := make([]byte, out)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
