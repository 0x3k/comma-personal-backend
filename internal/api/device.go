package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
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

// deviceListItem is the JSON representation used by the dashboard device
// listing. Field names match the frontend's Device type.
type deviceListItem struct {
	DongleID string     `json:"dongleId"`
	Serial   string     `json:"serial,omitempty"`
	LastSeen *time.Time `json:"lastSeen"`
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

	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
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

// ListDevices handles GET /v1/devices and returns all registered devices for
// the dashboard. It is intentionally unauthenticated because the frontend is
// a local single-user dashboard that has no way to mint a device JWT; it does
// not expose public keys, only the fields the devices page renders.
func (h *DeviceHandler) ListDevices(c echo.Context) error {
	devices, err := h.queries.ListDevices(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list devices",
			Code:  http.StatusInternalServerError,
		})
	}

	items := make([]deviceListItem, 0, len(devices))
	for _, d := range devices {
		item := deviceListItem{DongleID: d.DongleID}
		if d.Serial.Valid {
			item.Serial = d.Serial.String
		}
		if d.UpdatedAt.Valid {
			t := d.UpdatedAt.Time
			item.LastSeen = &t
		}
		items = append(items, item)
	}
	return c.JSON(http.StatusOK, items)
}

// RegisterRoutes wires up the authenticated device routes on the given Echo
// group. The group should already have auth middleware applied.
func (h *DeviceHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/devices/:dongle_id/", h.GetDevice)
	g.GET("/devices/:dongle_id", h.GetDevice)
}
