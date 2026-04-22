package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/ws"
)

// uploadQueueResponder is a thin wrapper around ws.TestDrainResponderWith
// that keeps the test file self-documenting about what the goroutine is
// doing (answering the handler's RPC call with a canned payload).
func uploadQueueResponder(t *testing.T, c *ws.Client, caller *ws.RPCCaller, result interface{}, rpcErr *ws.RPCError) *ws.TestRPCRecorder {
	t.Helper()
	return ws.TestDrainResponderWith(c, caller, result, rpcErr)
}

func TestUploadQueueListQueue_Offline(t *testing.T) {
	// Device is not registered in the hub, so GetClient returns false and
	// the handler should respond 503 without attempting an RPC call.
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/upload-queue", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := handler.ListQueue(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	var resp errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if resp.Code != http.StatusServiceUnavailable || resp.Error == "" {
		t.Errorf("unexpected error envelope: %+v", resp)
	}
}

func TestUploadQueueListQueue_Online(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	// Device responds with two upload items.
	items := []ws.UploadItem{
		{
			ID:            "id-1",
			Path:          "/data/media/0/realdata/foo/rlog",
			URL:           "https://example.com/upload/rlog",
			Headers:       map[string]string{},
			Priority:      10,
			RetryCount:    0,
			CreatedAt:     1700000000,
			Current:       false,
			Progress:      0,
			AllowCellular: false,
		},
		{
			ID:            "id-2",
			Path:          "/data/media/0/realdata/foo/qlog",
			URL:           "https://example.com/upload/qlog",
			Headers:       map[string]string{},
			Priority:      5,
			RetryCount:    1,
			CreatedAt:     1700000100,
			Current:       true,
			Progress:      0.42,
			AllowCellular: true,
		},
	}
	uploadQueueResponder(t, client, rpc, items, nil)

	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/upload-queue", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	done := make(chan error, 1)
	go func() { done <- handler.ListQueue(c) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var got []ws.UploadItem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 items, got %d", len(got))
	}
	if got[0].ID != "id-1" || got[1].ID != "id-2" {
		t.Errorf("unexpected item order: %+v", got)
	}
	if got[1].Progress != 0.42 || !got[1].Current || !got[1].AllowCellular {
		t.Errorf("progress/current/allow_cellular not preserved: %+v", got[1])
	}
}

func TestUploadQueueListQueue_MissingDongleID(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices//upload-queue", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("")

	if err := handler.ListQueue(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestUploadQueueCancel_Offline(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	body := `{"ids":["a","b"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/abc123/upload-queue/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := handler.CancelUpload(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

func TestUploadQueueCancel_EmptyIDs(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	body := `{"ids":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/abc123/upload-queue/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := handler.CancelUpload(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestUploadQueueCancel_MalformedBody(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/abc123/upload-queue/cancel", strings.NewReader(`{"ids":`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	if err := handler.CancelUpload(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestUploadQueueCancel_Online(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	rec := uploadQueueResponder(t, client, rpc, map[string]int{"success": 1}, nil)

	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	body := `{"ids":["id-1","id-2"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/abc123/upload-queue/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c := e.NewContext(req, w)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	done := make(chan error, 1)
	go func() { done <- handler.CancelUpload(c) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// Verify the handler invoked cancelUpload with both IDs.
	if rec.Len() == 0 {
		t.Fatal("expected at least one RPC call")
	}
	if rec.Method(0) != "cancelUpload" {
		t.Errorf("RPC method = %q, want cancelUpload", rec.Method(0))
	}
	var ids []string
	if err := json.Unmarshal(rec.Params(0), &ids); err != nil {
		t.Fatalf("failed to decode RPC params: %v", err)
	}
	if len(ids) != 2 || ids[0] != "id-1" || ids[1] != "id-2" {
		t.Errorf("RPC params = %v, want [id-1 id-2]", ids)
	}

	var resp cancelUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	if got := resp.Result["success"]; got == nil {
		t.Errorf("response missing result.success: %+v", resp)
	}
}

func TestUploadQueueCancel_SingleIDSendsString(t *testing.T) {
	// Single-ID cancelUpload calls must send a bare JSON string rather than
	// a one-element array, matching athenad's expectation.
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()
	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	rec := uploadQueueResponder(t, client, rpc, map[string]int{"success": 1}, nil)

	handler := NewUploadQueueHandler(hub, rpc)

	e := echo.New()
	body := `{"ids":["only-one"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/devices/abc123/upload-queue/cancel", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c := e.NewContext(req, w)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")

	done := make(chan error, 1)
	go func() { done <- handler.CancelUpload(c) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return within 2s")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rec.Len() == 0 {
		t.Fatal("expected at least one RPC call")
	}
	var single string
	if err := json.Unmarshal(rec.Params(0), &single); err != nil {
		t.Fatalf("single-ID params were not a JSON string: %s (err=%v)", string(rec.Params(0)), err)
	}
	if single != "only-one" {
		t.Errorf("param = %q, want only-one", single)
	}
}
