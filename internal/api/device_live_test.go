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
	h.callUploaderState = func(_ *ws.Client) (map[string]interface{}, error) {
		return map[string]interface{}{
			"uploaderState": map[string]interface{}{
				"lastSpeed":           2.5,
				"immediateQueueCount": 4.0,
				"immediateQueueSize":  1024.0,
				"rawQueueCount":       17.0,
				"rawQueueSize":        2048.0,
			},
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
	if resp.UploadSpeedMbps == nil || *resp.UploadSpeedMbps != 2.5 {
		t.Errorf("upload_speed_mbps = %v, want 2.5", pf(resp.UploadSpeedMbps))
	}
	if resp.ImmediateQueueCount == nil || *resp.ImmediateQueueCount != 4 {
		t.Errorf("immediate_queue_count = %v, want 4", pi(resp.ImmediateQueueCount))
	}
	if resp.RawQueueCount == nil || *resp.RawQueueCount != 17 {
		t.Errorf("raw_queue_count = %v, want 17", pi(resp.RawQueueCount))
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
	h.callUploaderState = func(_ *ws.Client) (map[string]interface{}, error) {
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
	if resp.UploadSpeedMbps != nil {
		t.Errorf("upload_speed_mbps = %v, want nil (rpc failed)", *resp.UploadSpeedMbps)
	}
}

// TestGetLiveUploaderRPCFailureLeavesDeviceFields asserts that when only the
// uploaderState RPC fails, the deviceState-derived fields (CPU, memory, free
// disk, etc.) still populate. This is the inverse of the partial-failure test
// and is required by the feature's acceptance criteria: failures of either
// RPC must not knock the other out.
func TestGetLiveUploaderRPCFailureLeavesDeviceFields(t *testing.T) {
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
	h.callUploaderState = func(_ *ws.Client) (map[string]interface{}, error) {
		return nil, errors.New("uploader rpc failed")
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
		t.Errorf("free_space_gb = %v, want 20.0 (deviceState should survive uploader failure)", pf(resp.FreeSpaceGB))
	}
	if resp.CPUUsagePercent == nil || *resp.CPUUsagePercent != 15 {
		t.Errorf("cpu_usage_percent = %v, want 15", pi(resp.CPUUsagePercent))
	}
	if resp.MemoryUsagePercent == nil {
		t.Error("memory_usage_percent should survive uploader failure")
	}
	if resp.UploadSpeedMbps != nil {
		t.Errorf("upload_speed_mbps = %v, want nil (rpc failed)", *resp.UploadSpeedMbps)
	}
	if resp.ImmediateQueueCount != nil {
		t.Errorf("immediate_queue_count = %v, want nil (rpc failed)", *resp.ImmediateQueueCount)
	}
}

// TestGetLiveDeviceStateFailureLeavesUploaderFields is the symmetric check:
// when deviceState RPC fails, uploaderState fields still populate.
func TestGetLiveDeviceStateFailureLeavesUploaderFields(t *testing.T) {
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
	h.callUploaderState = func(_ *ws.Client) (map[string]interface{}, error) {
		return map[string]interface{}{
			"uploaderState": map[string]interface{}{
				"lastSpeed":           3.14,
				"immediateQueueCount": 7.0,
				"rawQueueCount":       1.0,
			},
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

	if resp.UploadSpeedMbps == nil || *resp.UploadSpeedMbps != 3.14 {
		t.Errorf("upload_speed_mbps = %v, want 3.14", pf(resp.UploadSpeedMbps))
	}
	if resp.ImmediateQueueCount == nil || *resp.ImmediateQueueCount != 7 {
		t.Errorf("immediate_queue_count = %v, want 7", pi(resp.ImmediateQueueCount))
	}
	if resp.FreeSpaceGB != nil {
		t.Errorf("free_space_gb = %v, want nil (deviceState rpc failed)", *resp.FreeSpaceGB)
	}
	if resp.CPUUsagePercent != nil {
		t.Errorf("cpu_usage_percent = %v, want nil (deviceState rpc failed)", *resp.CPUUsagePercent)
	}
}

// TestGetLiveOfflineServesExtendedCache verifies that the new analytics
// fields (CPU, MEM, upload speed, queue depths) survive a device disconnect
// just like the original network/free_space fields do.
func TestGetLiveOfflineServesExtendedCache(t *testing.T) {
	h, _ := newTestLiveHandler()

	cpu := 33
	mem := 41
	speed := 1.25
	immCount := 5
	immSize := int64(2_000_000_000)
	rawCount := 9
	rawSize := int64(123)
	maxT := 51.0
	power := 7.1

	cachedAt := h.now().Add(-2 * time.Minute)
	h.cache.Store("abc123", cachedLive{
		resp: liveResponse{
			Online:                  true,
			CPUUsagePercent:         &cpu,
			MemoryUsagePercent:      &mem,
			MaxTempC:                &maxT,
			NetworkStrength:         "good",
			PowerDrawW:              &power,
			UploadSpeedMbps:         &speed,
			ImmediateQueueCount:     &immCount,
			ImmediateQueueSizeBytes: &immSize,
			RawQueueCount:           &rawCount,
			RawQueueSizeBytes:       &rawSize,
			FetchedAt:               cachedAt,
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
	if resp.UploadSpeedMbps == nil || *resp.UploadSpeedMbps != 1.25 {
		t.Errorf("cached upload_speed_mbps = %v, want 1.25", pf(resp.UploadSpeedMbps))
	}
	if resp.ImmediateQueueCount == nil || *resp.ImmediateQueueCount != 5 {
		t.Errorf("cached immediate_queue_count = %v, want 5", pi(resp.ImmediateQueueCount))
	}
	if resp.ImmediateQueueSizeBytes == nil || *resp.ImmediateQueueSizeBytes != 2_000_000_000 {
		t.Errorf("cached immediate_queue_size_bytes = %v, want 2e9", resp.ImmediateQueueSizeBytes)
	}
	if resp.RawQueueCount == nil || *resp.RawQueueCount != 9 {
		t.Errorf("cached raw_queue_count = %v, want 9", pi(resp.RawQueueCount))
	}
	if resp.RawQueueSizeBytes == nil || *resp.RawQueueSizeBytes != 123 {
		t.Errorf("cached raw_queue_size_bytes = %v, want 123", resp.RawQueueSizeBytes)
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
	h.callUploaderState = func(_ *ws.Client) (map[string]interface{}, error) {
		slow()
		return map[string]interface{}{}, nil
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

func TestExtractUploaderState(t *testing.T) {
	msg := map[string]interface{}{
		"uploaderState": map[string]interface{}{
			"lastSpeed":           1.75,
			"immediateQueueCount": 3.0,
			"immediateQueueSize":  4_500_000_000.0, // larger than int32 to exercise int64 path
			"rawQueueCount":       12.0,
			"rawQueueSize":        890.0,
		},
	}
	us := extractUploaderState(msg)
	if us.lastSpeedMbps == nil || *us.lastSpeedMbps != 1.75 {
		t.Errorf("lastSpeed = %v, want 1.75", pf(us.lastSpeedMbps))
	}
	if us.immediateQueueCount == nil || *us.immediateQueueCount != 3 {
		t.Errorf("immediateQueueCount = %v, want 3", pi(us.immediateQueueCount))
	}
	if us.immediateQueueSizeBytes == nil || *us.immediateQueueSizeBytes != 4_500_000_000 {
		t.Errorf("immediateQueueSize = %v, want 4.5e9", us.immediateQueueSizeBytes)
	}
	if us.rawQueueCount == nil || *us.rawQueueCount != 12 {
		t.Errorf("rawQueueCount = %v, want 12", pi(us.rawQueueCount))
	}
	if us.rawQueueSizeBytes == nil || *us.rawQueueSizeBytes != 890 {
		t.Errorf("rawQueueSize = %v, want 890", us.rawQueueSizeBytes)
	}
}

func TestExtractUploaderStateMissing(t *testing.T) {
	us := extractUploaderState(nil)
	if us.lastSpeedMbps != nil || us.immediateQueueCount != nil || us.rawQueueSizeBytes != nil {
		t.Error("expected all uploader fields nil for nil msg")
	}
	us = extractUploaderState(map[string]interface{}{"other": 1})
	if us.lastSpeedMbps != nil || us.immediateQueueCount != nil {
		t.Error("expected nil uploader fields when uploaderState key absent")
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
