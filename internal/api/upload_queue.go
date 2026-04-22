package api

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/ws"
)

// UploadQueueHandler serves /v1/devices/:dongle_id/upload-queue endpoints.
// Both endpoints proxy to the device over its live WebSocket using the
// listUploadQueue and cancelUpload RPCs.
//
// Ownership:
//   - GET /v1/devices/:dongle_id/upload-queue returns the current queue.
//   - POST /v1/devices/:dongle_id/upload-queue/cancel cancels one or more
//     upload IDs supplied in the JSON body.
//
// The handler returns 503 when the device is not connected because neither
// endpoint is useful without a live device -- we do not cache the queue.
type UploadQueueHandler struct {
	hub *ws.Hub
	rpc *ws.RPCCaller
}

// NewUploadQueueHandler constructs a handler from the shared WebSocket hub
// and RPC caller.
func NewUploadQueueHandler(hub *ws.Hub, rpc *ws.RPCCaller) *UploadQueueHandler {
	return &UploadQueueHandler{hub: hub, rpc: rpc}
}

// cancelUploadRequest is the JSON body accepted by POST .../cancel.
type cancelUploadRequest struct {
	IDs []string `json:"ids"`
}

// cancelUploadResponse is the JSON body returned by POST .../cancel. The
// result field carries whatever shape the device returned (athenad replies
// with map[string]int counts keyed by a per-ID status).
type cancelUploadResponse struct {
	Result map[string]interface{} `json:"result"`
}

// ListQueue handles GET /v1/devices/:dongle_id/upload-queue. It proxies the
// request through the WebSocket RPC caller to the connected device and
// returns the decoded queue verbatim. Returns 503 if no device is connected.
func (h *UploadQueueHandler) ListQueue(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	if h.hub == nil || h.rpc == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse{
			Error: "device is offline",
			Code:  http.StatusServiceUnavailable,
		})
	}

	client, ok := h.hub.GetClient(dongleID)
	if !ok || client == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse{
			Error: "device is offline",
			Code:  http.StatusServiceUnavailable,
		})
	}

	items, err := ws.CallListUploadQueue(h.rpc, client)
	if err != nil {
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "failed to list upload queue",
			Code:  http.StatusBadGateway,
		})
	}

	// Never return a nil slice: the frontend iterates over the response and
	// serializing nil as "null" trips a surprising number of callers.
	if items == nil {
		items = []ws.UploadItem{}
	}
	return c.JSON(http.StatusOK, items)
}

// CancelUpload handles POST /v1/devices/:dongle_id/upload-queue/cancel. The
// body must be {"ids": ["...", ...]}. Empty or missing ids is rejected with
// 400 so the operator does not accidentally send a no-op request.
func (h *UploadQueueHandler) CancelUpload(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	var req cancelUploadRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	if len(req.IDs) == 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "ids is required",
			Code:  http.StatusBadRequest,
		})
	}

	if h.hub == nil || h.rpc == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse{
			Error: "device is offline",
			Code:  http.StatusServiceUnavailable,
		})
	}

	client, ok := h.hub.GetClient(dongleID)
	if !ok || client == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse{
			Error: "device is offline",
			Code:  http.StatusServiceUnavailable,
		})
	}

	result, err := ws.CallCancelUpload(h.rpc, client, req.IDs)
	if err != nil {
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "failed to cancel upload",
			Code:  http.StatusBadGateway,
		})
	}

	return c.JSON(http.StatusOK, cancelUploadResponse{Result: result})
}

// RegisterListRoute wires the GET endpoint onto a group. The caller chooses
// the group (and its auth middleware) so operators can pair session-or-JWT
// auth with the read path per the feature spec.
func (h *UploadQueueHandler) RegisterListRoute(g *echo.Group) {
	g.GET("/devices/:dongle_id/upload-queue", h.ListQueue)
}

// RegisterCancelRoute wires the POST endpoint onto a group. The caller
// chooses the group so the session-only auth middleware can be applied to
// the mutating action independently from the read group.
func (h *UploadQueueHandler) RegisterCancelRoute(g *echo.Group) {
	g.POST("/devices/:dongle_id/upload-queue/cancel", h.CancelUpload)
}
