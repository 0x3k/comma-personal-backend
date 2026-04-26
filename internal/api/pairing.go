package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// PairingLookup is the subset of *db.Queries needed to verify a pairing
// JWT: the handler reads the token's `identity` claim and looks up the
// device row to verify the signature against devices.public_key.
type PairingLookup interface {
	GetDevice(ctx context.Context, dongleID string) (db.Device, error)
}

// PairingHandler serves GET /pair?pair=<JWT>, the destination of the QR
// code the device's pairing dialog displays. On a single-tenant personal
// backend the device is implicitly paired with the operator the moment it
// completes pilotauth (see deviceResponse.IsPaired in device.go), so this
// page is purely informational: it decodes the pair token, verifies the
// signature against the device's stored public key, and surfaces the
// dongle_id back to the operator so they can confirm the right device.
type PairingHandler struct {
	lookup PairingLookup
}

// NewPairingHandler creates a handler for the /pair route.
func NewPairingHandler(lookup PairingLookup) *PairingHandler {
	return &PairingHandler{lookup: lookup}
}

// Pair serves GET /pair. It accepts the token via either ?pair=<jwt> (the
// shape the device-side QR generates) or ?token=<jwt> (a friendlier alias
// for testing).
func (h *PairingHandler) Pair(c echo.Context) error {
	token := c.QueryParam("pair")
	if token == "" {
		token = c.QueryParam("token")
	}

	displayDongle := "(unknown)"
	displayStatus := "ready to pair"
	statusOK := true

	if token == "" {
		displayStatus = "missing pair token"
		statusOK = false
	} else if dongleID, verifyErr := h.verify(c.Request().Context(), token); verifyErr != nil {
		displayStatus = "verification failed: " + verifyErr.Error()
		statusOK = false
	} else {
		displayDongle = dongleID
	}

	return c.HTML(http.StatusOK, pairingConfirmationPage(displayDongle, displayStatus, statusOK))
}

// verify decodes the pairing JWT, looks up the device by its `identity`
// claim, verifies the signature against the device's stored public key,
// and asserts the `pair: true` claim is present. Returns the verified
// dongle_id on success.
func (h *PairingHandler) verify(ctx context.Context, token string) (string, error) {
	claimedDongleID, err := middleware.ParseIdentity(token)
	if err != nil {
		return "", fmt.Errorf("parse identity: %w", err)
	}

	device, err := h.lookup.GetDevice(ctx, claimedDongleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("device %q not registered with this backend", claimedDongleID)
		}
		return "", fmt.Errorf("device lookup failed")
	}
	if !device.PublicKey.Valid || device.PublicKey.String == "" {
		return "", fmt.Errorf("device %q has no registered public key", claimedDongleID)
	}
	if err := middleware.VerifySignedToken(token, device.PublicKey.String); err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}
	if err := assertPairClaim(token); err != nil {
		return "", err
	}
	return claimedDongleID, nil
}

// assertPairClaim re-parses the (already-verified) token to check for the
// `pair: true` claim. The signature has been checked in verify(), so this
// pass is unverified; it only inspects the claims dict.
func assertPairClaim(token string) error {
	parser := jwt.NewParser()
	t, _, err := parser.ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return fmt.Errorf("parse claims: %w", err)
	}
	claims, ok := t.Claims.(jwt.MapClaims)
	if !ok {
		return fmt.Errorf("token has no claims")
	}
	pair, _ := claims["pair"].(bool)
	if !pair {
		return fmt.Errorf("token is not a pair token (missing or false `pair` claim)")
	}
	return nil
}

// pairingConfirmationPage renders the same minimal HTML shape used by the
// sunnylink SSO endpoint, so the operator sees a consistent confirmation
// surface regardless of which QR they scanned.
func pairingConfirmationPage(dongleID, status string, ok bool) string {
	statusClass := "ok"
	if !ok {
		statusClass = "err"
	}
	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Device Pairing</title>
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
<h1>Device Pairing</h1>
<div class="row"><div class="k">Device</div><div class="v">` + html.EscapeString(dongleID) + `</div></div>
<div class="row"><div class="k">Status</div><div class="v ` + statusClass + `">` + html.EscapeString(status) + `</div></div>
<p>This personal backend is single-tenant: any device that completes pilotauth registration is automatically paired with the operator. The device's UI will dismiss the pairing dialog within a few seconds of the next /v1.1/devices/&lt;id&gt; poll.</p>
</body>
</html>`
}

// RegisterRoutes wires up GET /pair on the given Echo instance. Public, no
// auth: the operator scanning the QR on their phone may not have a session
// established yet, and on a single-tenant deployment exposing the device
// id back is no leak.
func (h *PairingHandler) RegisterRoutes(e *echo.Echo) {
	e.GET("/pair", h.Pair)
}
