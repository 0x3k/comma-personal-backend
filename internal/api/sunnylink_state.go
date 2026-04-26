package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// decodeSSOStateBase64 attempts both standard and URL-safe base64 with
// optional padding, since the QR encoder may produce either flavor.
func decodeSSOStateBase64(s string) (string, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("not valid base64")
}

// SunnylinkStateLookup is the subset of *db.Queries needed to authenticate
// sunnylink device-facing endpoints. Kept narrow so tests can stub a
// single method.
type SunnylinkStateLookup interface {
	GetDeviceBySunnylinkDongleID(ctx context.Context, sunnylinkDongleID pgtype.Text) (db.Device, error)
}

// SunnylinkStateHandler serves the device-facing endpoints sunnylink_state.py
// polls every five seconds: roles (sponsor tier) and users (pairing). Both
// are read-only and authenticated with a sunnylink Bearer JWT.
//
// Single-tenant deployment: every device that has completed sunnylink
// registration is implicitly paired with the operator and granted the
// highest sponsor tier so the corresponding gated features unlock.
type SunnylinkStateHandler struct {
	lookup SunnylinkStateLookup
}

// NewSunnylinkStateHandler creates a handler for the sunnylink state
// endpoints (/device/:sunnylink_dongle_id/roles, .../users) and the /sso
// landing page.
func NewSunnylinkStateHandler(lookup SunnylinkStateLookup) *SunnylinkStateHandler {
	return &SunnylinkStateHandler{lookup: lookup}
}

// sunnylinkRole matches sunnypilot/sunnylink/sunnylink_state.py's Role.
type sunnylinkRole struct {
	RoleType string `json:"role_type"`
	RoleTier string `json:"role_tier"`
}

// sunnylinkUser matches sunnypilot/sunnylink/sunnylink_state.py's User.
type sunnylinkUser struct {
	DeviceID  string `json:"device_id"`
	UserID    string `json:"user_id"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	TokenHash string `json:"token_hash"`
}

// authenticateSunnylink verifies that the request carries a valid sunnylink
// Bearer JWT whose `identity` claim matches the path-supplied
// sunnylink_dongle_id and whose signature checks against the device's
// stored sunnylink_public_key. Returns the device row on success; on
// failure, writes a JSON error response and returns (Device{}, true, err).
func (h *SunnylinkStateHandler) authenticateSunnylink(c echo.Context, pathDongleID string) (device db.Device, handled bool, err error) {
	tokenStr := extractBearerOrJWT(c.Request().Header.Get("Authorization"))
	if tokenStr == "" {
		return db.Device{}, true, c.JSON(http.StatusUnauthorized, errorResponse{
			Error: "missing authorization token",
			Code:  http.StatusUnauthorized,
		})
	}

	claimedDongleID, err := middleware.ParseIdentity(tokenStr)
	if err != nil {
		return db.Device{}, true, c.JSON(http.StatusUnauthorized, errorResponse{
			Error: err.Error(),
			Code:  http.StatusUnauthorized,
		})
	}
	if claimedDongleID != pathDongleID {
		return db.Device{}, true, c.JSON(http.StatusForbidden, errorResponse{
			Error: "token dongle_id does not match path dongle_id",
			Code:  http.StatusForbidden,
		})
	}

	d, err := h.lookup.GetDeviceBySunnylinkDongleID(
		c.Request().Context(),
		pgtype.Text{String: pathDongleID, Valid: true},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Device{}, true, c.JSON(http.StatusUnauthorized, errorResponse{
				Error: "sunnylink device not registered",
				Code:  http.StatusUnauthorized,
			})
		}
		return db.Device{}, true, c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up device",
			Code:  http.StatusInternalServerError,
		})
	}
	if !d.SunnylinkPublicKey.Valid || d.SunnylinkPublicKey.String == "" {
		return db.Device{}, true, c.JSON(http.StatusUnauthorized, errorResponse{
			Error: "sunnylink device has no registered public key",
			Code:  http.StatusUnauthorized,
		})
	}
	if err := middleware.VerifySignedToken(tokenStr, d.SunnylinkPublicKey.String); err != nil {
		return db.Device{}, true, c.JSON(http.StatusUnauthorized, errorResponse{
			Error: fmt.Sprintf("failed to validate token: %s", err.Error()),
			Code:  http.StatusUnauthorized,
		})
	}
	return d, false, nil
}

// GetRoles serves GET /device/:sunnylink_dongle_id/roles.
//
// Returns a single SPONSOR role at the GUARDIAN tier so every gated feature
// unlocks. The personal backend does not maintain a real sponsorship
// hierarchy; the operator owns the device outright.
func (h *SunnylinkStateHandler) GetRoles(c echo.Context) error {
	pathID := c.Param("sunnylink_dongle_id")
	if _, handled, err := h.authenticateSunnylink(c, pathID); handled {
		return err
	}
	return c.JSON(http.StatusOK, []sunnylinkRole{{
		RoleType: "SPONSOR",
		RoleTier: "GUARDIAN",
	}})
}

// GetUsers serves GET /device/:sunnylink_dongle_id/users.
//
// Returns a single synthetic operator user. SunnylinkState.is_paired() is
// true when at least one user.user_id is not in
// {"unregisteredsponsor", "temporarysponsor"}, so any concrete user_id
// flips the device into the "paired" state.
func (h *SunnylinkStateHandler) GetUsers(c echo.Context) error {
	pathID := c.Param("sunnylink_dongle_id")
	device, handled, err := h.authenticateSunnylink(c, pathID)
	if handled {
		return err
	}

	createdAt := int64(0)
	updatedAt := int64(0)
	if device.CreatedAt.Valid {
		createdAt = device.CreatedAt.Time.Unix()
	}
	if device.UpdatedAt.Valid {
		updatedAt = device.UpdatedAt.Time.Unix()
	}

	return c.JSON(http.StatusOK, []sunnylinkUser{{
		DeviceID:  device.SunnylinkDongleID.String,
		UserID:    "operator",
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		TokenHash: "local",
	}})
}

// SSO serves GET /sso?state=<base64>. The QR code on the device's pairing
// dialog points here. Decodes the state for display, attempts a best-effort
// signature check (purely informational on a single-tenant deployment), and
// renders a small HTML confirmation page.
//
// The state payload format is `1|<sunnylink_dongle_id>|<token>` per
// sunnylink_pairing_dialog.py. We surface the dongle id back to the user so
// they can confirm they're claiming the right device.
func (h *SunnylinkStateHandler) SSO(c echo.Context) error {
	state := c.QueryParam("state")

	displayDongle := "(unknown)"
	displayStatus := "ready to pair"
	statusOK := true

	if state == "" {
		displayStatus = "missing state parameter"
		statusOK = false
	} else {
		dongleID, _, decodeErr := decodeSunnylinkSSOState(state)
		if decodeErr != nil {
			displayStatus = "invalid state: " + decodeErr.Error()
			statusOK = false
		} else if dongleID != "" {
			displayDongle = dongleID
			// Best-effort confirmation that the device is registered.
			if _, err := h.lookup.GetDeviceBySunnylinkDongleID(
				c.Request().Context(),
				pgtype.Text{String: dongleID, Valid: true},
			); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					displayStatus = "device not registered with this backend"
					statusOK = false
				} else {
					displayStatus = "lookup failed"
					statusOK = false
				}
			}
		}
	}

	return c.HTML(http.StatusOK, sunnylinkSSOPage(displayDongle, displayStatus, statusOK))
}

// decodeSunnylinkSSOState decodes a base64-encoded `1|<dongle>|<token>`
// payload. The leading "1|" is a version tag we accept but ignore. Returns
// the dongle_id and token, or an error if the payload is malformed.
func decodeSunnylinkSSOState(state string) (string, string, error) {
	raw, err := decodeSSOStateBase64(state)
	if err != nil {
		return "", "", fmt.Errorf("base64 decode: %w", err)
	}
	parts := strings.SplitN(raw, "|", 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("expected 3 segments separated by '|', got %d", len(parts))
	}
	if parts[0] != "1" {
		return "", "", fmt.Errorf("unsupported state version %q", parts[0])
	}
	if parts[1] == "" {
		return "", "", fmt.Errorf("empty sunnylink dongle id")
	}
	return parts[1], parts[2], nil
}

// sunnylinkSSOPage returns a minimal HTML confirmation page. We escape the
// user-supplied fields so the page is safe to render even if a malformed
// state somehow injects HTML metacharacters.
func sunnylinkSSOPage(dongleID, status string, ok bool) string {
	statusClass := "ok"
	if !ok {
		statusClass = "err"
	}
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Sunnylink Pairing</title>
<style>
  body { font-family: -apple-system, system-ui, sans-serif; margin: 2em auto; max-width: 480px; line-height: 1.4; }
  h1 { font-size: 1.4em; }
  .row { padding: 0.5em 0; border-bottom: 1px solid #ddd; }
  .row .k { color: #666; font-size: 0.9em; }
  .row .v { font-family: monospace; word-break: break-all; }
  .ok { color: #1a7f37; font-weight: bold; }
  .err { color: #b91c1c; font-weight: bold; }
</style>
</head>
<body>
<h1>Sunnylink Pairing</h1>
<div class="row"><div class="k">Device</div><div class="v">` + html.EscapeString(dongleID) + `</div></div>
<div class="row"><div class="k">Status</div><div class="v ` + statusClass + `">` + html.EscapeString(status) + `</div></div>
<p>This personal backend is single-tenant: any device that completes sunnylink registration is automatically paired with the operator. No further action is required.</p>
</body>
</html>`
}

// extractBearerOrJWT reads "JWT <token>" or "Bearer <token>" from a header
// value. Returns the token (with whitespace trimmed) or "" if neither
// scheme matches.
func extractBearerOrJWT(authHeader string) string {
	if authHeader == "" {
		return ""
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "JWT") && !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// RegisterRoutes wires up the device-facing sunnylink state endpoints and
// the /sso landing page.
func (h *SunnylinkStateHandler) RegisterRoutes(e *echo.Echo) {
	e.GET("/device/:sunnylink_dongle_id/roles", h.GetRoles)
	e.GET("/device/:sunnylink_dongle_id/users", h.GetUsers)
	e.GET("/sso", h.SSO)
}
