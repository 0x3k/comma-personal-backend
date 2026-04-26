package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/ws"
)

// SunnylinkParamsLookup is the subset of *db.Queries the sunnylink params
// handler depends on. Kept narrow so tests can stub a single method.
type SunnylinkParamsLookup interface {
	GetDevice(ctx context.Context, dongleID string) (db.Device, error)
}

// SunnylinkParamsHandler exposes the sunnylink-only RPC surface
// (toggleLogUpload, getParams, saveParams) as operator-facing REST
// endpoints. The handler accepts the comma dongle_id (the identifier
// operators see in /devices and /routes) and translates it to the
// sunnylink_dongle_id used as the Hub key for the sunnylink WS connection.
type SunnylinkParamsHandler struct {
	lookup SunnylinkParamsLookup
	hub    *ws.Hub
	rpc    *ws.RPCCaller
}

// NewSunnylinkParamsHandler creates a handler for the sunnylink params
// REST endpoints. The hub and rpc parameters are required: every endpoint
// dispatches an RPC to the device, so a missing transport renders the
// endpoint useless and we surface that as a 503 at request time.
func NewSunnylinkParamsHandler(lookup SunnylinkParamsLookup, hub *ws.Hub, rpc *ws.RPCCaller) *SunnylinkParamsHandler {
	return &SunnylinkParamsHandler{lookup: lookup, hub: hub, rpc: rpc}
}

// getParamsRequest is the shape of GET /v1/sunnylink/devices/:dongle_id/params.
// keys is repeated query param `?keys=...&keys=...` so the URL stays clean
// in the typical "few specific keys" case; an empty list returns an empty
// dict (the device-side method silently drops unknown keys, so passing "all
// keys" via getParamsAllKeys is the right call when the operator wants
// everything).
type getParamsRequest struct {
	Keys        []string `query:"keys"`
	Compression bool     `query:"compression"`
}

// saveParamsRequest is the JSON body for PUT /v1/sunnylink/devices/:dongle_id/params.
type saveParamsRequest struct {
	Updates     map[string]string `json:"updates"`
	Compression bool              `json:"compression"`
}

// saveParamsResponse echoes any blocked keys back to the caller so the
// operator UI can surface "these were rejected" without re-implementing the
// blocklist.
type saveParamsResponse struct {
	Rejected []string `json:"rejected,omitempty"`
}

// toggleLogUploadRequest is the JSON body for POST /v1/sunnylink/devices/:dongle_id/log_upload.
type toggleLogUploadRequest struct {
	Enabled bool `json:"enabled"`
}

// resolveSunnylinkClient looks up the device row by comma dongle_id, ensures
// it has a sunnylink registration, and returns the hub Client for that
// sunnylink connection. The various 4xx/5xx responses are written to c
// directly; a non-nil error indicates the caller should return it without
// further processing.
func (h *SunnylinkParamsHandler) resolveSunnylinkClient(c echo.Context, commaDongleID string) (*ws.Client, error, bool) {
	if h.hub == nil || h.rpc == nil {
		return nil, c.JSON(http.StatusServiceUnavailable, errorResponse{
			Error: "sunnylink RPC not configured",
			Code:  http.StatusServiceUnavailable,
		}), true
	}
	if commaDongleID == "" {
		return nil, c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		}), true
	}
	if handled, err := checkDongleAccess(c, commaDongleID); handled {
		return nil, err, true
	}

	device, err := h.lookup.GetDevice(c.Request().Context(), commaDongleID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, c.JSON(http.StatusNotFound, errorResponse{
				Error: "device not found",
				Code:  http.StatusNotFound,
			}), true
		}
		return nil, c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up device",
			Code:  http.StatusInternalServerError,
		}), true
	}
	if !device.SunnylinkDongleID.Valid || device.SunnylinkDongleID.String == "" {
		return nil, c.JSON(http.StatusConflict, errorResponse{
			Error: "device has not completed sunnylink registration",
			Code:  http.StatusConflict,
		}), true
	}

	client, ok := h.hub.GetClient(device.SunnylinkDongleID.String)
	if !ok {
		return nil, c.JSON(http.StatusServiceUnavailable, errorResponse{
			Error: "sunnylink connection not active for device",
			Code:  http.StatusServiceUnavailable,
		}), true
	}
	return client, nil, false
}

// GetParams handles GET /v1/sunnylink/devices/:dongle_id/params and proxies
// to the sunnylink getParams RPC. The response shape is the device's raw
// dict (key -> base64 value, plus a "params" entry with detailed metadata)
// so callers can pick whichever form they prefer.
func (h *SunnylinkParamsHandler) GetParams(c echo.Context) error {
	commaDongleID := c.Param("dongle_id")
	client, err, handled := h.resolveSunnylinkClient(c, commaDongleID)
	if handled {
		return err
	}

	var req getParamsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse query params",
			Code:  http.StatusBadRequest,
		})
	}

	result, callErr := ws.CallGetParams(h.rpc, client, req.Keys, req.Compression)
	if callErr != nil {
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "device rpc failed: " + callErr.Error(),
			Code:  http.StatusBadGateway,
		})
	}
	return c.JSON(http.StatusOK, result)
}

// SaveParams handles PUT /v1/sunnylink/devices/:dongle_id/params and proxies
// to the sunnylink saveParams RPC. Blocked keys are stripped server-side
// before being forwarded to the device, and reported back in the
// `rejected` field so the operator UI can surface the failure.
func (h *SunnylinkParamsHandler) SaveParams(c echo.Context) error {
	commaDongleID := c.Param("dongle_id")
	client, err, handled := h.resolveSunnylinkClient(c, commaDongleID)
	if handled {
		return err
	}

	var req saveParamsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	rejected, callErr := ws.CallSaveParams(h.rpc, client, req.Updates, req.Compression)
	if callErr != nil {
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "device rpc failed: " + callErr.Error(),
			Code:  http.StatusBadGateway,
		})
	}
	return c.JSON(http.StatusOK, saveParamsResponse{Rejected: rejected})
}

// ToggleLogUpload handles POST /v1/sunnylink/devices/:dongle_id/log_upload
// and proxies to the sunnylink toggleLogUpload RPC. Body is `{enabled: bool}`.
func (h *SunnylinkParamsHandler) ToggleLogUpload(c echo.Context) error {
	commaDongleID := c.Param("dongle_id")
	client, err, handled := h.resolveSunnylinkClient(c, commaDongleID)
	if handled {
		return err
	}

	var req toggleLogUploadRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	if callErr := ws.CallToggleLogUpload(h.rpc, client, req.Enabled); callErr != nil {
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "device rpc failed: " + callErr.Error(),
			Code:  http.StatusBadGateway,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// RegisterReadRoutes wires the GET sunnylink params endpoint. The group
// should accept either a session cookie or a device JWT.
func (h *SunnylinkParamsHandler) RegisterReadRoutes(g *echo.Group) {
	g.GET("/sunnylink/devices/:dongle_id/params", h.GetParams)
}

// RegisterMutationRoutes wires the writes (saveParams, toggleLogUpload).
// These should be session-only -- a compromised device should not be able to
// flip its own log-upload toggle from the server side.
func (h *SunnylinkParamsHandler) RegisterMutationRoutes(g *echo.Group) {
	g.PUT("/sunnylink/devices/:dongle_id/params", h.SaveParams)
	g.POST("/sunnylink/devices/:dongle_id/log_upload", h.ToggleLogUpload)
}
