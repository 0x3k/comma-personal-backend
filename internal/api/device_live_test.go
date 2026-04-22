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
				"freeSpaceGB":   42.0,
				"thermalStatus": 1.0,
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
			free, thermal := extractDeviceState(tt.msg)
			if !floatPtrEqual(free, tt.wantFree) {
				t.Errorf("free = %v, want %v", pf(free), pf(tt.wantFree))
			}
			if !intPtrEqual(thermal, tt.wantThermal) {
				t.Errorf("thermal = %v, want %v", pi(thermal), pi(tt.wantThermal))
			}
		})
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
