package api

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// ContextKeyUIUserID is the Echo context key used to carry the authenticated
// UI user's ID when a request arrives with a valid session cookie. It is
// namespaced separately from middleware.ContextKeyDongleID so handlers can
// distinguish a device-authenticated request from an operator-authenticated
// one.
const ContextKeyUIUserID = "ui_user_id"

// SessionAuth returns Echo middleware that authenticates a request against
// the signed session cookie set by POST /v1/session/login. When the cookie
// is missing, expired, or tampered with, the middleware replies 401 with the
// project-standard error envelope. On success it stores the UI user ID in
// the Echo context under ContextKeyUIUserID.
//
// The handler is intentionally strict: if sessionSecret is empty, every
// request is rejected. Callers should guard registration with
// cfg.UIAuthEnabled() so the middleware is never installed with an empty
// secret in production.
func SessionAuth(sessionSecret []byte, users UserLookup) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userID, err := authenticateSession(c, sessionSecret, users)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, errorResponse{
					Error: "authentication required",
					Code:  http.StatusUnauthorized,
				})
			}
			c.Set(ContextKeyUIUserID, userID)
			return next(c)
		}
	}
}

// SessionOrJWTAuth returns middleware that accepts either a signed session
// cookie (like SessionAuth) or a device-issued JWT that matches the dongle
// stored in the database (like middleware.JWTAuthFromDB). This gives the UI
// and the device both a path to the endpoint without forcing the device to
// own a cookie or the operator to mint a JWT. When sessionSecret is empty
// only the JWT path is available.
//
// The session check runs first because it is cheap (HMAC verification, no
// DB lookup) and because the UI is the typical caller; the JWT fallback is
// there mainly for parity with other authenticated device-facing endpoints.
func SessionOrJWTAuth(sessionSecret []byte, users UserLookup, devices middleware.DeviceLookup) echo.MiddlewareFunc {
	jwtMW := middleware.JWTAuthFromDB(devices)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		jwtHandler := jwtMW(next)

		return func(c echo.Context) error {
			if len(sessionSecret) > 0 {
				if userID, err := authenticateSession(c, sessionSecret, users); err == nil {
					c.Set(ContextKeyUIUserID, userID)
					return next(c)
				}
			}
			return jwtHandler(c)
		}
	}
}

// authenticateSession reads the signed session cookie off the request,
// verifies its HMAC against sessionSecret, and confirms the embedded user
// ID still exists in the ui_users table. It returns the user's ID on
// success or a non-nil error on any failure mode.
func authenticateSession(c echo.Context, sessionSecret []byte, users UserLookup) (int32, error) {
	cookie, err := c.Cookie(SessionCookieName)
	if err != nil {
		return 0, err
	}
	userID, err := ParseSessionCookie(sessionSecret, cookie.Value)
	if err != nil {
		return 0, err
	}
	if users != nil {
		if _, err := users.GetUIUserByID(c.Request().Context(), userID); err != nil {
			return 0, err
		}
	}
	return userID, nil
}

// Compile-time check that db.Queries satisfies the UserLookup interface. Keeps
// a build-time guarantee that the narrow interface stays aligned with the
// generated code.
var _ UserLookup = (*db.Queries)(nil)
