package api

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/settings"
)

// SettingsHandler exposes the operator-configurable settings table over HTTP.
// Currently only the retention window is wired; add new endpoints here as
// more runtime-adjustable settings land.
type SettingsHandler struct {
	store *settings.Store
	// envRetentionDays is the value loaded from RETENTION_DAYS at startup.
	// Used as a fallback when the settings table has no stored override
	// (for example, in fresh test databases or when a migration is rolled
	// back).
	envRetentionDays int
}

// NewSettingsHandler wires the given settings.Store and env-var fallback
// into a handler ready to register on an Echo group.
func NewSettingsHandler(store *settings.Store, envRetentionDays int) *SettingsHandler {
	return &SettingsHandler{
		store:            store,
		envRetentionDays: envRetentionDays,
	}
}

// retentionResponse is the JSON body returned for retention endpoints.
type retentionResponse struct {
	RetentionDays int `json:"retention_days"`
}

// retentionRequest is the expected JSON body for PUT /v1/settings/retention.
// A pointer is used so we can distinguish "field omitted" from "field set to 0"
// (the latter is valid and means "never delete").
type retentionRequest struct {
	RetentionDays *int `json:"retention_days"`
}

// GetRetention handles GET /v1/settings/retention.
func (h *SettingsHandler) GetRetention(c echo.Context) error {
	days, err := h.store.RetentionDays(c.Request().Context(), h.envRetentionDays)
	if err != nil {
		// RetentionDays already falls back to the env default on
		// ErrNotFound; anything reaching us here is a real failure.
		if errors.Is(err, settings.ErrNotFound) {
			return c.JSON(http.StatusOK, retentionResponse{RetentionDays: h.envRetentionDays})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to read retention setting",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, retentionResponse{RetentionDays: days})
}

// SetRetention handles PUT /v1/settings/retention. The body must contain a
// non-negative integer retention_days field; zero means "never delete".
func (h *SettingsHandler) SetRetention(c echo.Context) error {
	var req retentionRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	if req.RetentionDays == nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "retention_days is required",
			Code:  http.StatusBadRequest,
		})
	}
	if *req.RetentionDays < 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "retention_days must be 0 or a positive integer",
			Code:  http.StatusBadRequest,
		})
	}

	if err := h.store.SetInt(c.Request().Context(), settings.KeyRetentionDays, *req.RetentionDays); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to update retention setting",
			Code:  http.StatusInternalServerError,
		})
	}
	return c.JSON(http.StatusOK, retentionResponse{RetentionDays: *req.RetentionDays})
}

// RegisterRoutes wires up every settings endpoint on the given Echo group.
//
// Deprecated: prefer the split RegisterReadRoutes / RegisterMutationRoutes
// methods so callers can apply SessionOrJWT to reads and SessionRequired
// to mutations (operators-only; devices should never rewrite settings).
func (h *SettingsHandler) RegisterRoutes(g *echo.Group) {
	h.RegisterReadRoutes(g)
	h.RegisterMutationRoutes(g)
}

// RegisterReadRoutes wires the read-only settings endpoints.
func (h *SettingsHandler) RegisterReadRoutes(g *echo.Group) {
	g.GET("/settings/retention", h.GetRetention)
}

// RegisterMutationRoutes wires the settings-mutation endpoints. The
// group is expected to require an operator session cookie (not a device
// JWT).
func (h *SettingsHandler) RegisterMutationRoutes(g *echo.Group) {
	g.PUT("/settings/retention", h.SetRetention)
}
