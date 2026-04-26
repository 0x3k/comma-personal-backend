package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
)

// PilotAuthHandler handles POST /v2/pilotauth/, the device registration
// endpoint defined by openpilot in system/athena/registration.py and reused
// by sunnypilot's sunnylink in sunnypilot/sunnylink/api.py. Two flavors share
// the same path:
//
//  1. Comma flow: device sends imei/imei2/serial/public_key/register_token.
//     Server verifies the token, allocates a fresh dongle_id, stores the
//     public_key, and returns {"dongle_id": "..."}.
//  2. Sunnylink flow: device additionally sends `comma_dongle_id` (the dongle
//     id it received from the comma flow earlier). Server links a
//     sunnylink_dongle_id onto the matching devices row and returns
//     {"device_id": "..."}.
//
// The sunnylink branch only succeeds if the submitted public_key matches the
// public_key already stored for that comma_dongle_id, so an attacker who knows
// only the comma_dongle_id cannot hijack a sunnylink identity.
type PilotAuthHandler struct {
	queries *db.Queries
	cfg     *config.Config
}

// NewPilotAuthHandler creates a handler for the pilotauth endpoint.
func NewPilotAuthHandler(queries *db.Queries, cfg *config.Config) *PilotAuthHandler {
	return &PilotAuthHandler{queries: queries, cfg: cfg}
}

// pilotAuthRequest is populated from URL query params (openpilot's
// BaseApi.api_get sends them via requests' params kwarg, not the body) or
// form-encoded body. CommaDongleID is the sunnylink-only field that switches
// the handler into sunnylink registration mode.
type pilotAuthRequest struct {
	IMEI          string `form:"imei" query:"imei" json:"imei"`
	IMEI2         string `form:"imei2" query:"imei2" json:"imei2"`
	Serial        string `form:"serial" query:"serial" json:"serial"`
	PublicKey     string `form:"public_key" query:"public_key" json:"public_key"`
	RegisterToken string `form:"register_token" query:"register_token" json:"register_token"`
	CommaDongleID string `form:"comma_dongle_id" query:"comma_dongle_id" json:"comma_dongle_id"`
}

// pilotAuthResponse matches openpilot's expected {"dongle_id": "..."} shape.
type pilotAuthResponse struct {
	DongleID string `json:"dongle_id"`
}

// sunnylinkPilotAuthResponse matches sunnypilot's expected
// {"device_id": "..."} shape (sunnypilot/sunnylink/api.py reads `device_id`).
type sunnylinkPilotAuthResponse struct {
	DeviceID string `json:"device_id"`
}

// errorResponse is the JSON envelope returned on failure across the api
// package.
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// Handle processes a device registration request. Routes to the comma or the
// sunnylink branch based on the presence of the `comma_dongle_id` form field.
func (h *PilotAuthHandler) Handle(c echo.Context) error {
	var req pilotAuthRequest
	// Echo's DefaultBinder binds body-or-query depending on method; openpilot
	// sends POST with URL query params and no body, which the default binder
	// skips. Read each field from the body first (if any) and fall back to
	// query params.
	_ = c.Bind(&req)
	pickQuery := func(field *string, name string) {
		if *field == "" {
			*field = c.QueryParam(name)
		}
	}
	pickQuery(&req.IMEI, "imei")
	pickQuery(&req.IMEI2, "imei2")
	pickQuery(&req.Serial, "serial")
	pickQuery(&req.PublicKey, "public_key")
	pickQuery(&req.RegisterToken, "register_token")
	pickQuery(&req.CommaDongleID, "comma_dongle_id")

	if req.PublicKey == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "public_key is required",
			Code:  http.StatusBadRequest,
		})
	}
	if req.RegisterToken == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "register_token is required",
			Code:  http.StatusBadRequest,
		})
	}

	if err := verifyRegisterToken(req.RegisterToken, req.PublicKey); err != nil {
		return c.JSON(http.StatusUnauthorized, errorResponse{
			Error: fmt.Sprintf("failed to verify register_token: %s", err.Error()),
			Code:  http.StatusUnauthorized,
		})
	}

	if req.CommaDongleID != "" {
		return h.handleSunnylink(c, req)
	}

	return h.handleComma(c, req)
}

// handleComma is the original comma.ai pilotauth flow. New devices get a
// freshly generated dongle_id; subsequent calls with a known public_key get
// the existing dongle_id back.
func (h *PilotAuthHandler) handleComma(c echo.Context, req pilotAuthRequest) error {
	if !h.cfg.IsSerialAllowed(req.Serial) {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "device not allowed to register",
			Code:  http.StatusForbidden,
		})
	}

	ctx := c.Request().Context()
	pubKeyText := pgtype.Text{String: req.PublicKey, Valid: true}

	existing, err := h.queries.GetDeviceByPublicKey(ctx, pubKeyText)
	if err == nil {
		return c.JSON(http.StatusOK, pilotAuthResponse{DongleID: existing.DongleID})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up device",
			Code:  http.StatusInternalServerError,
		})
	}

	dongleID, err := generateDongleID()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to generate dongle_id",
			Code:  http.StatusInternalServerError,
		})
	}

	device, err := h.queries.CreateDevice(ctx, db.CreateDeviceParams{
		DongleID:  dongleID,
		Serial:    pgtype.Text{String: req.Serial, Valid: req.Serial != ""},
		PublicKey: pubKeyText,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to register device",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.JSON(http.StatusOK, pilotAuthResponse{DongleID: device.DongleID})
}

// handleSunnylink links a sunnylink_dongle_id onto an existing comma device
// row. The device must already have completed the comma registration (so the
// row exists) and its submitted public_key must match the public_key recorded
// during that earlier registration; otherwise an attacker who knows only the
// comma_dongle_id could claim its sunnylink slot. If the device has already
// completed sunnylink registration the existing sunnylink_dongle_id is
// returned to keep the call idempotent.
func (h *PilotAuthHandler) handleSunnylink(c echo.Context, req pilotAuthRequest) error {
	ctx := c.Request().Context()

	device, err := h.queries.GetDevice(ctx, req.CommaDongleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "comma dongle_id not registered",
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up comma device",
			Code:  http.StatusInternalServerError,
		})
	}

	if !device.PublicKey.Valid || device.PublicKey.String != req.PublicKey {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "public_key does not match comma registration",
			Code:  http.StatusForbidden,
		})
	}

	if device.SunnylinkDongleID.Valid && device.SunnylinkDongleID.String != "" {
		return c.JSON(http.StatusOK, sunnylinkPilotAuthResponse{DeviceID: device.SunnylinkDongleID.String})
	}

	sunnylinkID, err := generateDongleID()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to generate sunnylink dongle_id",
			Code:  http.StatusInternalServerError,
		})
	}

	updated, err := h.queries.SetSunnylinkRegistration(ctx, db.SetSunnylinkRegistrationParams{
		DongleID:           req.CommaDongleID,
		SunnylinkDongleID:  pgtype.Text{String: sunnylinkID, Valid: true},
		SunnylinkPublicKey: pgtype.Text{String: req.PublicKey, Valid: true},
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to register sunnylink identity",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.JSON(http.StatusOK, sunnylinkPilotAuthResponse{DeviceID: updated.SunnylinkDongleID.String})
}

// verifyRegisterToken parses the JWT using the provided PEM-encoded public
// key and validates the `register: true` claim. Both RS256 and ES256 are
// accepted to match openpilot's two supported key formats (id_rsa, id_ecdsa).
func verifyRegisterToken(token, publicKeyPEM string) error {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (interface{}, error) {
		switch t.Method.Alg() {
		case jwt.SigningMethodRS256.Alg():
			return jwt.ParseRSAPublicKeyFromPEM([]byte(publicKeyPEM))
		case jwt.SigningMethodES256.Alg():
			return jwt.ParseECPublicKeyFromPEM([]byte(publicKeyPEM))
		default:
			return nil, fmt.Errorf("unsupported signing algorithm: %s", t.Method.Alg())
		}
	}, jwt.WithValidMethods([]string{"RS256", "ES256"}))
	if err != nil {
		return fmt.Errorf("failed to parse token: %w", err)
	}
	if !parsed.Valid {
		return errors.New("token is not valid")
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return errors.New("token has invalid claims")
	}
	reg, _ := claims["register"].(bool)
	if !reg {
		return errors.New("register claim is missing or not true")
	}
	return nil
}

// generateDongleID creates a 16-char lowercase hex identifier for a new
// device, matching the comma.ai dongle_id format. The same generator is used
// for sunnylink_dongle_id; the two namespaces are kept apart by which column
// the value lives in.
func generateDongleID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("failed to read random bytes: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// RegisterRoutes wires up the pilotauth routes on the given Echo instance.
func (h *PilotAuthHandler) RegisterRoutes(e *echo.Echo) {
	e.POST("/v2/pilotauth/", h.Handle)
	e.POST("/v2/pilotauth", h.Handle)
}
