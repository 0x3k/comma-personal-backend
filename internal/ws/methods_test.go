package ws

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRPCCaller_NextRequestID_Increments(t *testing.T) {
	caller := NewRPCCaller()

	id1 := caller.nextRequestID()
	id2 := caller.nextRequestID()
	id3 := caller.nextRequestID()

	if string(id1) != "1" {
		t.Errorf("first ID = %s, want 1", string(id1))
	}
	if string(id2) != "2" {
		t.Errorf("second ID = %s, want 2", string(id2))
	}
	if string(id3) != "3" {
		t.Errorf("third ID = %s, want 3", string(id3))
	}
}

func TestRPCCaller_HandleResponse_MatchesPending(t *testing.T) {
	caller := NewRPCCaller()

	// Simulate a pending call.
	pc := &pendingCall{ch: make(chan *RPCResponse, 1)}
	caller.mu.Lock()
	caller.pending["42"] = pc
	caller.mu.Unlock()

	resp := &RPCResponse{
		JSONRPC: "2.0",
		Result:  "ok",
		ID:      json.RawMessage(`42`),
	}

	matched := caller.HandleResponse(resp)
	if !matched {
		t.Fatal("HandleResponse returned false, expected true")
	}

	select {
	case got := <-pc.ch:
		if got.Result != "ok" {
			t.Errorf("Result = %v, want %q", got.Result, "ok")
		}
	default:
		t.Fatal("expected response on pending call channel")
	}
}

func TestRPCCaller_HandleResponse_NoMatch(t *testing.T) {
	caller := NewRPCCaller()

	resp := &RPCResponse{
		JSONRPC: "2.0",
		Result:  "ok",
		ID:      json.RawMessage(`999`),
	}

	matched := caller.HandleResponse(resp)
	if matched {
		t.Fatal("HandleResponse returned true for unmatched ID")
	}
}

func TestRPCCaller_HandleResponse_NilID(t *testing.T) {
	caller := NewRPCCaller()

	resp := &RPCResponse{
		JSONRPC: "2.0",
		Result:  "ok",
	}

	matched := caller.HandleResponse(resp)
	if matched {
		t.Fatal("HandleResponse returned true for nil ID")
	}
}

// testClientWithResponder creates a Client whose sendCh is drained by a
// goroutine that parses outgoing RPCRequests and sends back a canned response
// via the caller's HandleResponse.
func testClientWithResponder(t *testing.T, caller *RPCCaller, result interface{}, rpcErr *RPCError) *Client {
	t.Helper()
	hub := NewHub()
	c := &Client{
		DongleID: "test-device",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	go func() {
		for msg := range c.sendCh {
			var req RPCRequest
			if err := json.Unmarshal(msg, &req); err != nil {
				continue
			}
			resp := &RPCResponse{
				JSONRPC: jsonRPCVersion,
				ID:      req.ID,
				Result:  result,
				Error:   rpcErr,
			}
			caller.HandleResponse(resp)
		}
	}()

	t.Cleanup(func() {
		c.Close()
	})

	return c
}

func TestCallGetNetworkType(t *testing.T) {
	caller := NewRPCCaller()
	networkResult := map[string]interface{}{
		"network_type": float64(5),
	}
	client := testClientWithResponder(t, caller, networkResult, nil)

	result, err := CallGetNetworkType(caller, client)
	if err != nil {
		t.Fatalf("CallGetNetworkType returned error: %v", err)
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not a map, got %T", result)
	}
	if resultMap["network_type"] != float64(5) {
		t.Errorf("network_type = %v, want 5", resultMap["network_type"])
	}
}

func TestCallGetNetworkType_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "device error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetNetworkType(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetNetworkType")
	}
}

func TestCallGetSimInfo(t *testing.T) {
	caller := NewRPCCaller()
	simResult := map[string]interface{}{
		"sim_id": float64(0),
		"state":  "READY",
	}
	client := testClientWithResponder(t, caller, simResult, nil)

	result, err := CallGetSimInfo(caller, client)
	if err != nil {
		t.Fatalf("CallGetSimInfo returned error: %v", err)
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not a map, got %T", result)
	}
	if resultMap["state"] != "READY" {
		t.Errorf("state = %v, want READY", resultMap["state"])
	}
}

func TestCallGetSimInfo_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "sim error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetSimInfo(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetSimInfo")
	}
}

func TestCallUploadFileToUrl(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, map[string]int{"enqueued": 1}, nil)

	err := CallUploadFileToUrl(caller, client, "https://example.com/upload", map[string]string{"Authorization": "Bearer tok"}, "/data/rlog")
	if err != nil {
		t.Fatalf("CallUploadFileToUrl returned error: %v", err)
	}
}

func TestCallUploadFileToUrl_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "upload failed")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	err := CallUploadFileToUrl(caller, client, "https://example.com/upload", nil, "/data/rlog")
	if err == nil {
		t.Fatal("expected error from CallUploadFileToUrl")
	}
}

func TestCallUploadFileToUrl_ParamsMarshaled(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "param-check",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	// Start a goroutine that reads the request, verifies params, and responds.
	go func() {
		msg := <-c.sendCh
		var req RPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		var p UploadFileToUrlParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		if p.URL != "https://upload.example.com" || p.Path != "/tmp/file.hevc" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected params")
		} else {
			resp.Result = map[string]bool{"ok": true}
		}
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })

	err := CallUploadFileToUrl(caller, c, "https://upload.example.com", map[string]string{"X-Custom": "val"}, "/tmp/file.hevc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCallSetNavDestination(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, map[string]bool{"success": true}, nil)

	err := CallSetNavDestination(caller, client, 37.7749, -122.4194, "San Francisco")
	if err != nil {
		t.Fatalf("CallSetNavDestination returned error: %v", err)
	}
}

func TestCallSetNavDestination_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "nav error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	err := CallSetNavDestination(caller, client, 0, 0, "")
	if err == nil {
		t.Fatal("expected error from CallSetNavDestination")
	}
}

func TestCallSetNavDestination_ParamsMarshaled(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "nav-params",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	go func() {
		msg := <-c.sendCh
		var req RPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		var p SetNavDestinationParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		if p.Latitude != 40.7128 || p.Longitude != -74.006 || p.Place != "New York" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected params")
		} else {
			resp.Result = map[string]bool{"success": true}
		}
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })

	err := CallSetNavDestination(caller, c, 40.7128, -74.006, "New York")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRPCCaller_Timeout(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	// Client that never responds.
	c := &Client{
		DongleID: "timeout-device",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}
	t.Cleanup(func() { c.Close() })

	_, err := caller.CallWithTimeout(c, "getNetworkType", nil, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRPCCaller_ClientDisconnect(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "disconnect-device",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	// Close the client after a short delay to simulate disconnect.
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.Close()
	}()

	_, err := caller.CallWithTimeout(c, "getSimInfo", nil, 5*time.Second)
	if err == nil {
		t.Fatal("expected disconnect error")
	}
}

// Device-side handler tests.

func TestHandleUploadFileToUrl_Valid(t *testing.T) {
	params := json.RawMessage(`{"url":"https://example.com/upload","headers":{"Authorization":"Bearer tok"},"fn":"/data/rlog"}`)

	result, rpcErr := handleUploadFileToUrl("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]int)
	if !ok {
		t.Fatalf("result is not map[string]int, got %T", result)
	}
	if m["enqueued"] != 1 {
		t.Errorf("enqueued = %d, want 1", m["enqueued"])
	}
}

func TestHandleUploadFileToUrl_MissingURL(t *testing.T) {
	params := json.RawMessage(`{"fn":"/data/rlog"}`)

	_, rpcErr := handleUploadFileToUrl("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for missing url")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleUploadFileToUrl_MissingPath(t *testing.T) {
	params := json.RawMessage(`{"url":"https://example.com/upload"}`)

	_, rpcErr := handleUploadFileToUrl("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for missing fn")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleUploadFileToUrl_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`not json`)

	_, rpcErr := handleUploadFileToUrl("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleGetNetworkType(t *testing.T) {
	result, rpcErr := handleGetNetworkType("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map[string]interface{}, got %T", result)
	}
	if m["network_type"] != 5 {
		t.Errorf("network_type = %v, want 5", m["network_type"])
	}
}

func TestHandleGetSimInfo(t *testing.T) {
	result, rpcErr := handleGetSimInfo("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map[string]interface{}, got %T", result)
	}
	if m["sim_id"] != 0 {
		t.Errorf("sim_id = %v, want 0", m["sim_id"])
	}
}

func TestHandleSetNavDestination_Valid(t *testing.T) {
	params := json.RawMessage(`{"latitude":37.7749,"longitude":-122.4194,"place_name":"San Francisco"}`)

	result, rpcErr := handleSetNavDestination("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]bool)
	if !ok {
		t.Fatalf("result is not map[string]bool, got %T", result)
	}
	if !m["success"] {
		t.Error("expected success=true")
	}
}

func TestHandleSetNavDestination_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`not json`)

	_, rpcErr := handleSetNavDestination("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestRegisterDefaultHandlers(t *testing.T) {
	handlers := make(map[string]MethodHandler)
	RegisterDefaultHandlers(handlers)

	expected := []string{
		"uploadFileToUrl",
		"getNetworkType",
		"getSimInfo",
		"setNavDestination",
		"getMessage",
	}

	for _, method := range expected {
		if _, ok := handlers[method]; !ok {
			t.Errorf("handler not registered for method %q", method)
		}
	}
}

func TestCallGetMessage_HappyPath(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "msg-happy",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}
	t.Cleanup(func() { c.Close() })

	go func() {
		msg := <-c.sendCh
		var req RPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		var p GetMessageParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		if req.Method != "getMessage" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected method")
		} else if p.Service != "carState" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected service")
		} else if p.Timeout != 1000 {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected timeout default")
		} else {
			resp.Result = map[string]interface{}{
				"carState": map[string]interface{}{
					"vEgo": 12.5,
				},
			}
		}
		caller.HandleResponse(resp)
	}()

	// timeoutMs <= 0 should default to 1000 ms.
	result, err := CallGetMessage(caller, c, "carState", 0)
	if err != nil {
		t.Fatalf("CallGetMessage returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	inner, ok := result["carState"].(map[string]interface{})
	if !ok {
		t.Fatalf("result[carState] is not a map, got %T", result["carState"])
	}
	if inner["vEgo"] != 12.5 {
		t.Errorf("vEgo = %v, want 12.5", inner["vEgo"])
	}
}

func TestCallGetMessage_InvalidService(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInvalidParams, "invalid service: \"bogusService\"")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetMessage(caller, client, "bogusService", 100)
	if err == nil {
		t.Fatal("expected error from CallGetMessage with invalid service")
	}
}

func TestCallGetMessage_TimeoutBuffer(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	// Client that never responds, so we always hit the Go-side timeout.
	c := &Client{
		DongleID: "msg-timeout",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}
	t.Cleanup(func() { c.Close() })

	// Drain outgoing requests so sends don't block.
	go func() {
		for range c.sendCh {
		}
	}()

	const deviceTimeoutMs = 50
	start := time.Now()
	_, err := CallGetMessage(caller, c, "carState", deviceTimeoutMs)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error from CallGetMessage")
	}

	minExpected := time.Duration(deviceTimeoutMs+getMessageCallBufferMs) * time.Millisecond
	if elapsed < minExpected {
		t.Errorf("elapsed = %v, want >= %v (device timeout + buffer)", elapsed, minExpected)
	}

	// Sanity: don't wait unreasonably long beyond the buffer (allow scheduling slack).
	maxExpected := minExpected + 500*time.Millisecond
	if elapsed > maxExpected {
		t.Errorf("elapsed = %v, want <= %v", elapsed, maxExpected)
	}
}

func TestHandleGetMessage_Valid(t *testing.T) {
	params := json.RawMessage(`{"service":"deviceState","timeout":250}`)

	result, rpcErr := handleGetMessage("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map[string]interface{}, got %T", result)
	}
	inner, ok := m["deviceState"].(map[string]interface{})
	if !ok {
		t.Fatalf("result[deviceState] is not a map, got %T", m["deviceState"])
	}
	if inner["service"] != "deviceState" {
		t.Errorf("service = %v, want deviceState", inner["service"])
	}
}

func TestHandleGetMessage_MissingService(t *testing.T) {
	params := json.RawMessage(`{"timeout":100}`)

	_, rpcErr := handleGetMessage("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for missing service")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleGetMessage_UnknownService(t *testing.T) {
	params := json.RawMessage(`{"service":"notAService"}`)

	_, rpcErr := handleGetMessage("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for unknown service")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleGetMessage_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`not json`)

	_, rpcErr := handleGetMessage("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}
