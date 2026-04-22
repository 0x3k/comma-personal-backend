package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// fakeDeviceLookup is a minimal middleware.DeviceLookup implementation driven
// by a map keyed on dongle_id. Missing entries return pgx.ErrNoRows which
// JWTAuthFromDB turns into a 401.
type fakeDeviceLookup struct {
	devices map[string]db.Device
	err     error
}

func (f *fakeDeviceLookup) GetDevice(_ context.Context, dongleID string) (db.Device, error) {
	if f.err != nil {
		return db.Device{}, f.err
	}
	d, ok := f.devices[dongleID]
	if !ok {
		return db.Device{}, fmt.Errorf("device not found: %s", dongleID)
	}
	return d, nil
}

// okHandler is the echo handler mounted behind the combined-auth middleware
// in these tests. It asserts which auth context key was populated by the
// middleware and responds with 200 on success.
func okHandler(wantUserID bool, wantDongleID string) echo.HandlerFunc {
	return func(c echo.Context) error {
		if wantUserID {
			if _, ok := c.Get(ContextKeyUIUserID).(int32); !ok {
				return c.JSON(http.StatusInternalServerError, errorResponse{
					Error: "expected ui_user_id in context",
					Code:  http.StatusInternalServerError,
				})
			}
		}
		if wantDongleID != "" {
			got, _ := c.Get(middleware.ContextKeyDongleID).(string)
			if got != wantDongleID {
				return c.JSON(http.StatusInternalServerError, errorResponse{
					Error: fmt.Sprintf("dongle_id = %q, want %q", got, wantDongleID),
					Code:  http.StatusInternalServerError,
				})
			}
		}
		return c.NoContent(http.StatusOK)
	}
}

func TestSessionOrJWTAuth_ValidSessionCookie(t *testing.T) {
	secret := []byte("test-secret")
	expiresAt := time.Now().Add(1 * time.Hour)
	token := SignSessionToken(secret, 42, expiresAt)

	lookup := &fakeDeviceLookup{devices: map[string]db.Device{}}
	mw := SessionOrJWTAuth(secret, lookup)

	e := echo.New()
	e.GET("/protected", okHandler(true, ""), mw)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionOrJWTAuth_InvalidSessionFallsBackToJWT(t *testing.T) {
	secret := []byte("test-secret")
	priv, pubPEM := testDeviceKey(t)
	token := signDeviceJWT(t, priv, "abc123")

	device := newTestDevice("abc123", "SERIAL", pubPEM)
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{"abc123": *device}}
	mw := SessionOrJWTAuth(secret, lookup)

	e := echo.New()
	e.GET("/protected", okHandler(false, "abc123"), mw)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	// Send a cookie that was signed with a different secret: the middleware
	// must fall through to JWT validation instead of rejecting outright.
	badToken := SignSessionToken([]byte("other-secret"), 42, time.Now().Add(time.Hour))
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: badToken})
	req.Header.Set("Authorization", "JWT "+token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionOrJWTAuth_NoCredentials(t *testing.T) {
	secret := []byte("test-secret")
	mw := SessionOrJWTAuth(secret, &fakeDeviceLookup{devices: map[string]db.Device{}})

	e := echo.New()
	e.GET("/protected", okHandler(false, ""), mw)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestSessionOrJWTAuth_EmptySecretAllowsJWTOnly(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	token := signDeviceJWT(t, priv, "abc123")

	device := newTestDevice("abc123", "SERIAL", pubPEM)
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{"abc123": *device}}

	// Empty secret simulates SESSION_SECRET not being set. JWT must still
	// work; the session branch is effectively disabled.
	mw := SessionOrJWTAuth(nil, lookup)

	e := echo.New()
	e.GET("/protected", okHandler(false, "abc123"), mw)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "JWT "+token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSessionOrJWTAuth_BothSetSessionWins(t *testing.T) {
	secret := []byte("test-secret")
	token := SignSessionToken(secret, 7, time.Now().Add(time.Hour))

	// Populate JWT machinery with a different dongle_id so we can detect
	// whether the middleware fell through to JWT auth. It must not.
	priv, pubPEM := testDeviceKey(t)
	jwtToken := signDeviceJWT(t, priv, "abc123")
	device := newTestDevice("abc123", "SERIAL", pubPEM)
	lookup := &fakeDeviceLookup{devices: map[string]db.Device{"abc123": *device}}

	mw := SessionOrJWTAuth(secret, lookup)

	e := echo.New()
	e.GET("/protected", func(c echo.Context) error {
		if _, ok := c.Get(ContextKeyUIUserID).(int32); !ok {
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "expected session auth to win when both are valid",
				Code:  http.StatusInternalServerError,
			})
		}
		if got, _ := c.Get(middleware.ContextKeyDongleID).(string); got != "" {
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "did not expect dongle_id to be set when session auth wins",
				Code:  http.StatusInternalServerError,
			})
		}
		return c.NoContent(http.StatusOK)
	}, mw)

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	req.Header.Set("Authorization", "JWT "+jwtToken)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}
