package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/sessioncookie"
)

// ContextKeyUserID is the Echo context key used to store the authenticated
// UI user's database ID once SessionRequired or SessionOrJWT has validated
// the session cookie.
const ContextKeyUserID = "ui_user_id"

// ContextKeyAuthMode is the Echo context key used to record which
// credential the request authenticated with: "session" or "jwt". Handlers
// can use it to branch behaviour between humans and devices (for example,
// skipping the "authDongleID must match the URL :dongle_id" check when the
// caller is an operator with a session cookie).
const ContextKeyAuthMode = "auth_mode"

// AuthModeSession / AuthModeJWT are the two values stored under
// ContextKeyAuthMode by the session middleware family.
const (
	AuthModeSession = "session"
	AuthModeJWT     = "jwt"
)

// SessionRequired returns Echo middleware that requires a valid signed
// session cookie. The cookie's name, signature scheme, and expiry are
// handled by the sessioncookie package.
//
// When secret is empty, UI auth is disabled (open mode): the middleware
// becomes a no-op pass-through. The caller is responsible for logging a
// single startup warning -- per-request log spam would be unhelpful.
//
// On success, the middleware stores the authenticated user ID under
// ContextKeyUserID and the string "session" under ContextKeyAuthMode.
func SessionRequired(secret []byte) echo.MiddlewareFunc {
	if len(secret) == 0 {
		return openMode()
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userID, err := parseSessionCookie(c, secret)
			if err != nil {
				return unauthorizedSession(c)
			}
			c.Set(ContextKeyUserID, userID)
			c.Set(ContextKeyAuthMode, AuthModeSession)
			return next(c)
		}
	}
}

// SessionOrJWT returns Echo middleware that accepts either a valid signed
// session cookie OR a valid device JWT. The cookie is tried first because
// operators hitting the dashboard in a browser never carry an Authorization
// header; only if there is no session cookie (or it is invalid) does the
// middleware fall through to JWTAuthFromDB-style validation.
//
// In open mode (empty secret) the middleware still accepts JWT-authenticated
// requests but lets cookie-less requests pass through; this matches the
// acceptance criterion "SessionRequired/SessionOrJWT degrade to allowing
// all" while keeping device auth functional for the endpoints that share
// a group with the dashboard read path.
//
// On success the middleware records which credential was accepted under
// ContextKeyAuthMode so handlers can tell a human apart from a device.
func SessionOrJWT(secret []byte, lookup DeviceLookup) echo.MiddlewareFunc {
	openSession := len(secret) == 0
	jwtMW := JWTAuthFromDB(lookup)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Try the session cookie first. Any cookie failure falls
			// through to the JWT path so a stale cookie on a device
			// doesn't lock it out.
			if !openSession {
				if userID, err := parseSessionCookie(c, secret); err == nil {
					c.Set(ContextKeyUserID, userID)
					c.Set(ContextKeyAuthMode, AuthModeSession)
					return next(c)
				}
			}

			// No (or invalid) cookie. If the request doesn't carry an
			// Authorization header, decide by open-mode state: in open
			// mode we let it through (the dashboard has no cookie); in
			// closed mode we return 401 without attempting JWT parsing,
			// matching the dashboard's expected error shape.
			if _, err := extractToken(c); err != nil {
				if openSession {
					c.Set(ContextKeyAuthMode, AuthModeSession)
					return next(c)
				}
				return unauthorizedSession(c)
			}

			// Authorization header is present: delegate to the real JWT
			// middleware, which verifies the token against the device's
			// registered public key. Wrap the next handler so we can
			// stamp the auth mode without duplicating the logic.
			wrapped := func(c echo.Context) error {
				c.Set(ContextKeyAuthMode, AuthModeJWT)
				return next(c)
			}
			return jwtMW(wrapped)(c)
		}
	}
}

// openMode returns middleware that is a pure pass-through but still stamps
// ContextKeyAuthMode so downstream handlers can tell the request was not
// authenticated via a live session. Used when SESSION_SECRET is unset.
func openMode() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(ContextKeyAuthMode, AuthModeSession)
			return next(c)
		}
	}
}

// parseSessionCookie reads the session cookie from the request and verifies
// its signature + expiry. It returns a non-nil error if no cookie is
// present or the cookie is invalid; the error is intentionally coarse
// because it is surfaced back to the client only as a generic 401.
func parseSessionCookie(c echo.Context, secret []byte) (int32, error) {
	cookie, err := c.Cookie(sessioncookie.Name)
	if err != nil {
		return 0, err
	}
	return sessioncookie.Parse(secret, cookie.Value)
}

// unauthorizedSession writes the project-standard 401 envelope for a
// failed session check. Kept distinct from the JWT 401 copy so log greps
// can tell the two apart.
func unauthorizedSession(c echo.Context) error {
	return c.JSON(http.StatusUnauthorized, errorResponse{
		Error: "authentication required",
		Code:  http.StatusUnauthorized,
	})
}
