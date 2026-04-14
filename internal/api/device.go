package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// DeviceHandler handles device-related API endpoints.
type DeviceHandler struct {
	queries *db.Queries
}

// NewDeviceHandler creates a handler for device endpoints.
func NewDeviceHandler(queries *db.Queries) *DeviceHandler {
	return &DeviceHandler{
		queries: queries,
	}
}

// deviceResponse is the JSON body returned for a device info request.
type deviceResponse struct {
	DongleID  string    `json:"dongle_id"`
	Serial    string    `json:"serial"`
	PublicKey string    `json:"public_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetDevice handles GET /v1.1/devices/:dongle_id/ and returns the device info
// as JSON. Returns 404 if the device is not found.
func (h *DeviceHandler) GetDevice(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	device, err := h.queries.GetDevice(c.Request().Context(), dongleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "device not found",
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve device",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.JSON(http.StatusOK, deviceResponse{
		DongleID:  device.DongleID,
		Serial:    device.Serial.String,
		PublicKey: device.PublicKey.String,
		CreatedAt: device.CreatedAt.Time,
		UpdatedAt: device.UpdatedAt.Time,
	})
}

// RegisterRoutes wires up the device routes on the given Echo group.
// The group should already have auth middleware applied.
func (h *DeviceHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/devices/:dongle_id/", h.GetDevice)
	g.GET("/devices/:dongle_id", h.GetDevice)
}
