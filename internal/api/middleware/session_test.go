package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/sessioncookie"
)

// fakeDeviceLookup satisfies DeviceLookup with a hand-populated map keyed
// by dongle_id. JWTAuthFromDB fetches the public_key column from each row
// to verify the JWT signature; we only populate the fields it reads.
type fakeDeviceLookup struct {
	devices map[string]db.Device
}

func (f *fakeDeviceLookup) GetDevice(_ context.Context, dongleID string) (db.Device, error) {
	d, ok := f.devices[dongleID]
	if !ok {
		return db.Device{}, pgx.ErrNoRows
	}
	return d, nil
}

// newDevice returns a db.Device with a freshly-generated RSA public key
// stored in the public_key column and the private half returned separately
// so the test can mint tokens signed by it.
func newDevice(t *testing.T, dongleID string) (db.Device, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	dev := db.Device{
		DongleID:  dongleID,
		PublicKey: pgtype.Text{String: string(pemBytes), Valid: true},
	}
	return dev, priv
}

// signDeviceJWT mints an RS256 token with the openpilot claim shape.
func signDeviceJWT(t *testing.T, priv *rsa.PrivateKey, dongleID string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"identity": dongleID,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// probeHandler writes a small JSON body echoing whichever context values
// the middleware stamped. Tests inspect it to assert the correct auth
// mode and user id flowed through.
func probeHandler(c echo.Context) error {
	userID, _ := c.Get(ContextKeyUserID).(int32)
	mode, _ := c.Get(ContextKeyAuthMode).(string)
	dongleID, _ := c.Get(ContextKeyDongleID).(string)
	return c.JSON(http.StatusOK, map[string]any{
		"user_id":   userID,
		"mode":      mode,
		"dongle_id": dongleID,
	})
}

// invokeMiddleware runs the middleware chain against the given request and
// returns the response recorder plus the decoded JSON body. Keeping the
// plumbing in one place keeps the actual test bodies short.
func invokeMiddleware(mw echo.MiddlewareFunc, req *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = mw(probeHandler)(c)

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

// validSessionCookie mints a session cookie signed with the given secret
// that expires an hour from now.
func validSessionCookie(secret []byte, userID int32) *http.Cookie {
	token := sessioncookie.Sign(secret, userID, time.Now().Add(time.Hour))
	return &http.Cookie{Name: sessioncookie.Name, Value: token}
}

func TestSessionRequiredValidCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	mw := SessionRequired(secret)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(validSessionCookie(secret, 42))

	rec, body := invokeMiddleware(mw, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	// JSON numbers come back as float64 after unmarshal into map[string]any.
	if got, want := body["user_id"], float64(42); got != want {
		t.Errorf("user_id = %v, want %v", got, want)
	}
	if got, want := body["mode"], AuthModeSession; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

func TestSessionRequiredMissingCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	mw := SessionRequired(secret)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)

	rec, _ := invokeMiddleware(mw, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionRequiredExpiredCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	mw := SessionRequired(secret)

	// Signed with an expiry in the past. sessioncookie.Parse compares
	// against time.Now(); an hour ago is well past that.
	token := sessioncookie.Sign(secret, 7, time.Now().Add(-time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: sessioncookie.Name, Value: token})

	rec, _ := invokeMiddleware(mw, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for expired cookie", rec.Code)
	}
}

func TestSessionRequiredTamperedCookie(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	mw := SessionRequired(secret)

	// Sign with one secret, verify with another: must reject.
	fakeToken := sessioncookie.Sign([]byte("wrong-secret"), 9, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: sessioncookie.Name, Value: fakeToken})

	rec, _ := invokeMiddleware(mw, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for tampered cookie", rec.Code)
	}

	// Also test a structurally broken cookie (no dot, no base64) which
	// should hit the "malformed" branch rather than the signature branch.
	req2 := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req2.AddCookie(&http.Cookie{Name: sessioncookie.Name, Value: "not-a-cookie"})
	rec2, _ := invokeMiddleware(mw, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for malformed cookie", rec2.Code)
	}
}

func TestSessionRequiredOpenMode(t *testing.T) {
	// Empty secret => open mode => every request passes through.
	mw := SessionRequired(nil)

	// No cookie, no header: still 200. This mirrors the acceptance
	// criterion that UI auth-disabled deployments do not gate anything.
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rec, body := invokeMiddleware(mw, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in open mode", rec.Code)
	}
	if got, want := body["mode"], AuthModeSession; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}

	// An explicitly-empty []byte{} secret should behave the same.
	rec2, _ := invokeMiddleware(SessionRequired([]byte{}), req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with zero-length secret", rec2.Code)
	}
}

func TestSessionOrJWTCookieFirst(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	// No device lookup entries: this test proves the cookie path does
	// not touch the DB at all when the cookie is valid.
	mw := SessionOrJWT(secret, &fakeDeviceLookup{devices: map[string]db.Device{}})

	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	req.AddCookie(validSessionCookie(secret, 123))

	rec, body := invokeMiddleware(mw, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := body["user_id"], float64(123); got != want {
		t.Errorf("user_id = %v, want %v", got, want)
	}
	if got, want := body["mode"], AuthModeSession; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
	if got := body["dongle_id"]; got != "" {
		t.Errorf("dongle_id should be empty for session auth, got %v", got)
	}
}

func TestSessionOrJWTFallsBackToJWT(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	dev, priv := newDevice(t, "dongle-xyz")
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{dev.DongleID: dev}}
	mw := SessionOrJWT(secret, lookup)

	// No cookie: should flow to the JWT path.
	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	req.Header.Set("Authorization", "Bearer "+signDeviceJWT(t, priv, dev.DongleID))

	rec, body := invokeMiddleware(mw, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := body["dongle_id"], dev.DongleID; got != want {
		t.Errorf("dongle_id = %v, want %v", got, want)
	}
	if got, want := body["mode"], AuthModeJWT; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

func TestSessionOrJWTInvalidCookieFallsThroughToJWT(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	dev, priv := newDevice(t, "dongle-xyz")
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{dev.DongleID: dev}}
	mw := SessionOrJWT(secret, lookup)

	// A stale cookie (wrong secret) must not lock out a device that
	// also carries a valid JWT.
	stale := sessioncookie.Sign([]byte("different-secret"), 1, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	req.AddCookie(&http.Cookie{Name: sessioncookie.Name, Value: stale})
	req.Header.Set("Authorization", "Bearer "+signDeviceJWT(t, priv, dev.DongleID))

	rec, body := invokeMiddleware(mw, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := body["mode"], AuthModeJWT; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

func TestSessionOrJWTNoCredentials(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{}}
	mw := SessionOrJWT(secret, lookup)

	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	rec, _ := invokeMiddleware(mw, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when no creds are supplied", rec.Code)
	}

	// Body should be our standard error envelope, not the JWT one,
	// because the dashboard hits this path when the user isn't logged in.
	if !strings.Contains(rec.Body.String(), "authentication required") {
		t.Errorf("expected session 401 copy, got %s", rec.Body.String())
	}
}

func TestSessionOrJWTBadJWT(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	dev, _ := newDevice(t, "dongle-xyz")
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{dev.DongleID: dev}}
	mw := SessionOrJWT(secret, lookup)

	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	rec, _ := invokeMiddleware(mw, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for malformed JWT", rec.Code)
	}
}

func TestSessionOrJWTOpenMode(t *testing.T) {
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{}}
	mw := SessionOrJWT(nil, lookup)

	// No cookie, no header: must still pass in open mode.
	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	rec, body := invokeMiddleware(mw, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 in open mode", rec.Code)
	}
	if got, want := body["mode"], AuthModeSession; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
}

func TestSessionOrJWTOpenModeStillAcceptsJWT(t *testing.T) {
	dev, priv := newDevice(t, "dongle-xyz")
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{dev.DongleID: dev}}
	mw := SessionOrJWT(nil, lookup)

	// Even in open mode we want a device JWT to set the dongle_id
	// context so the handlers can enforce their ownership checks.
	req := httptest.NewRequest(http.MethodGet, "/mixed", nil)
	req.Header.Set("Authorization", "Bearer "+signDeviceJWT(t, priv, dev.DongleID))
	rec, body := invokeMiddleware(mw, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := body["mode"], AuthModeJWT; got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
	if got, want := body["dongle_id"], dev.DongleID; got != want {
		t.Errorf("dongle_id = %v, want %v", got, want)
	}
}

// TestSessionRequiredWrongCookieName ensures the middleware does not mistake
// a JWT cookie (used by /ws/ auth on openpilot's side) or some other cookie
// for a session cookie.
func TestSessionRequiredWrongCookieName(t *testing.T) {
	secret := []byte("test-secret-32-bytes-xxxxxxxxxxx")
	mw := SessionRequired(secret)

	valid := sessioncookie.Sign(secret, 5, time.Now().Add(time.Hour))
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "jwt", Value: valid}) // wrong name

	rec, _ := invokeMiddleware(mw, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when cookie name mismatches", rec.Code)
	}
}

// Sanity: the errorResponse type this package uses for 401 bodies should
// match the project envelope shape.
func TestUnauthorizedEnvelope(t *testing.T) {
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	c := e.NewContext(req, rec)
	_ = unauthorizedSession(c)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", body.Code)
	}
	if body.Error == "" {
		t.Errorf("error should be populated; got %q", body.Error)
	}
}

// compile-time assertion that fakeDeviceLookup satisfies DeviceLookup,
// so a breaking change to the interface shows up here rather than at
// runtime in a test further down.
var _ DeviceLookup = (*fakeDeviceLookup)(nil)

// _ is a tiny helper printf referenced nowhere; keeps fmt imported if
// another test needs it later.
var _ = fmt.Sprintf
