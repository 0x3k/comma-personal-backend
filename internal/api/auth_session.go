package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
)

// SessionOrJWT returns Echo middleware that authenticates a request via either
// the web UI session cookie or a device-issued JWT. It is intended for routes
// that the browser dashboard needs to reach (session cookie) but that a device
// should also be able to reach (JWT). Exactly one of the two mechanisms must
// succeed; otherwise the request is rejected with 401.
//
// Behaviour:
//   - If a session cookie named SessionCookieName is present and validates
//     against the configured sessionSecret, the request is authorised as a UI
//     user. ContextKeyDongleID is populated from the URL :dongle_id path param
//     (when present) so downstream handlers that compare against the context
//     key still work for UI requests.
//   - Otherwise, the Authorization header is parsed as a JWT and verified
//     against the device's stored public key, using the same flow as
//     middleware.JWTAuthFromDB. On success, ContextKeyDongleID is set from the
//     token claims.
//   - If neither succeeds, the request is rejected with 401.
//
// sessionSecret may be empty; in that case only the JWT branch is available
// (equivalent to middleware.JWTAuthFromDB).
func SessionOrJWT(sessionSecret string, lookup middleware.DeviceLookup) echo.MiddlewareFunc {
	secret := []byte(sessionSecret)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if len(secret) > 0 {
				if cookie, err := c.Cookie(SessionCookieName); err == nil && cookie != nil && cookie.Value != "" {
					if _, err := ParseSessionCookie(secret, cookie.Value); err == nil {
						if id := c.Param("dongle_id"); id != "" {
							c.Set(middleware.ContextKeyDongleID, id)
						}
						return next(c)
					}
				}
			}

			tokenString, err := extractBearerToken(c)
			if err != nil {
				return unauthorizedSession(c, "missing session cookie or authorization token")
			}

			dongleID, err := middleware.ParseIdentity(tokenString)
			if err != nil {
				return unauthorizedSession(c, err.Error())
			}

			device, err := lookup.GetDevice(c.Request().Context(), dongleID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return unauthorizedSession(c, "device not registered")
				}
				return c.JSON(http.StatusInternalServerError, errorResponse{
					Error: "failed to look up device",
					Code:  http.StatusInternalServerError,
				})
			}
			if !device.PublicKey.Valid || device.PublicKey.String == "" {
				return unauthorizedSession(c, "device has no registered public key")
			}

			if err := middleware.VerifySignedToken(tokenString, device.PublicKey.String); err != nil {
				return unauthorizedSession(c, "failed to validate token: "+err.Error())
			}

			c.Set(middleware.ContextKeyDongleID, dongleID)
			return next(c)
		}
	}
}

// extractBearerToken reads the JWT token string from the Authorization header.
// It mirrors middleware.extractToken (which is unexported) so the SessionOrJWT
// middleware can reuse the same parsing rules without duplicating package
// internals in tests.
func extractBearerToken(c echo.Context) (string, error) {
	auth := c.Request().Header.Get("Authorization")
	if auth == "" {
		return "", errors.New("missing authorization header")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 {
		return "", errors.New("malformed authorization header")
	}
	scheme := parts[0]
	if !strings.EqualFold(scheme, "JWT") && !strings.EqualFold(scheme, "Bearer") {
		return "", errors.New("unsupported authorization scheme")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errors.New("empty authorization token")
	}
	return token, nil
}

func unauthorizedSession(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, errorResponse{
		Error: msg,
		Code:  http.StatusUnauthorized,
	})
}
