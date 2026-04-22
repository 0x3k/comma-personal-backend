package api

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
)

// ContextKeyUIUserID is the Echo context key used to store the authenticated
// UI user ID when a request is validated via a session cookie. Handlers that
// accept either auth method can look it up to distinguish operator-initiated
// requests from device-initiated ones.
const ContextKeyUIUserID = "ui_user_id"

// SessionOrJWTAuth returns Echo middleware that accepts either:
//
//   - A valid signed session cookie (as minted by SessionHandler.Login), OR
//   - A valid device JWT signed by the device's private key
//
// The session path is attempted first so an operator with both a cookie and
// a lingering Authorization header is not silently denied by a stale JWT.
// When the session secret is empty (i.e. UI auth is disabled), the middleware
// falls through to JWT-only. Unauthenticated requests receive a 401.
//
// On success the authenticated identity is stored in the Echo context:
//   - session cookie  => ContextKeyUIUserID (int32)
//   - device JWT      => middleware.ContextKeyDongleID (string)
//
// Handlers use those keys to apply per-device authorization. A request that
// carries a session cookie is treated as an operator and can target any
// registered device; a JWT-auth request must match the target device.
func SessionOrJWTAuth(sessionSecret []byte, lookup middleware.DeviceLookup) echo.MiddlewareFunc {
	jwtMW := middleware.JWTAuthFromDB(lookup)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		jwtHandler := jwtMW(next)

		return func(c echo.Context) error {
			// Try session cookie first. A malformed or expired cookie is not
			// fatal here; we simply fall through to JWT auth.
			if len(sessionSecret) > 0 {
				if cookie, err := c.Cookie(SessionCookieName); err == nil && cookie.Value != "" {
					userID, err := ParseSessionCookie(sessionSecret, cookie.Value)
					if err == nil {
						c.Set(ContextKeyUIUserID, userID)
						return next(c)
					}
				}
			}

			// No valid session cookie, so require a JWT. If that also fails
			// the JWT middleware will write a 401 response itself.
			if hasAuthHeader(c) {
				return jwtHandler(c)
			}

			return c.JSON(http.StatusUnauthorized, errorResponse{
				Error: "authentication required",
				Code:  http.StatusUnauthorized,
			})
		}
	}
}

// hasAuthHeader reports whether the request carries a Bearer/JWT
// Authorization header. It is used to decide whether to run the JWT
// middleware (which responds with 401 when the header is missing) or
// short-circuit to a uniform "auth required" response.
func hasAuthHeader(c echo.Context) bool {
	return c.Request().Header.Get("Authorization") != ""
}
