package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/ws"
)

// stubSnapshotCaller is a SnapshotCaller for tests. It either returns a
// canned SnapshotResult or an error, and records the number of calls it
// received so the rate-limit assertions can verify the caller was not
// invoked when the limiter rejected a request.
type stubSnapshotCaller struct {
	mu     sync.Mutex
	calls  int
	result *ws.SnapshotResult
	err    error
}

func (s *stubSnapshotCaller) TakeSnapshot(_ *ws.Client) (*ws.SnapshotResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

func (s *stubSnapshotCaller) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stubHub is a minimal SnapshotClientGetter implementation backed by a map.
// Entries with a nil *ws.Client simulate "registered but not connected" (the
// GetClient call returns ok=false). Missing entries simulate "unknown device".
type stubHub struct {
	clients map[string]*ws.Client
}

func (s *stubHub) GetClient(dongleID string) (*ws.Client, bool) {
	if s == nil {
		return nil, false
	}
	c, ok := s.clients[dongleID]
	return c, ok
}

// newSnapshotTestRequest returns a POST request + recorder + Echo context
// wired with the dongle_id path param. The caller can mark the request as
// device-authenticated by passing a non-empty authDongleID (which sets the
// same context key JWTAuthFromDB would on a live request).
func newSnapshotTestRequest(dongleID, authDongleID string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/"+dongleID+"/snapshot", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues(dongleID)
	if authDongleID != "" {
		c.Set(middleware.ContextKeyDongleID, authDongleID)
	}
	return c, rec
}

func TestSnapshot_OfflineReturns503(t *testing.T) {
	caller := &stubSnapshotCaller{}
	hub := &stubHub{clients: map[string]*ws.Client{}}
	h := newSnapshotHandler(hub, caller)

	c, rec := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	var errResp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	if errResp.Error != "device is offline" {
		t.Errorf("error = %q, want %q", errResp.Error, "device is offline")
	}
	if errResp.Code != http.StatusServiceUnavailable {
		t.Errorf("error code = %d, want %d", errResp.Code, http.StatusServiceUnavailable)
	}
	if caller.Calls() != 0 {
		t.Errorf("expected 0 RPC calls for offline device, got %d", caller.Calls())
	}
}

func TestSnapshot_SuccessReturnsDataURLs(t *testing.T) {
	// Use a non-nil *ws.Client value so the stub hub can report the device
	// as connected. The client is never actually exercised because the
	// SnapshotCaller stub short-circuits the RPC.
	connected := &ws.Client{}
	hub := &stubHub{clients: map[string]*ws.Client{"abc123": connected}}
	caller := &stubSnapshotCaller{result: &ws.SnapshotResult{
		JpegBack:  "BACKBASE64",
		JpegFront: "FRONTBASE64",
	}}
	h := newSnapshotHandler(hub, caller)

	c, rec := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	wantBack := "data:image/jpeg;base64,BACKBASE64"
	wantFront := "data:image/jpeg;base64,FRONTBASE64"
	if resp.JpegBack != wantBack {
		t.Errorf("jpeg_back = %q, want %q", resp.JpegBack, wantBack)
	}
	if resp.JpegFront != wantFront {
		t.Errorf("jpeg_front = %q, want %q", resp.JpegFront, wantFront)
	}
	if caller.Calls() != 1 {
		t.Errorf("expected 1 RPC call, got %d", caller.Calls())
	}
}

func TestSnapshot_RawStringFallback(t *testing.T) {
	connected := &ws.Client{}
	hub := &stubHub{clients: map[string]*ws.Client{"abc123": connected}}
	caller := &stubSnapshotCaller{result: &ws.SnapshotResult{
		RawString: "LEGACY",
	}}
	h := newSnapshotHandler(hub, caller)

	c, rec := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.JpegBack != "data:image/jpeg;base64,LEGACY" {
		t.Errorf("jpeg_back = %q, want data-url-wrapped LEGACY", resp.JpegBack)
	}
	if resp.JpegFront != "" {
		t.Errorf("jpeg_front = %q, want empty", resp.JpegFront)
	}
}

func TestSnapshot_RateLimitBlocksSecondHit(t *testing.T) {
	connected := &ws.Client{}
	hub := &stubHub{clients: map[string]*ws.Client{"abc123": connected}}
	caller := &stubSnapshotCaller{result: &ws.SnapshotResult{
		JpegBack:  "B",
		JpegFront: "F",
	}}
	h := newSnapshotHandler(hub, caller)

	// Drive a deterministic clock so the test does not sleep.
	base := time.Now()
	now := base
	h.now = func() time.Time { return now }

	// First hit succeeds and populates the last-hit bucket.
	c1, rec1 := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c1); err != nil {
		t.Fatalf("first call returned error: %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", rec1.Code)
	}

	// Second hit within the rate-limit window must be rejected with 429 and
	// must not reach the RPC caller.
	now = base.Add(1 * time.Second)
	c2, rec2 := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c2); err != nil {
		t.Fatalf("second call returned error: %v", err)
	}
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second call status = %d, want 429; body = %s", rec2.Code, rec2.Body.String())
	}

	var errResp errorResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}
	if !strings.Contains(errResp.Error, "rate limit") {
		t.Errorf("error = %q, want it to mention rate limit", errResp.Error)
	}
	if caller.Calls() != 1 {
		t.Errorf("expected exactly 1 RPC call (second blocked), got %d", caller.Calls())
	}

	// After the rate-limit window elapses, a third call must succeed again.
	now = base.Add(snapshotRateLimit + 1*time.Millisecond)
	c3, rec3 := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c3); err != nil {
		t.Fatalf("third call returned error: %v", err)
	}
	if rec3.Code != http.StatusOK {
		t.Errorf("third call status = %d, want 200; body = %s", rec3.Code, rec3.Body.String())
	}
	if caller.Calls() != 2 {
		t.Errorf("expected 2 RPC calls after window, got %d", caller.Calls())
	}
}

func TestSnapshot_RateLimitPerDongle(t *testing.T) {
	connected := &ws.Client{}
	hub := &stubHub{clients: map[string]*ws.Client{"dev1": connected, "dev2": connected}}
	caller := &stubSnapshotCaller{result: &ws.SnapshotResult{JpegBack: "B"}}
	h := newSnapshotHandler(hub, caller)

	base := time.Now()
	h.now = func() time.Time { return base }

	// Session-authenticated operators (no dongle_id in context) can target
	// any device, so we leave the auth dongleID empty here.
	c1, rec1 := newSnapshotTestRequest("dev1", "")
	if err := h.TakeSnapshot(c1); err != nil {
		t.Fatalf("dev1 call returned error: %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("dev1 status = %d, want 200", rec1.Code)
	}

	c2, rec2 := newSnapshotTestRequest("dev2", "")
	if err := h.TakeSnapshot(c2); err != nil {
		t.Fatalf("dev2 call returned error: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("dev2 status = %d, want 200; body = %s", rec2.Code, rec2.Body.String())
	}

	// Immediately re-hit dev1: must be rate-limited even though dev2 was
	// just served.
	c3, rec3 := newSnapshotTestRequest("dev1", "")
	if err := h.TakeSnapshot(c3); err != nil {
		t.Fatalf("dev1 retry returned error: %v", err)
	}
	if rec3.Code != http.StatusTooManyRequests {
		t.Errorf("dev1 retry status = %d, want 429", rec3.Code)
	}
}

func TestSnapshot_RPCErrorReturns502(t *testing.T) {
	connected := &ws.Client{}
	hub := &stubHub{clients: map[string]*ws.Client{"abc123": connected}}
	caller := &stubSnapshotCaller{err: fmt.Errorf("rpc timeout")}
	h := newSnapshotHandler(hub, caller)

	c, rec := newSnapshotTestRequest("abc123", "abc123")
	if err := h.TakeSnapshot(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshot_JWTDongleIDMismatch(t *testing.T) {
	connected := &ws.Client{}
	hub := &stubHub{clients: map[string]*ws.Client{"abc123": connected}}
	caller := &stubSnapshotCaller{result: &ws.SnapshotResult{JpegBack: "B"}}
	h := newSnapshotHandler(hub, caller)

	// Device "xyz789" tries to snapshot "abc123" -- must be forbidden.
	c, rec := newSnapshotTestRequest("abc123", "xyz789")
	if err := h.TakeSnapshot(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if caller.Calls() != 0 {
		t.Errorf("expected 0 RPC calls on forbidden request, got %d", caller.Calls())
	}
}

func TestSnapshot_MissingDongleIDReturns400(t *testing.T) {
	caller := &stubSnapshotCaller{}
	h := newSnapshotHandler(&stubHub{}, caller)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/devices//snapshot", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	// Intentionally no dongle_id param set.

	if err := h.TakeSnapshot(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSnapshot_RegisterRoutes(t *testing.T) {
	h := newSnapshotHandler(&stubHub{}, &stubSnapshotCaller{})

	e := echo.New()
	g := e.Group("/v1")
	h.RegisterRoutes(g)

	expected := map[string]bool{
		"POST /v1/devices/:dongle_id/snapshot":  true,
		"POST /v1/devices/:dongle_id/snapshot/": true,
	}

	for _, r := range e.Routes() {
		key := r.Method + " " + r.Path
		delete(expected, key)
	}
	for route := range expected {
		t.Errorf("expected route %s to be registered", route)
	}
}

func TestExtractJpegs_NilResult(t *testing.T) {
	back, front := extractJpegs(nil)
	if back != "" || front != "" {
		t.Errorf("expected empty strings for nil input, got back=%q front=%q", back, front)
	}
}

func TestToJpegDataURL_PreservesExistingDataURL(t *testing.T) {
	original := "data:image/png;base64,ALREADY_WRAPPED"
	got := toJpegDataURL(original)
	if got != original {
		t.Errorf("toJpegDataURL(%q) = %q, want input unchanged", original, got)
	}
}
