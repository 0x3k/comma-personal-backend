package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/ws"
)

// liveCacheTTL is how long the last successful live-status response is held
// in the per-handler cache, so a disconnect is still informative for up to
// five minutes.
const liveCacheTTL = 5 * time.Minute

// rpcCallTimeout bounds each individual athenad RPC. Kept short because the
// live endpoint fans out four calls in parallel and the UI polls every 5s.
const liveRPCTimeout = 3 * time.Second

// DeviceLiveHandler orchestrates the parallel athenad RPC calls that feed the
// web UI's live status card. It owns a small in-memory cache so an offline
// device can still surface its last-known values.
type DeviceLiveHandler struct {
	hub *ws.Hub
	rpc *ws.RPCCaller

	cache sync.Map // map[string]cachedLive
	now   func() time.Time

	// Injected seams for testing so the handler can exercise success and
	// partial-failure paths without standing up a real WebSocket client.
	callNetworkType    func(*ws.Client) (interface{}, error)
	callNetworkMetered func(*ws.Client) (bool, error)
	callSimInfo        func(*ws.Client) (interface{}, error)
	callDeviceState    func(*ws.Client) (map[string]interface{}, error)
}

// NewDeviceLiveHandler constructs a live-status handler. hub and rpc may be
// nil; in that case every call short-circuits to the offline branch.
func NewDeviceLiveHandler(hub *ws.Hub, rpc *ws.RPCCaller) *DeviceLiveHandler {
	h := &DeviceLiveHandler{
		hub: hub,
		rpc: rpc,
		now: time.Now,
	}
	h.callNetworkType = func(c *ws.Client) (interface{}, error) {
		return ws.CallGetNetworkType(rpc, c)
	}
	h.callNetworkMetered = func(c *ws.Client) (bool, error) {
		return ws.CallGetNetworkMetered(rpc, c)
	}
	h.callSimInfo = func(c *ws.Client) (interface{}, error) {
		return ws.CallGetSimInfo(rpc, c)
	}
	h.callDeviceState = func(c *ws.Client) (map[string]interface{}, error) {
		return ws.CallGetMessage(rpc, c, "deviceState", int(liveRPCTimeout/time.Millisecond))
	}
	return h
}

// liveResponse is the JSON body returned by GET /v1/devices/:dongle_id/live.
// Every field other than Online is nullable: if a particular RPC fails the
// value is omitted (null) rather than failing the whole response.
type liveResponse struct {
	Online        bool        `json:"online"`
	NetworkType   interface{} `json:"network_type"`
	Metered       *bool       `json:"metered"`
	Sim           interface{} `json:"sim"`
	FreeSpaceGB   *float64    `json:"free_space_gb"`
	ThermalStatus *int        `json:"thermal_status"`
	FetchedAt     time.Time   `json:"fetched_at"`
	// CachedAt is non-zero when Online is false and the payload was served
	// from the last-known-value cache. The UI renders it as "last seen".
	CachedAt *time.Time `json:"cached_at,omitempty"`
}

// cachedLive is the entry stored in the per-dongle sync.Map.
type cachedLive struct {
	resp liveResponse
	at   time.Time
}

// GetLive handles GET /v1/devices/:dongle_id/live.
//
// Behaviour:
//   - If the device has no active WebSocket client, return {online:false}
//     merged with whatever was cached within liveCacheTTL. Cached values that
//     are older than the TTL are dropped.
//   - Otherwise, fan out getNetworkType, getNetworkMetered, getSimInfo, and
//     getMessage("deviceState") in parallel. Each RPC's failure is swallowed:
//     its field becomes null in the response. A partial response is still
//     cached because it is strictly more informative than nothing.
func (h *DeviceLiveHandler) GetLive(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	if h.hub == nil {
		return c.JSON(http.StatusOK, h.offlineResponse(dongleID))
	}

	client, ok := h.hub.GetClient(dongleID)
	if !ok {
		return c.JSON(http.StatusOK, h.offlineResponse(dongleID))
	}

	resp := h.fetchLive(client)
	h.cache.Store(dongleID, cachedLive{resp: resp, at: h.now()})
	return c.JSON(http.StatusOK, resp)
}

// fetchLive issues all four athenad RPCs in parallel and merges successful
// results into a liveResponse. A failed RPC leaves its field as the zero
// value (null when serialised).
func (h *DeviceLiveHandler) fetchLive(client *ws.Client) liveResponse {
	resp := liveResponse{
		Online:    true,
		FetchedAt: h.now().UTC(),
	}

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		if v, err := h.callNetworkType(client); err == nil {
			resp.NetworkType = v
		}
	}()
	go func() {
		defer wg.Done()
		if v, err := h.callNetworkMetered(client); err == nil {
			metered := v
			resp.Metered = &metered
		}
	}()
	go func() {
		defer wg.Done()
		if v, err := h.callSimInfo(client); err == nil {
			resp.Sim = v
		}
	}()
	go func() {
		defer wg.Done()
		if v, err := h.callDeviceState(client); err == nil {
			free, thermal := extractDeviceState(v)
			resp.FreeSpaceGB = free
			resp.ThermalStatus = thermal
		}
	}()

	wg.Wait()
	return resp
}

// offlineResponse returns the response payload used when the device has no
// active WebSocket client. Any fresh cache entry is merged in so the UI can
// still show the most recent known values alongside the offline badge.
func (h *DeviceLiveHandler) offlineResponse(dongleID string) liveResponse {
	resp := liveResponse{
		Online:    false,
		FetchedAt: h.now().UTC(),
	}

	raw, ok := h.cache.Load(dongleID)
	if !ok {
		return resp
	}
	cached, ok := raw.(cachedLive)
	if !ok {
		return resp
	}
	if h.now().Sub(cached.at) > liveCacheTTL {
		h.cache.Delete(dongleID)
		return resp
	}

	resp.NetworkType = cached.resp.NetworkType
	resp.Metered = cached.resp.Metered
	resp.Sim = cached.resp.Sim
	resp.FreeSpaceGB = cached.resp.FreeSpaceGB
	resp.ThermalStatus = cached.resp.ThermalStatus
	cachedAt := cached.at.UTC()
	resp.CachedAt = &cachedAt
	return resp
}

// extractDeviceState pulls the two fields the UI needs from the deviceState
// message returned by getMessage. The value tree is
// {"deviceState": {"freeSpacePercent": f, "thermalStatus": n, ...}}; we
// accept both camelCase and snake_case keys so older agnos builds still work.
func extractDeviceState(msg map[string]interface{}) (*float64, *int) {
	if msg == nil {
		return nil, nil
	}
	inner, ok := msg["deviceState"].(map[string]interface{})
	if !ok {
		return nil, nil
	}

	var free *float64
	for _, k := range []string{"freeSpaceGB", "free_space_gb", "freeSpacePercent", "free_space_percent", "freeSpace"} {
		if v, ok := inner[k]; ok {
			if f, ok := toFloat64(v); ok {
				free = &f
				break
			}
		}
	}

	var thermal *int
	for _, k := range []string{"thermalStatus", "thermal_status"} {
		if v, ok := inner[k]; ok {
			if n, ok := toInt(v); ok {
				thermal = &n
				break
			}
		}
	}

	return free, thermal
}

// toFloat64 widens any JSON-decoded numeric value to float64. JSON numbers
// arrive as float64 by default, but when the RPC layer rehydrates via
// json.Number they come back as a string.
func toFloat64(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// toInt narrows any JSON-decoded numeric value to int. Capnp enums arrive as
// numeric codes, so a clean int conversion is sufficient for thermal_status.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	}
	return 0, false
}

// RegisterRoutes wires the live endpoint onto the given Echo group. The group
// is expected to already have session-or-JWT auth middleware applied.
func (h *DeviceLiveHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/devices/:dongle_id/live", h.GetLive)
}
