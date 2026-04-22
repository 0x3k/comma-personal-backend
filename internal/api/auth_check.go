package api

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
)

// checkDongleAccess enforces per-device authorization on handlers that
// live behind the SessionOrJWT middleware chain. The rules:
//
//   - If the caller authenticated with an operator session cookie, they
//     are allowed to target any dongle_id. The dashboard operator is
//     trusted; there are no per-device permissions in this single-user
//     service.
//   - If the caller authenticated with a device JWT (athenad / openpilot)
//     or the auth mode is missing (pure JWT-only routes), they may only
//     act on their own dongle_id. This prevents a compromised device
//     from reading or mutating another device's state.
//
// The function returns (handled bool, err error). When handled is true,
// a 403 response has been written and the caller must return err
// immediately without producing any further body. When handled is false,
// err is nil and the handler should proceed; this mirrors the inline
// pattern the handlers used before the session middleware landed, where
// c.JSON returned nil on success so the forbidden branch looked the
// same to tests that inspected only rec.Code.
func checkDongleAccess(c echo.Context, dongleID string) (handled bool, err error) {
	mode, _ := c.Get(middleware.ContextKeyAuthMode).(string)
	if mode == middleware.AuthModeSession {
		return false, nil
	}
	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID == dongleID {
		return false, nil
	}
	return true, c.JSON(http.StatusForbidden, errorResponse{
		Error: "dongle_id does not match authenticated device",
		Code:  http.StatusForbidden,
	})
}
