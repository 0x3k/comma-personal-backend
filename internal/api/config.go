package api

import (
	"log"
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/ws"
)

// ConfigHandler handles device configuration parameter endpoints.
type ConfigHandler struct {
	queries *db.Queries
	hub     *ws.Hub
	rpc     *ws.RPCCaller
}

// NewConfigHandler creates a handler for device config param endpoints.
// The hub and rpc parameters are used to push config changes to connected
// devices. Either may be nil, in which case WebSocket push is skipped.
func NewConfigHandler(queries *db.Queries, hub *ws.Hub, rpc *ws.RPCCaller) *ConfigHandler {
	return &ConfigHandler{
		queries: queries,
		hub:     hub,
		rpc:     rpc,
	}
}

// paramResponse is the JSON representation of a single device parameter.
type paramResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// setParamRequest is the expected JSON body for PUT /v1/devices/:dongle_id/params/:key.
type setParamRequest struct {
	Value string `json:"value"`
}

// ListParams handles GET /v1/devices/:dongle_id/params and returns all
// configuration parameters for the device.
func (h *ConfigHandler) ListParams(c echo.Context) error {
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

	params, err := h.queries.ListDeviceParams(c.Request().Context(), dongleID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list device params",
			Code:  http.StatusInternalServerError,
		})
	}

	result := make([]paramResponse, len(params))
	for i, p := range params {
		result[i] = paramResponse{
			Key:   p.Key,
			Value: p.Value,
		}
	}

	return c.JSON(http.StatusOK, result)
}

// SetParam handles PUT /v1/devices/:dongle_id/params/:key and sets the
// value for the given parameter key. If the device is connected via
// WebSocket, the change is pushed as an RPC call.
func (h *ConfigHandler) SetParam(c echo.Context) error {
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

	key := c.Param("key")
	if key == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "key is required",
			Code:  http.StatusBadRequest,
		})
	}

	var req setParamRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	param, err := h.queries.SetDeviceParam(c.Request().Context(), db.SetDeviceParamParams{
		DongleID: dongleID,
		Key:      key,
		Value:    req.Value,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to set device param",
			Code:  http.StatusInternalServerError,
		})
	}

	h.pushParamChange(dongleID, param.Key, param.Value)

	return c.JSON(http.StatusOK, paramResponse{
		Key:   param.Key,
		Value: param.Value,
	})
}

// DeleteParam handles DELETE /v1/devices/:dongle_id/params/:key and
// removes the parameter. If the device is connected via WebSocket,
// the deletion is pushed as an RPC call.
func (h *ConfigHandler) DeleteParam(c echo.Context) error {
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

	key := c.Param("key")
	if key == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "key is required",
			Code:  http.StatusBadRequest,
		})
	}

	err := h.queries.DeleteDeviceParam(c.Request().Context(), db.DeleteDeviceParamParams{
		DongleID: dongleID,
		Key:      key,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to delete device param",
			Code:  http.StatusInternalServerError,
		})
	}

	h.pushParamDelete(dongleID, key)

	return c.NoContent(http.StatusNoContent)
}

// RegisterRoutes wires up the config param routes on the given Echo group.
// The group should already have auth middleware applied.
func (h *ConfigHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/devices/:dongle_id/params", h.ListParams)
	g.PUT("/devices/:dongle_id/params/:key", h.SetParam)
	g.DELETE("/devices/:dongle_id/params/:key", h.DeleteParam)
}

// pushParamChange sends an RPC notification to the device when a parameter
// is created or updated. It is a best-effort operation; failures are logged
// but do not affect the HTTP response.
func (h *ConfigHandler) pushParamChange(dongleID, key, value string) {
	if h.hub == nil || h.rpc == nil {
		return
	}

	client, ok := h.hub.GetClient(dongleID)
	if !ok {
		return
	}

	params := map[string]string{
		"key":   key,
		"value": value,
	}

	go func() {
		resp, err := h.rpc.Call(client, "setParam", params)
		if err != nil {
			log.Printf("failed to push param change to %s: %v", dongleID, err)
			return
		}
		if resp.Error != nil {
			log.Printf("device %s rejected param change: %v", dongleID, resp.Error)
		}
	}()
}

// pushParamDelete sends an RPC notification to the device when a parameter
// is deleted. It is a best-effort operation; failures are logged but do
// not affect the HTTP response.
func (h *ConfigHandler) pushParamDelete(dongleID, key string) {
	if h.hub == nil || h.rpc == nil {
		return
	}

	client, ok := h.hub.GetClient(dongleID)
	if !ok {
		return
	}

	params := map[string]string{
		"key": key,
	}

	go func() {
		resp, err := h.rpc.Call(client, "deleteParam", params)
		if err != nil {
			log.Printf("failed to push param delete to %s: %v", dongleID, err)
			return
		}
		if resp.Error != nil {
			log.Printf("device %s rejected param delete: %v", dongleID, resp.Error)
		}
	}()
}
