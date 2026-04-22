package api

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/ws"
)

// snapshotRateLimit is the minimum interval between successive snapshot
// requests for the same device. The camera pipeline on the car is not free to
// trigger, so this guard prevents a stuck UI or over-eager operator from
// thrashing it.
const snapshotRateLimit = 5 * time.Second

// SnapshotClientGetter is the subset of *ws.Hub the snapshot handler depends
// on. Narrowing the interface keeps the tests independent of the full hub.
type SnapshotClientGetter interface {
	GetClient(dongleID string) (*ws.Client, bool)
}

// SnapshotCaller is the subset of *ws.RPCCaller used by SnapshotHandler. It is
// satisfied by *ws.RPCCaller and can be faked in tests without spinning up a
// real RPC session.
type SnapshotCaller interface {
	TakeSnapshot(client *ws.Client) (*ws.SnapshotResult, error)
}

// rpcSnapshotCaller adapts a *ws.RPCCaller to the SnapshotCaller interface by
// delegating to ws.CallTakeSnapshot.
type rpcSnapshotCaller struct {
	caller *ws.RPCCaller
}

// TakeSnapshot issues a takeSnapshot RPC on the given connected client.
func (r *rpcSnapshotCaller) TakeSnapshot(client *ws.Client) (*ws.SnapshotResult, error) {
	return ws.CallTakeSnapshot(r.caller, client)
}

// SnapshotHandler serves POST /v1/devices/:dongle_id/snapshot. It fronts the
// takeSnapshot RPC with a per-device rate limiter so the car's cameras are
// not hammered by a buggy frontend.
type SnapshotHandler struct {
	hub    SnapshotClientGetter
	caller SnapshotCaller

	mu      sync.Mutex
	lastHit map[string]time.Time
	now     func() time.Time
}

// NewSnapshotHandler wires the handler to a WebSocket hub and RPC caller.
// The hub lets the handler look up a live device connection; the caller
// issues the actual takeSnapshot RPC and parses the response.
func NewSnapshotHandler(hub *ws.Hub, caller *ws.RPCCaller) *SnapshotHandler {
	return newSnapshotHandler(hub, &rpcSnapshotCaller{caller: caller})
}

// newSnapshotHandler is the internal constructor used by both production code
// and tests. Tests pass custom SnapshotCaller/SnapshotClientGetter fakes so
// they can drive deterministic RPC responses.
func newSnapshotHandler(hub SnapshotClientGetter, caller SnapshotCaller) *SnapshotHandler {
	return &SnapshotHandler{
		hub:     hub,
		caller:  caller,
		lastHit: make(map[string]time.Time),
		now:     time.Now,
	}
}

// snapshotResponse is the JSON body returned by POST .../snapshot. The two
// fields are always base64 data-URL strings when populated so the frontend
// can drop them into an <img src=...> without further processing. A field
// may be empty when the device only returned one image.
type snapshotResponse struct {
	JpegBack  string `json:"jpeg_back"`
	JpegFront string `json:"jpeg_front"`
}

// TakeSnapshot handles POST /v1/devices/:dongle_id/snapshot. It enforces the
// per-device rate limit, looks up the connected WebSocket client, issues the
// takeSnapshot RPC, and returns the two JPEGs as base64 data URLs. Offline
// devices are surfaced as 503 so the UI can distinguish them from a 500.
func (h *SnapshotHandler) TakeSnapshot(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	// For device-auth requests, enforce that the JWT belongs to the target
	// device. Session-auth requests do not carry a dongle_id, so they bypass
	// this check: the operator can take a snapshot of any registered device.
	if authDongleID, ok := c.Get(middleware.ContextKeyDongleID).(string); ok && authDongleID != "" {
		if authDongleID != dongleID {
			return c.JSON(http.StatusForbidden, errorResponse{
				Error: "dongle_id does not match authenticated device",
				Code:  http.StatusForbidden,
			})
		}
	}

	if !h.allowHit(dongleID) {
		return c.JSON(http.StatusTooManyRequests, errorResponse{
			Error: "snapshot rate limit exceeded; try again in a few seconds",
			Code:  http.StatusTooManyRequests,
		})
	}

	if h.hub == nil {
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

	result, err := h.caller.TakeSnapshot(client)
	if err != nil {
		log.Printf("snapshot: takeSnapshot failed for %s: %v", dongleID, err)
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "failed to take snapshot",
			Code:  http.StatusBadGateway,
		})
	}
	if result == nil {
		return c.JSON(http.StatusBadGateway, errorResponse{
			Error: "device returned an empty snapshot",
			Code:  http.StatusBadGateway,
		})
	}

	back, front := extractJpegs(result)
	return c.JSON(http.StatusOK, snapshotResponse{
		JpegBack:  back,
		JpegFront: front,
	})
}

// allowHit returns true when the given device has not hit the endpoint
// within the last snapshotRateLimit window. A successful check also updates
// the last-hit timestamp so the next call sees the refreshed window.
func (h *SnapshotHandler) allowHit(dongleID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if last, ok := h.lastHit[dongleID]; ok && now.Sub(last) < snapshotRateLimit {
		return false
	}
	h.lastHit[dongleID] = now
	return true
}

// extractJpegs converts a SnapshotResult into the two base64 data URLs the
// API exposes. The device may return either an object containing jpegBack /
// jpegFront, or a single base64 string for backwards compatibility; in the
// single-string case the payload is treated as the rear camera image.
func extractJpegs(r *ws.SnapshotResult) (back, front string) {
	if r == nil {
		return "", ""
	}
	if r.JpegBack != "" {
		back = toJpegDataURL(r.JpegBack)
	}
	if r.JpegFront != "" {
		front = toJpegDataURL(r.JpegFront)
	}
	if back == "" && front == "" && r.RawString != "" {
		back = toJpegDataURL(r.RawString)
	}
	return back, front
}

// toJpegDataURL wraps a base64 JPEG payload in the RFC 2397 data URL prefix
// expected by the browser. If s already starts with "data:" it is returned
// unchanged so a device that begins to emit full data URLs does not get
// double-wrapped.
func toJpegDataURL(s string) string {
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "data:") {
		return s
	}
	return "data:image/jpeg;base64," + s
}

// RegisterRoutes wires the snapshot route onto the given Echo group. The
// group is expected to already have auth middleware applied.
func (h *SnapshotHandler) RegisterRoutes(g *echo.Group) {
	g.POST("/devices/:dongle_id/snapshot", h.TakeSnapshot)
	g.POST("/devices/:dongle_id/snapshot/", h.TakeSnapshot)
}
