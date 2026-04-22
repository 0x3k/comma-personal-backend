package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// deviceLookupStub satisfies middleware.DeviceLookup for tests without
// pulling in a real db.Queries.
type deviceLookupStub struct {
	device *db.Device
	err    error
}

func (s *deviceLookupStub) GetDevice(_ context.Context, _ string) (db.Device, error) {
	if s.err != nil {
		return db.Device{}, s.err
	}
	if s.device == nil {
		return db.Device{}, nil
	}
	return *s.device, nil
}

func TestSessionOrJWTAllowsValidSession(t *testing.T) {
	secret := "test-secret"
	token := SignSessionToken([]byte(secret), 1, time.Now().Add(time.Hour))

	lookup := &deviceLookupStub{}

	called := false
	wrapped := SessionOrJWT(secret, lookup)(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := wrapped(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !called {
		t.Fatal("expected wrapped handler to be invoked for a valid session")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, _ := c.Get(middleware.ContextKeyDongleID).(string); got != "abc123" {
		t.Errorf("context dongle_id = %q, want %q", got, "abc123")
	}
}

func TestSessionOrJWTRejectsMissingAuth(t *testing.T) {
	lookup := &deviceLookupStub{}
	wrapped := SessionOrJWT("test-secret", lookup)(func(c echo.Context) error {
		t.Fatal("wrapped handler should not be called")
		return nil
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := wrapped(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSessionOrJWTRejectsTamperedSession(t *testing.T) {
	lookup := &deviceLookupStub{}
	wrapped := SessionOrJWT("test-secret", lookup)(func(c echo.Context) error {
		t.Fatal("wrapped handler should not be called")
		return nil
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "not-a-real.session"})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := wrapped(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestSessionOrJWTAcceptsValidJWT(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	device := newTestDevice("abc123", "SERIAL001", pubPEM)
	lookup := &deviceLookupStub{device: device}

	token := signDeviceJWT(t, priv, "abc123")

	called := false
	wrapped := SessionOrJWT("test-secret", lookup)(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	req.Header.Set("Authorization", "JWT "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := wrapped(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !called {
		t.Fatal("expected wrapped handler to be invoked for a valid JWT")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, _ := c.Get(middleware.ContextKeyDongleID).(string); got != "abc123" {
		t.Errorf("context dongle_id = %q, want %q", got, "abc123")
	}
}

func TestSessionOrJWTEmptySecretRequiresJWT(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	device := newTestDevice("abc123", "SERIAL001", pubPEM)
	lookup := &deviceLookupStub{device: device}

	token := signDeviceJWT(t, priv, "abc123")

	called := false
	wrapped := SessionOrJWT("", lookup)(func(c echo.Context) error {
		called = true
		return c.NoContent(http.StatusOK)
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	req.Header.Set("Authorization", "JWT "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := wrapped(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if !called {
		t.Error("expected wrapped handler to be invoked with valid JWT and empty session secret")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
