package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/ws"
)

// newTestLiveHandler constructs a DeviceLiveHandler with a real hub/rpc but
// all four athenad calls stubbed by the test. Returning the mutable handler
// lets each test override individual callers to exercise success,
// partial-failure, and error paths.
func newTestLiveHandler() (*DeviceLiveHandler, *ws.Hub) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	h := NewDeviceLiveHandler(hub, rpc)
	h.now = func() time.Time {
		return time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	}
	return h, hub
}

func TestGetLiveOfflineNoCache(t *testing.T) {
	h, _ := newTestLiveHandler()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Online {
		t.Errorf("online = true, want false")
	}
	if resp.NetworkType != nil {
		t.Errorf("network_type = %v, want nil", resp.NetworkType)
	}
	if resp.Metered != nil {
		t.Errorf("metered = %v, want nil", *resp.Metered)
	}
	if resp.Sim != nil {
		t.Errorf("sim = %v, want nil", resp.Sim)
	}
	if resp.FreeSpaceGB != nil {
		t.Errorf("free_space_gb = %v, want nil", *resp.FreeSpaceGB)
	}
	if resp.ThermalStatus != nil {
		t.Errorf("thermal_status = %v, want nil", *resp.ThermalStatus)
	}
	if resp.CachedAt != nil {
		t.Errorf("cached_at = %v, want nil", resp.CachedAt)
	}
}

func TestGetLiveOfflineServesCache(t *testing.T) {
	h, _ := newTestLiveHandler()
	metered := true
	thermal := 2
	free := 12.5

	cachedAt := h.now().Add(-1 * time.Minute)
	h.cache.Store("abc123", cachedLive{
		resp: liveResponse{
			Online:        true,
			NetworkType:   "LTE",
			Metered:       &metered,
			Sim:           map[string]interface{}{"state": "READY"},
			FreeSpaceGB:   &free,
			ThermalStatus: &thermal,
			FetchedAt:     cachedAt,
		},
		at: cachedAt,
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Online {
		t.Errorf("online = true, want false (device disconnected)")
	}
	if resp.NetworkType != "LTE" {
		t.Errorf("network_type = %v, want LTE", resp.NetworkType)
	}
	if resp.Metered == nil || *resp.Metered != true {
		t.Errorf("metered = %v, want true", resp.Metered)
	}
	if resp.FreeSpaceGB == nil || *resp.FreeSpaceGB != 12.5 {
		t.Errorf("free_space_gb = %v, want 12.5", resp.FreeSpaceGB)
	}
	if resp.ThermalStatus == nil || *resp.ThermalStatus != 2 {
		t.Errorf("thermal_status = %v, want 2", resp.ThermalStatus)
	}
	if resp.CachedAt == nil {
		t.Error("expected cached_at to be populated when serving from cache")
	}
}

func TestGetLiveCacheExpired(t *testing.T) {
	h, _ := newTestLiveHandler()
	metered := true

	// Stored 10 minutes ago; TTL is 5 minutes, so it should be evicted.
	oldAt := h.now().Add(-10 * time.Minute)
	h.cache.Store("abc123", cachedLive{
		resp: liveResponse{Metered: &metered},
		at:   oldAt,
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Online {
		t.Errorf("online = true, want false")
	}
	if resp.Metered != nil {
		t.Errorf("metered = %v, want nil after cache expiry", *resp.Metered)
	}
	if resp.CachedAt != nil {
		t.Errorf("cached_at = %v, want nil after cache expiry", resp.CachedAt)
	}

	if _, ok := h.cache.Load("abc123"); ok {
		t.Error("expected expired cache entry to be evicted")
	}
}

func TestGetLiveOnlineSuccess(t *testing.T) {
	h, hub := newTestLiveHandler()

	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	h.callNetworkType = func(_ *ws.Client) (interface{}, error) {
		return map[string]interface{}{"network_type": 5}, nil
	}
	h.callNetworkMetered = func(_ *ws.Client) (bool, error) { return false, nil }
	h.callSimInfo = func(_ *ws.Client) (interface{}, error) {
		return map[string]interface{}{"sim_id": "0001", "state": "READY"}, nil
	}
	h.callDeviceState = func(_ *ws.Client) (map[string]interface{}, error) {
		return map[string]interface{}{
			"deviceState": map[string]interface{}{
				"freeSpaceGB":        42.0,
				"thermalStatus":      1.0,
				"cpuUsagePercent":    []interface{}{10.0, 30.0, 5.0},
				"memoryUsagePercent": 51.0,
				"maxTempC":           58.0,
				"networkStrength":    2.0,
				"powerDrawW":         6.7,
			},
		}, nil
	}
	h.callListUploadQueue = func(_ *ws.Client) ([]ws.UploadItem, error) {
		return []ws.UploadItem{
			{ID: "a", Path: "/data/media/0/realdata/00000050--xx--12/qlog.zst", Priority: 1, Current: true, Progress: 0.42},
			{ID: "b", Path: "/data/media/0/realdata/00000050--xx--12/fcamera.hevc", Priority: 99},
			{ID: "c", Path: "/data/media/0/realdata/00000049--xx--3/dcamera.hevc", Priority: 99},
		}, nil
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if !resp.Online {
		t.Error("online = false, want true")
	}
	if resp.Metered == nil || *resp.Metered != false {
		t.Errorf("metered = %v, want false", resp.Metered)
	}
	if resp.FreeSpaceGB == nil || *resp.FreeSpaceGB != 42.0 {
		t.Errorf("free_space_gb = %v, want 42.0", resp.FreeSpaceGB)
	}
	if resp.ThermalStatus == nil || *resp.ThermalStatus != 1 {
		t.Errorf("thermal_status = %v, want 1", resp.ThermalStatus)
	}
	if resp.Sim == nil {
		t.Error("sim = nil, want populated")
	}
	if resp.NetworkType == nil {
		t.Error("network_type = nil, want populated")
	}
	if resp.CPUUsagePercent == nil || *resp.CPUUsagePercent != 30 {
		t.Errorf("cpu_usage_percent = %v, want 30 (peak)", pi(resp.CPUUsagePercent))
	}
	if resp.MemoryUsagePercent == nil || *resp.MemoryUsagePercent != 51 {
		t.Errorf("memory_usage_percent = %v, want 51", pi(resp.MemoryUsagePercent))
	}
	if resp.MaxTempC == nil || *resp.MaxTempC != 58.0 {
		t.Errorf("max_temp_c = %v, want 58.0", pf(resp.MaxTempC))
	}
	if resp.PowerDrawW == nil || *resp.PowerDrawW != 6.7 {
		t.Errorf("power_draw_w = %v, want 6.7", pf(resp.PowerDrawW))
	}
	if resp.UploadQueueCount == nil || *resp.UploadQueueCount != 3 {
		t.Errorf("upload_queue_count = %v, want 3", pi(resp.UploadQueueCount))
	}
	if resp.ImmediateQueueCount == nil || *resp.ImmediateQueueCount != 1 {
		t.Errorf("immediate_queue_count = %v, want 1", pi(resp.ImmediateQueueCount))
	}
	if resp.RawQueueCount == nil || *resp.RawQueueCount != 2 {
		t.Errorf("raw_queue_count = %v, want 2", pi(resp.RawQueueCount))
	}
	if resp.UploadingNow == nil || *resp.UploadingNow != true {
		t.Errorf("uploading_now = %v, want true", resp.UploadingNow)
	}
	if resp.UploadingPath == nil || *resp.UploadingPath != "qlog.zst" {
		t.Errorf("uploading_path = %v, want qlog.zst", resp.UploadingPath)
	}
	if resp.UploadingProgress == nil || *resp.UploadingProgress != 0.42 {
		t.Errorf("uploading_progress = %v, want 0.42", pf(resp.UploadingProgress))
	}

	// Cache must be populated for the offline-fallback path.
	if _, ok := h.cache.Load("abc123"); !ok {
		t.Error("expected cache entry after successful live fetch")
	}
}

func TestGetLivePartialFailure(t *testing.T) {
	h, hub := newTestLiveHandler()

	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	// getNetworkType succeeds; every other RPC fails.
	h.callNetworkType = func(_ *ws.Client) (interface{}, error) {
		return map[string]interface{}{"network_type": 5}, nil
	}
	h.callNetworkMetered = func(_ *ws.Client) (bool, error) {
		return false, errors.New("rpc timed out")
	}
	h.callSimInfo = func(_ *ws.Client) (interface{}, error) {
		return nil, errors.New("rpc error")
	}
	h.callDeviceState = func(_ *ws.Client) (map[string]interface{}, error) {
		return nil, errors.New("service unavailable")
	}
	h.callListUploadQueue = func(_ *ws.Client) ([]ws.UploadItem, error) {
		return nil, errors.New("service unavailable")
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if !resp.Online {
		t.Error("online should still be true when the socket is connected, regardless of per-RPC failures")
	}
	if resp.NetworkType == nil {
		t.Error("expected network_type to survive partial failure")
	}
	if resp.Metered != nil {
		t.Errorf("metered = %v, want nil (rpc failed)", *resp.Metered)
	}
	if resp.Sim != nil {
		t.Errorf("sim = %v, want nil (rpc failed)", resp.Sim)
	}
	if resp.FreeSpaceGB != nil {
		t.Errorf("free_space_gb = %v, want nil (rpc failed)", *resp.FreeSpaceGB)
	}
	if resp.ThermalStatus != nil {
		t.Errorf("thermal_status = %v, want nil (rpc failed)", *resp.ThermalStatus)
	}
	if resp.UploadQueueCount != nil {
		t.Errorf("upload_queue_count = %v, want nil (rpc failed)", *resp.UploadQueueCount)
	}
	if resp.UploadingNow != nil {
		t.Errorf("uploading_now = %v, want nil (rpc failed)", *resp.UploadingNow)
	}
}

// TestGetLiveUploadQueueRPCFailureLeavesDeviceFields asserts that when only
// the listUploadQueue RPC fails, the deviceState-derived fields (CPU, memory,
// free disk, etc.) still populate. This is the inverse of the partial-failure
// test and is required by the feature's acceptance criteria: failures of
// either RPC must not knock the other out.
func TestGetLiveUploadQueueRPCFailureLeavesDeviceFields(t *testing.T) {
	h, hub := newTestLiveHandler()

	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	h.callNetworkType = func(_ *ws.Client) (interface{}, error) { return "LTE", nil }
	h.callNetworkMetered = func(_ *ws.Client) (bool, error) { return false, nil }
	h.callSimInfo = func(_ *ws.Client) (interface{}, error) { return map[string]interface{}{}, nil }
	h.callDeviceState = func(_ *ws.Client) (map[string]interface{}, error) {
		return map[string]interface{}{
			"deviceState": map[string]interface{}{
				"freeSpaceGB":        20.0,
				"cpuUsagePercent":    []interface{}{15.0, 5.0},
				"memoryUsagePercent": 30.0,
			},
		}, nil
	}
	h.callListUploadQueue = func(_ *ws.Client) ([]ws.UploadItem, error) {
		return nil, errors.New("upload queue rpc failed")
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.FreeSpaceGB == nil || *resp.FreeSpaceGB != 20.0 {
		t.Errorf("free_space_gb = %v, want 20.0 (deviceState should survive queue failure)", pf(resp.FreeSpaceGB))
	}
	if resp.CPUUsagePercent == nil || *resp.CPUUsagePercent != 15 {
		t.Errorf("cpu_usage_percent = %v, want 15", pi(resp.CPUUsagePercent))
	}
	if resp.MemoryUsagePercent == nil {
		t.Error("memory_usage_percent should survive queue failure")
	}
	if resp.UploadQueueCount != nil {
		t.Errorf("upload_queue_count = %v, want nil (rpc failed)", *resp.UploadQueueCount)
	}
	if resp.ImmediateQueueCount != nil {
		t.Errorf("immediate_queue_count = %v, want nil (rpc failed)", *resp.ImmediateQueueCount)
	}
	if resp.UploadingNow != nil {
		t.Errorf("uploading_now = %v, want nil (rpc failed)", *resp.UploadingNow)
	}
}

// TestGetLiveDeviceStateFailureLeavesQueueFields is the symmetric check:
// when deviceState RPC fails, listUploadQueue-derived fields still populate.
func TestGetLiveDeviceStateFailureLeavesQueueFields(t *testing.T) {
	h, hub := newTestLiveHandler()

	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	h.callNetworkType = func(_ *ws.Client) (interface{}, error) { return "LTE", nil }
	h.callNetworkMetered = func(_ *ws.Client) (bool, error) { return false, nil }
	h.callSimInfo = func(_ *ws.Client) (interface{}, error) { return map[string]interface{}{}, nil }
	h.callDeviceState = func(_ *ws.Client) (map[string]interface{}, error) {
		return nil, errors.New("device state unavailable")
	}
	h.callListUploadQueue = func(_ *ws.Client) ([]ws.UploadItem, error) {
		return []ws.UploadItem{
			{ID: "1", Path: "/data/foo/qlog.zst", Priority: 1, Current: false},
			{ID: "2", Path: "/data/foo/fcamera.hevc", Priority: 99},
		}, nil
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.UploadQueueCount == nil || *resp.UploadQueueCount != 2 {
		t.Errorf("upload_queue_count = %v, want 2", pi(resp.UploadQueueCount))
	}
	if resp.ImmediateQueueCount == nil || *resp.ImmediateQueueCount != 1 {
		t.Errorf("immediate_queue_count = %v, want 1", pi(resp.ImmediateQueueCount))
	}
	if resp.RawQueueCount == nil || *resp.RawQueueCount != 1 {
		t.Errorf("raw_queue_count = %v, want 1", pi(resp.RawQueueCount))
	}
	if resp.FreeSpaceGB != nil {
		t.Errorf("free_space_gb = %v, want nil (deviceState rpc failed)", *resp.FreeSpaceGB)
	}
	if resp.CPUUsagePercent != nil {
		t.Errorf("cpu_usage_percent = %v, want nil (deviceState rpc failed)", *resp.CPUUsagePercent)
	}
}

// TestGetLiveOfflineServesExtendedCache verifies that the new analytics
// fields (CPU, MEM, queue counts, currently-uploading) survive a device
// disconnect just like the original network/free_space fields do.
func TestGetLiveOfflineServesExtendedCache(t *testing.T) {
	h, _ := newTestLiveHandler()

	cpu := 33
	mem := 41
	queueTotal := 14
	immCount := 5
	rawCount := 9
	maxT := 51.0
	power := 7.1
	uploadingNow := true
	uploadingPath := "qlog.zst"
	uploadingProgress := 0.62

	cachedAt := h.now().Add(-2 * time.Minute)
	h.cache.Store("abc123", cachedLive{
		resp: liveResponse{
			Online:              true,
			CPUUsagePercent:     &cpu,
			MemoryUsagePercent:  &mem,
			MaxTempC:            &maxT,
			NetworkStrength:     "good",
			PowerDrawW:          &power,
			UploadQueueCount:    &queueTotal,
			ImmediateQueueCount: &immCount,
			RawQueueCount:       &rawCount,
			UploadingNow:        &uploadingNow,
			UploadingPath:       &uploadingPath,
			UploadingProgress:   &uploadingProgress,
			FetchedAt:           cachedAt,
		},
		at: cachedAt,
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.Online {
		t.Error("online = true, want false (device disconnected)")
	}
	if resp.CPUUsagePercent == nil || *resp.CPUUsagePercent != 33 {
		t.Errorf("cached cpu_usage_percent = %v, want 33", pi(resp.CPUUsagePercent))
	}
	if resp.MemoryUsagePercent == nil || *resp.MemoryUsagePercent != 41 {
		t.Errorf("cached memory_usage_percent = %v, want 41", pi(resp.MemoryUsagePercent))
	}
	if resp.MaxTempC == nil || *resp.MaxTempC != 51.0 {
		t.Errorf("cached max_temp_c = %v, want 51.0", pf(resp.MaxTempC))
	}
	if resp.NetworkStrength != "good" {
		t.Errorf("cached network_strength = %v, want \"good\"", resp.NetworkStrength)
	}
	if resp.PowerDrawW == nil || *resp.PowerDrawW != 7.1 {
		t.Errorf("cached power_draw_w = %v, want 7.1", pf(resp.PowerDrawW))
	}
	if resp.UploadQueueCount == nil || *resp.UploadQueueCount != 14 {
		t.Errorf("cached upload_queue_count = %v, want 14", pi(resp.UploadQueueCount))
	}
	if resp.ImmediateQueueCount == nil || *resp.ImmediateQueueCount != 5 {
		t.Errorf("cached immediate_queue_count = %v, want 5", pi(resp.ImmediateQueueCount))
	}
	if resp.RawQueueCount == nil || *resp.RawQueueCount != 9 {
		t.Errorf("cached raw_queue_count = %v, want 9", pi(resp.RawQueueCount))
	}
	if resp.UploadingNow == nil || *resp.UploadingNow != true {
		t.Error("cached uploading_now should be true")
	}
	if resp.UploadingPath == nil || *resp.UploadingPath != "qlog.zst" {
		t.Errorf("cached uploading_path = %v, want qlog.zst", resp.UploadingPath)
	}
	if resp.UploadingProgress == nil || *resp.UploadingProgress != 0.62 {
		t.Errorf("cached uploading_progress = %v, want 0.62", pf(resp.UploadingProgress))
	}
	if resp.CachedAt == nil {
		t.Error("expected cached_at when serving from cache")
	}
}

func TestGetLiveParallelDispatch(t *testing.T) {
	h, hub := newTestLiveHandler()

	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	// Each RPC sleeps for 100ms. If the handler were serial the wall clock
	// would be at least 400ms; in parallel it should finish near 100ms.
	// We assert a conservative upper bound to avoid CI flakiness.
	var running atomic.Int32
	var maxConcurrent atomic.Int32
	slow := func() {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	h.callNetworkType = func(_ *ws.Client) (interface{}, error) {
		slow()
		return "LTE", nil
	}
	h.callNetworkMetered = func(_ *ws.Client) (bool, error) {
		slow()
		return false, nil
	}
	h.callSimInfo = func(_ *ws.Client) (interface{}, error) {
		slow()
		return map[string]interface{}{}, nil
	}
	h.callDeviceState = func(_ *ws.Client) (map[string]interface{}, error) {
		slow()
		return map[string]interface{}{}, nil
	}
	h.callListUploadQueue = func(_ *ws.Client) ([]ws.UploadItem, error) {
		slow()
		return []ws.UploadItem{}, nil
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	start := time.Now()
	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Errorf("handler took %s; RPCs are probably serial", elapsed)
	}
	if got := maxConcurrent.Load(); got < 2 {
		t.Errorf("max concurrent RPCs = %d, want >= 2", got)
	}
}

func TestGetLiveMissingDongleID(t *testing.T) {
	h, _ := newTestLiveHandler()

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices//live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to unmarshal error body: %v", err)
	}
	if body.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestGetLiveNilHubOffline(t *testing.T) {
	h := NewDeviceLiveHandler(nil, nil)
	h.now = func() time.Time { return time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC) }

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/live", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := h.GetLive(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp liveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Online {
		t.Error("online = true, want false when hub is nil")
	}
}

func TestDeviceLiveRegisterRoutes(t *testing.T) {
	h := NewDeviceLiveHandler(nil, nil)

	e := echo.New()
	g := e.Group("/v1")
	h.RegisterRoutes(g)

	routes := e.Routes()
	found := false
	for _, r := range routes {
		if r.Method == http.MethodGet && r.Path == "/v1/devices/:dongle_id/live" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected route GET /v1/devices/:dongle_id/live to be registered")
	}
}

func TestExtractDeviceState(t *testing.T) {
	tests := []struct {
		name        string
		msg         map[string]interface{}
		wantFree    *float64
		wantThermal *int
	}{
		{
			name:        "camelCase keys",
			msg:         map[string]interface{}{"deviceState": map[string]interface{}{"freeSpaceGB": 10.5, "thermalStatus": 2.0}},
			wantFree:    ptrFloat(10.5),
			wantThermal: ptrInt(2),
		},
		{
			name:        "snake_case keys",
			msg:         map[string]interface{}{"deviceState": map[string]interface{}{"free_space_gb": 7.2, "thermal_status": 3.0}},
			wantFree:    ptrFloat(7.2),
			wantThermal: ptrInt(3),
		},
		{
			name:        "freeSpacePercent fallback",
			msg:         map[string]interface{}{"deviceState": map[string]interface{}{"freeSpacePercent": 0.85, "thermalStatus": 0.0}},
			wantFree:    ptrFloat(0.85),
			wantThermal: ptrInt(0),
		},
		{
			name:        "missing deviceState",
			msg:         map[string]interface{}{"other": map[string]interface{}{}},
			wantFree:    nil,
			wantThermal: nil,
		},
		{
			name:        "nil message",
			msg:         nil,
			wantFree:    nil,
			wantThermal: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds := extractDeviceState(tt.msg)
			if !floatPtrEqual(ds.freeSpaceGB, tt.wantFree) {
				t.Errorf("free = %v, want %v", pf(ds.freeSpaceGB), pf(tt.wantFree))
			}
			if !intPtrEqual(ds.thermalStatus, tt.wantThermal) {
				t.Errorf("thermal = %v, want %v", pi(ds.thermalStatus), pi(tt.wantThermal))
			}
		})
	}
}

func TestExtractDeviceStateExtended(t *testing.T) {
	msg := map[string]interface{}{
		"deviceState": map[string]interface{}{
			"freeSpaceGB":        15.0,
			"thermalStatus":      1.0,
			"cpuUsagePercent":    []interface{}{12.0, 47.0, 9.0, 5.0},
			"memoryUsagePercent": 38.0,
			"maxTempC":           62.5,
			"networkStrength":    "good",
			"powerDrawW":         8.4,
		},
	}
	ds := extractDeviceState(msg)
	if ds.cpuUsagePercent == nil || *ds.cpuUsagePercent != 47 {
		t.Errorf("cpuUsagePercent = %v, want peak 47", pi(ds.cpuUsagePercent))
	}
	if ds.memoryUsagePercent == nil || *ds.memoryUsagePercent != 38 {
		t.Errorf("memoryUsagePercent = %v, want 38", pi(ds.memoryUsagePercent))
	}
	if ds.maxTempC == nil || *ds.maxTempC != 62.5 {
		t.Errorf("maxTempC = %v, want 62.5", pf(ds.maxTempC))
	}
	if ds.networkStrength != "good" {
		t.Errorf("networkStrength = %v, want \"good\"", ds.networkStrength)
	}
	if ds.powerDrawW == nil || *ds.powerDrawW != 8.4 {
		t.Errorf("powerDrawW = %v, want 8.4", pf(ds.powerDrawW))
	}
}

func TestExtractDeviceStateCPUScalarFallback(t *testing.T) {
	// Some firmwares may report cpuUsagePercent as a scalar Int8 instead of a
	// per-core list. The extractor should tolerate both shapes.
	msg := map[string]interface{}{
		"deviceState": map[string]interface{}{"cpuUsagePercent": 22.0},
	}
	ds := extractDeviceState(msg)
	if ds.cpuUsagePercent == nil || *ds.cpuUsagePercent != 22 {
		t.Errorf("scalar cpuUsagePercent = %v, want 22", pi(ds.cpuUsagePercent))
	}
}

func TestExtractUploadQueueEmpty(t *testing.T) {
	q := extractUploadQueue(nil)
	if q.totalCount != 0 || q.immediateCount != 0 || q.rawCount != 0 {
		t.Errorf("nil queue: totals = %d/%d/%d, want 0/0/0", q.totalCount, q.immediateCount, q.rawCount)
	}
	if q.uploadingNow {
		t.Error("nil queue: uploadingNow should be false")
	}

	q = extractUploadQueue([]ws.UploadItem{})
	if q.totalCount != 0 {
		t.Errorf("empty queue: totalCount = %d, want 0", q.totalCount)
	}
}

func TestExtractUploadQueuePrioritySplit(t *testing.T) {
	// All-99 queue (the typical real-device case with athenad's
	// DEFAULT_UPLOAD_PRIORITY) lands entirely in the raw bucket.
	q := extractUploadQueue([]ws.UploadItem{
		{ID: "a", Priority: 99},
		{ID: "b", Priority: 99},
		{ID: "c", Priority: 99},
	})
	if q.totalCount != 3 || q.immediateCount != 0 || q.rawCount != 3 {
		t.Errorf("all-99 queue: totals = %d/%d/%d, want 3/0/3", q.totalCount, q.immediateCount, q.rawCount)
	}

	// Mixed priorities: anything < 99 is immediate.
	q = extractUploadQueue([]ws.UploadItem{
		{ID: "a", Priority: 1},
		{ID: "b", Priority: 50},
		{ID: "c", Priority: 99},
		{ID: "d", Priority: 100},
	})
	if q.totalCount != 4 || q.immediateCount != 2 || q.rawCount != 2 {
		t.Errorf("mixed queue: totals = %d/%d/%d, want 4/2/2", q.totalCount, q.immediateCount, q.rawCount)
	}
}

func TestExtractUploadQueueActiveItem(t *testing.T) {
	// Active item with progress: surface basename + progress fraction.
	q := extractUploadQueue([]ws.UploadItem{
		{ID: "a", Path: "/data/foo/bar/qlog.zst", Priority: 1, Current: true, Progress: 0.37},
		{ID: "b", Path: "/data/foo/bar/fcamera.hevc", Priority: 99},
	})
	if !q.uploadingNow {
		t.Error("uploadingNow should be true when an item is current")
	}
	if q.uploadingPath != "qlog.zst" {
		t.Errorf("uploadingPath = %q, want qlog.zst (basename)", q.uploadingPath)
	}
	if q.uploadingProgress == nil || *q.uploadingProgress != 0.37 {
		t.Errorf("uploadingProgress = %v, want 0.37", pf(q.uploadingProgress))
	}

	// Active item with zero progress: progress stays nil so the UI can
	// distinguish "in-flight, no progress reported yet" from "37% done".
	q = extractUploadQueue([]ws.UploadItem{
		{ID: "a", Path: "/data/foo/qlog.zst", Priority: 1, Current: true, Progress: 0},
	})
	if !q.uploadingNow {
		t.Error("uploadingNow should be true even when progress is 0")
	}
	if q.uploadingProgress != nil {
		t.Errorf("uploadingProgress = %v, want nil for zero progress", pf(q.uploadingProgress))
	}

	// No current item: uploadingNow false, path empty.
	q = extractUploadQueue([]ws.UploadItem{
		{ID: "a", Path: "/data/foo/qlog.zst", Priority: 99, Current: false},
	})
	if q.uploadingNow {
		t.Error("uploadingNow should be false when no item is current")
	}
	if q.uploadingPath != "" {
		t.Errorf("uploadingPath = %q, want empty when no current item", q.uploadingPath)
	}
}

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(n int) *int           { return &n }

func floatPtrEqual(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func intPtrEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func pf(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%f", *p)
}

func pi(p *int) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%d", *p)
}
