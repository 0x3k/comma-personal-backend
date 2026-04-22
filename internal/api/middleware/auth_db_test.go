package middleware

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// stubLookup is a test DeviceLookup that returns a fixed device (or a fixed
// error) regardless of the dongle_id passed in. We record the id that was
// looked up so tests can assert the middleware reads identity from the token.
type stubLookup struct {
	device    db.Device
	err       error
	gotDongle string
}

func (s *stubLookup) GetDevice(_ context.Context, dongleID string) (db.Device, error) {
	s.gotDongle = dongleID
	if s.err != nil {
		return db.Device{}, s.err
	}
	return s.device, nil
}

func rsaPEM(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal rsa pub: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
}

func ecdsaPEM(t *testing.T, pub *ecdsa.PublicKey) string {
	t.Helper()
	b, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal ecdsa pub: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
}

func deviceWithPEM(dongleID, publicKeyPEM string) db.Device {
	now := time.Now()
	return db.Device{
		DongleID:  dongleID,
		PublicKey: pgtype.Text{String: publicKeyPEM, Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func signRS256(t *testing.T, priv *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("sign rs256: %v", err)
	}
	return signed
}

func signES256(t *testing.T, priv *ecdsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("sign es256: %v", err)
	}
	return signed
}

func runAuth(mw echo.MiddlewareFunc, authHeader string) *httptest.ResponseRecorder {
	e := echo.New()
	handler := func(c echo.Context) error {
		dongle, _ := c.Get(ContextKeyDongleID).(string)
		return c.String(http.StatusOK, dongle)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = mw(handler)(c)
	return rec
}

func TestJWTAuthFromDB_RS256HappyPath(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	dev := deviceWithPEM("abc123", rsaPEM(t, &priv.PublicKey))
	lookup := &stubLookup{device: dev}

	token := signRS256(t, priv, jwt.MapClaims{
		"identity": "abc123",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "JWT "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "abc123" {
		t.Errorf("handler saw dongle %q, want abc123", rec.Body.String())
	}
	if lookup.gotDongle != "abc123" {
		t.Errorf("GetDevice called with %q, want abc123", lookup.gotDongle)
	}
}

func TestJWTAuthFromDB_ES256HappyPath(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdsa: %v", err)
	}
	dev := deviceWithPEM("ec-dongle", ecdsaPEM(t, &priv.PublicKey))
	lookup := &stubLookup{device: dev}

	token := signES256(t, priv, jwt.MapClaims{
		"identity": "ec-dongle",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "JWT "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestJWTAuthFromDB_WrongKey(t *testing.T) {
	realPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	attackerPriv, _ := rsa.GenerateKey(rand.Reader, 2048)

	dev := deviceWithPEM("abc123", rsaPEM(t, &realPriv.PublicKey))
	lookup := &stubLookup{device: dev}

	token := signRS256(t, attackerPriv, jwt.MapClaims{
		"identity": "abc123",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "JWT "+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestJWTAuthFromDB_UnknownDongle(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	lookup := &stubLookup{err: pgx.ErrNoRows}

	token := signRS256(t, priv, jwt.MapClaims{
		"identity": "never-registered",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "JWT "+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestJWTAuthFromDB_ExpiredToken(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	dev := deviceWithPEM("abc123", rsaPEM(t, &priv.PublicKey))
	lookup := &stubLookup{device: dev}

	token := signRS256(t, priv, jwt.MapClaims{
		"identity": "abc123",
		"iat":      time.Now().Add(-2 * time.Hour).Unix(),
		"exp":      time.Now().Add(-time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "JWT "+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestJWTAuthFromDB_AlgMismatch(t *testing.T) {
	// Device is registered with an RSA public key but the attacker signs
	// with ECDSA claiming the same identity. The verifier must reject the
	// alg/key combination.
	realPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	attackerPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	dev := deviceWithPEM("abc123", rsaPEM(t, &realPriv.PublicKey))
	lookup := &stubLookup{device: dev}

	token := signES256(t, attackerPriv, jwt.MapClaims{
		"identity": "abc123",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "JWT "+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestJWTAuthFromDB_MissingToken(t *testing.T) {
	lookup := &stubLookup{}
	rec := runAuth(JWTAuthFromDB(lookup), "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
}

func TestJWTAuthFromDB_MalformedToken(t *testing.T) {
	lookup := &stubLookup{}
	rec := runAuth(JWTAuthFromDB(lookup), "JWT not.a.jwt")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if lookup.gotDongle != "" {
		t.Errorf("lookup should not have been called for a malformed token, got %q", lookup.gotDongle)
	}
}

func TestJWTAuthFromDB_AcceptsBearerScheme(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	dev := deviceWithPEM("abc123", rsaPEM(t, &priv.PublicKey))
	lookup := &stubLookup{device: dev}

	token := signRS256(t, priv, jwt.MapClaims{
		"identity": "abc123",
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	rec := runAuth(JWTAuthFromDB(lookup), "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}
