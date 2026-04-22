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
// endpoint defined by openpilot in system/athena/registration.py. The device
// sends its hardware identity (imei, imei2, serial), its freshly-generated
// public key, and a register_token JWT signed with the matching private key.
// The backend verifies the token against the public key and returns a
// server-assigned dongle_id (or the existing one if the public key is already
// registered).
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
// form-encoded body. Both JSON and query binding work via Echo's DefaultBinder.
type pilotAuthRequest struct {
	IMEI          string `form:"imei" query:"imei" json:"imei"`
	IMEI2         string `form:"imei2" query:"imei2" json:"imei2"`
	Serial        string `form:"serial" query:"serial" json:"serial"`
	PublicKey     string `form:"public_key" query:"public_key" json:"public_key"`
	RegisterToken string `form:"register_token" query:"register_token" json:"register_token"`
}

// pilotAuthResponse matches openpilot's expected {"dongle_id": "..."} shape.
type pilotAuthResponse struct {
	DongleID string `json:"dongle_id"`
}

// errorResponse is the JSON envelope returned on failure across the api
// package.
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// Handle processes a device registration request.
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
// device, matching the comma.ai dongle_id format.
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
