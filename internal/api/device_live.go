package api

import (
	"net/http"
	"path/filepath"
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
// live endpoint fans out several calls in parallel and the UI polls every 5s.
const liveRPCTimeout = 3 * time.Second

// defaultUploadPriority mirrors openpilot's athenad DEFAULT_UPLOAD_PRIORITY
// (system/athena/athenad.py:57). Items with priority < this are considered
// "immediate" (e.g. user-requested route data); >= are "raw" / background.
const defaultUploadPriority = 99

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
	callNetworkType     func(*ws.Client) (interface{}, error)
	callNetworkMetered  func(*ws.Client) (bool, error)
	callSimInfo         func(*ws.Client) (interface{}, error)
	callDeviceState     func(*ws.Client) (map[string]interface{}, error)
	callListUploadQueue func(*ws.Client) ([]ws.UploadItem, error)
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
	h.callListUploadQueue = func(c *ws.Client) ([]ws.UploadItem, error) {
		return ws.CallListUploadQueue(rpc, c)
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

	// Extended deviceState analytics. All extracted from the same getMessage
	// payload as FreeSpaceGB/ThermalStatus, so they cost zero extra device
	// round trips. Each field is null when absent or the call failed.
	CPUUsagePercent    *int        `json:"cpu_usage_percent"`
	MemoryUsagePercent *int        `json:"memory_usage_percent"`
	MaxTempC           *float64    `json:"max_temp_c"`
	NetworkStrength    interface{} `json:"network_strength"`
	PowerDrawW         *float64    `json:"power_draw_w"`

	// Upload queue analytics, sourced from listUploadQueue. The earlier
	// uploaderState cereal RPC is deprecated upstream and not published by
	// either openpilot or sunnypilot, so we read the canonical queue
	// endpoint instead. Counts are split by athenad's DEFAULT_UPLOAD_PRIORITY
	// (=99): items with priority < 99 are immediate, >= 99 are raw.
	UploadQueueCount    *int     `json:"upload_queue_count"`
	ImmediateQueueCount *int     `json:"immediate_queue_count"`
	RawQueueCount       *int     `json:"raw_queue_count"`
	UploadingNow        *bool    `json:"uploading_now"`
	UploadingPath       *string  `json:"uploading_path"`
	UploadingProgress   *float64 `json:"uploading_progress"`

	FetchedAt time.Time `json:"fetched_at"`
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
//   - Otherwise, fan out getNetworkType, getNetworkMetered, getSimInfo,
//     getMessage("deviceState"), and listUploadQueue in parallel. Each
//     RPC's failure is swallowed: its field becomes null in the response.
//     A partial response is still cached because it is strictly more
//     informative than nothing.
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

// fetchLive issues all athenad RPCs in parallel and merges successful results
// into a liveResponse. A failed RPC leaves its field as the zero value (null
// when serialised).
func (h *DeviceLiveHandler) fetchLive(client *ws.Client) liveResponse {
	resp := liveResponse{
		Online:    true,
		FetchedAt: h.now().UTC(),
	}

	var wg sync.WaitGroup
	wg.Add(5)

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
			ds := extractDeviceState(v)
			resp.FreeSpaceGB = ds.freeSpaceGB
			resp.ThermalStatus = ds.thermalStatus
			resp.CPUUsagePercent = ds.cpuUsagePercent
			resp.MemoryUsagePercent = ds.memoryUsagePercent
			resp.MaxTempC = ds.maxTempC
			resp.NetworkStrength = ds.networkStrength
			resp.PowerDrawW = ds.powerDrawW
		}
	}()
	go func() {
		defer wg.Done()
		if items, err := h.callListUploadQueue(client); err == nil {
			q := extractUploadQueue(items)
			resp.UploadQueueCount = &q.totalCount
			resp.ImmediateQueueCount = &q.immediateCount
			resp.RawQueueCount = &q.rawCount
			resp.UploadingNow = &q.uploadingNow
			if q.uploadingPath != "" {
				path := q.uploadingPath
				resp.UploadingPath = &path
			}
			if q.uploadingProgress != nil {
				resp.UploadingProgress = q.uploadingProgress
			}
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
	resp.CPUUsagePercent = cached.resp.CPUUsagePercent
	resp.MemoryUsagePercent = cached.resp.MemoryUsagePercent
	resp.MaxTempC = cached.resp.MaxTempC
	resp.NetworkStrength = cached.resp.NetworkStrength
	resp.PowerDrawW = cached.resp.PowerDrawW
	resp.UploadQueueCount = cached.resp.UploadQueueCount
	resp.ImmediateQueueCount = cached.resp.ImmediateQueueCount
	resp.RawQueueCount = cached.resp.RawQueueCount
	resp.UploadingNow = cached.resp.UploadingNow
	resp.UploadingPath = cached.resp.UploadingPath
	resp.UploadingProgress = cached.resp.UploadingProgress
	cachedAt := cached.at.UTC()
	resp.CachedAt = &cachedAt
	return resp
}

// extractedDeviceState is the parsed subset of the deviceState cereal message
// that the UI consumes. Each field is a pointer so callers can distinguish
// "missing/unparseable" from "zero".
type extractedDeviceState struct {
	freeSpaceGB        *float64
	thermalStatus      *int
	cpuUsagePercent    *int
	memoryUsagePercent *int
	maxTempC           *float64
	networkStrength    interface{}
	powerDrawW         *float64
}

// extractDeviceState pulls the fields the UI needs from the deviceState
// message returned by getMessage. The value tree is
// {"deviceState": {"freeSpacePercent": f, "thermalStatus": n, ...}}; we accept
// both camelCase and snake_case keys so older agnos builds still work.
func extractDeviceState(msg map[string]interface{}) extractedDeviceState {
	var out extractedDeviceState
	if msg == nil {
		return out
	}
	inner, ok := msg["deviceState"].(map[string]interface{})
	if !ok {
		return out
	}

	for _, k := range []string{"freeSpaceGB", "free_space_gb", "freeSpacePercent", "free_space_percent", "freeSpace"} {
		if v, ok := inner[k]; ok {
			if f, ok := toFloat64(v); ok {
				out.freeSpaceGB = &f
				break
			}
		}
	}

	for _, k := range []string{"thermalStatus", "thermal_status"} {
		if v, ok := inner[k]; ok {
			if n, ok := toInt(v); ok {
				out.thermalStatus = &n
				break
			}
		}
	}

	// cpuUsagePercent is a List(Int8) per cereal: report the peak core so the
	// UI can flag a single hot core without rendering N bars.
	for _, k := range []string{"cpuUsagePercent", "cpu_usage_percent"} {
		if v, ok := inner[k]; ok {
			if peak, ok := peakIntList(v); ok {
				out.cpuUsagePercent = &peak
				break
			}
			// Some firmwares may report a scalar instead of a list; tolerate it.
			if n, ok := toInt(v); ok {
				out.cpuUsagePercent = &n
				break
			}
		}
	}

	for _, k := range []string{"memoryUsagePercent", "memory_usage_percent"} {
		if v, ok := inner[k]; ok {
			if n, ok := toInt(v); ok {
				out.memoryUsagePercent = &n
				break
			}
		}
	}

	for _, k := range []string{"maxTempC", "max_temp_c"} {
		if v, ok := inner[k]; ok {
			if f, ok := toFloat64(v); ok {
				out.maxTempC = &f
				break
			}
		}
	}

	// networkStrength is a NetworkStrength enum (capnp). athenad may serialise
	// it as either a string ("good") or an integer index; pass through as
	// interface{} and let the frontend render it.
	for _, k := range []string{"networkStrength", "network_strength"} {
		if v, ok := inner[k]; ok {
			out.networkStrength = v
			break
		}
	}

	for _, k := range []string{"powerDrawW", "power_draw_w"} {
		if v, ok := inner[k]; ok {
			if f, ok := toFloat64(v); ok {
				out.powerDrawW = &f
				break
			}
		}
	}

	return out
}

// extractedUploadQueue is the aggregate view of athenad's listUploadQueue
// result. uploadingPath is empty when no item is currently uploading;
// uploadingProgress is non-nil only when the active item reports progress > 0.
type extractedUploadQueue struct {
	totalCount        int
	immediateCount    int
	rawCount          int
	uploadingNow      bool
	uploadingPath     string
	uploadingProgress *float64
}

// extractUploadQueue derives the panel's queue stats from athenad's queue
// items. priority < defaultUploadPriority (99) counts as immediate; >= 99
// is raw. uploadingNow is set when any item has current=true; the path and
// progress fields surface the first such item so the UI can show "uploading
// <basename> at NN%" without rendering the full queue.
func extractUploadQueue(items []ws.UploadItem) extractedUploadQueue {
	out := extractedUploadQueue{totalCount: len(items)}
	for i := range items {
		item := items[i]
		if item.Priority < defaultUploadPriority {
			out.immediateCount++
		} else {
			out.rawCount++
		}
		if item.Current && !out.uploadingNow {
			out.uploadingNow = true
			out.uploadingPath = filepath.Base(item.Path)
			if item.Progress > 0 {
				p := item.Progress
				out.uploadingProgress = &p
			}
		}
	}
	return out
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

// peakIntList returns the maximum value in v when v is a JSON-decoded list of
// numbers. Used for cpuUsagePercent where each entry is a per-core sample and
// the UI wants the hottest core. Returns false if v is not a list or is empty.
func peakIntList(v interface{}) (int, bool) {
	list, ok := v.([]interface{})
	if !ok || len(list) == 0 {
		return 0, false
	}
	peak := 0
	any := false
	for _, item := range list {
		if n, ok := toInt(item); ok {
			if !any || n > peak {
				peak = n
				any = true
			}
		}
	}
	if !any {
		return 0, false
	}
	return peak, true
}

// RegisterRoutes wires the live endpoint onto the given Echo group. The group
// is expected to already have session-or-JWT auth middleware applied.
func (h *DeviceLiveHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/devices/:dongle_id/live", h.GetLive)
}
