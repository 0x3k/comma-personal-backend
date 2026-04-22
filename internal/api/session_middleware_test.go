package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

func TestSessionAuthHappyPath(t *testing.T) {
	secret := []byte("test-secret")
	user := newTestUser(t, 42, "alice", "pw")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"alice": user}}

	mw := SessionAuth(secret, lookup)
	var gotUserID int32
	handler := mw(func(c echo.Context) error {
		gotUserID, _ = c.Get(ContextKeyUIUserID).(int32)
		return c.NoContent(http.StatusOK)
	})

	token := SignSessionToken(secret, user.ID, time.Now().Add(time.Hour))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/any", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if gotUserID != user.ID {
		t.Errorf("stored user id = %d, want %d", gotUserID, user.ID)
	}
}

func TestSessionAuthMissingCookie(t *testing.T) {
	mw := SessionAuth([]byte("test-secret"), &fakeUserLookup{users: map[string]db.UiUser{}})
	handler := mw(func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/any", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionAuthBadSignature(t *testing.T) {
	user := newTestUser(t, 99, "alice", "pw")
	lookup := &fakeUserLookup{users: map[string]db.UiUser{"alice": user}}
	mw := SessionAuth([]byte("server-secret"), lookup)
	handler := mw(func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	// Sign the cookie with a different secret so the HMAC check fails.
	token := SignSessionToken([]byte("attacker-secret"), user.ID, time.Now().Add(time.Hour))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/any", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionOrJWTAuth_SessionPath(t *testing.T) {
	secret := []byte("test-secret")
	user := newTestUser(t, 7, "alice", "pw")
	userLookup := &fakeUserLookup{users: map[string]db.UiUser{"alice": user}}
	deviceLookup := &stubDeviceLookup{}

	mw := SessionOrJWTAuth(secret, userLookup, deviceLookup)
	handler := mw(func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	token := SignSessionToken(secret, user.ID, time.Now().Add(time.Hour))

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/any", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 via session path; body = %s", rec.Code, rec.Body.String())
	}
	if deviceLookup.called {
		t.Error("JWT lookup should not be called when the session cookie is valid")
	}
}

func TestSessionOrJWTAuth_JWTPath(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	token := signDeviceJWT(t, priv, "abc123")
	device := newTestDevice("abc123", "SERIAL", pubPEM)

	deviceLookup := &stubDeviceLookup{device: device}
	mw := SessionOrJWTAuth([]byte("session-secret"), &fakeUserLookup{users: map[string]db.UiUser{}}, deviceLookup)

	var gotDongleID string
	handler := mw(func(c echo.Context) error {
		gotDongleID, _ = c.Get(middleware.ContextKeyDongleID).(string)
		return c.NoContent(http.StatusOK)
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/any", nil)
	req.Header.Set("Authorization", "JWT "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 via JWT path; body = %s", rec.Code, rec.Body.String())
	}
	if gotDongleID != "abc123" {
		t.Errorf("context dongle = %q, want abc123", gotDongleID)
	}
	if !deviceLookup.called {
		t.Error("JWT lookup should have been invoked when no session cookie is present")
	}
}

func TestSessionOrJWTAuth_NeitherAuth(t *testing.T) {
	deviceLookup := &stubDeviceLookup{}
	mw := SessionOrJWTAuth([]byte("session-secret"), &fakeUserLookup{users: map[string]db.UiUser{}}, deviceLookup)
	handler := mw(func(c echo.Context) error { return c.NoContent(http.StatusOK) })

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/any", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

// stubDeviceLookup is a local middleware.DeviceLookup stub that records
// whether it was called and returns the configured device (or an error).
type stubDeviceLookup struct {
	device *db.Device
	called bool
}

var errStubDeviceMissing = errors.New("stub: no device configured")

func (s *stubDeviceLookup) GetDevice(_ context.Context, _ string) (db.Device, error) {
	s.called = true
	if s.device == nil {
		return db.Device{}, errStubDeviceMissing
	}
	return *s.device, nil
}
