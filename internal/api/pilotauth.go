package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
)

// PilotAuthHandler handles the POST /v2/pilotauth/ endpoint for device
// registration. Comma devices call this on first boot or re-registration
// to obtain a JWT token.
type PilotAuthHandler struct {
	queries   *db.Queries
	jwtSecret string
	cfg       *config.Config
}

// NewPilotAuthHandler creates a handler for the pilotauth endpoint.
func NewPilotAuthHandler(queries *db.Queries, jwtSecret string, cfg *config.Config) *PilotAuthHandler {
	return &PilotAuthHandler{
		queries:   queries,
		jwtSecret: jwtSecret,
		cfg:       cfg,
	}
}

// pilotAuthRequest is the expected JSON body for POST /v2/pilotauth/.
type pilotAuthRequest struct {
	DongleID  string `json:"dongle_id" form:"dongle_id"`
	PublicKey string `json:"public_key" form:"public_key"`
	Serial    string `json:"serial" form:"serial"`
}

// pilotAuthResponse is the JSON body returned on success.
type pilotAuthResponse struct {
	Token string `json:"access_token"`
}

// errorResponse is the JSON envelope returned on failure.
type errorResponse struct {
	Error string `json:"error"`
	Code  int    `json:"code"`
}

// Handle processes a device registration request. It validates the input,
// upserts the device record, and returns a signed JWT.
func (h *PilotAuthHandler) Handle(c echo.Context) error {
	var req pilotAuthRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	if req.DongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	if req.PublicKey == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "public_key is required",
			Code:  http.StatusBadRequest,
		})
	}

	if !h.cfg.IsDongleAllowed(req.DongleID) {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "device not allowed to register",
			Code:  http.StatusForbidden,
		})
	}

	device, err := h.queries.UpsertDevice(c.Request().Context(), db.UpsertDeviceParams{
		DongleID:  req.DongleID,
		Serial:    pgtype.Text{String: req.Serial, Valid: req.Serial != ""},
		PublicKey: pgtype.Text{String: req.PublicKey, Valid: true},
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to register device",
			Code:  http.StatusInternalServerError,
		})
	}

	token, err := h.signToken(device.DongleID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to generate token",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.JSON(http.StatusOK, pilotAuthResponse{
		Token: token,
	})
}

// signToken creates a JWT with dongle_id in the claims, signed with HS256.
func (h *PilotAuthHandler) signToken(dongleID string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"dongle_id": dongleID,
		"identity":  dongleID,
		"iat":       now.Unix(),
		"exp":       now.Add(90 * 24 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(h.jwtSecret))
	if err != nil {
		return "", fmt.Errorf("failed to sign token: %w", err)
	}

	return signed, nil
}

// RegisterRoutes wires up the pilotauth routes on the given Echo instance.
func (h *PilotAuthHandler) RegisterRoutes(e *echo.Echo) {
	e.POST("/v2/pilotauth/", h.Handle)
	e.POST("/v2/pilotauth", h.Handle)
}
