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
		"listUploadQueue",
		"cancelUpload",
	}

	for _, method := range expected {
		if _, ok := handlers[method]; !ok {
			t.Errorf("handler not registered for method %q", method)
		}
	}
}

func TestCallListUploadQueue(t *testing.T) {
	caller := NewRPCCaller()
	// The JSON result arrives as []interface{} with map[string]interface{} entries;
	// emulate that by providing a typed []UploadItem which json.Marshal will encode
	// correctly when re-marshaled inside CallListUploadQueue.
	result := []UploadItem{
		{
			ID:            "abc123",
			Path:          "/data/media/0/realdata/foo/rlog",
			URL:           "https://example.com/upload/rlog",
			Headers:       map[string]string{"Authorization": "Bearer tok"},
			Priority:      10,
			RetryCount:    0,
			CreatedAt:     1700000000,
			Current:       false,
			Progress:      0,
			AllowCellular: false,
		},
		{
			ID:            "def456",
			Path:          "/data/media/0/realdata/foo/qlog",
			URL:           "https://example.com/upload/qlog",
			Headers:       map[string]string{},
			Priority:      5,
			RetryCount:    2,
			CreatedAt:     1700000100,
			Current:       true,
			Progress:      0.42,
			AllowCellular: true,
		},
	}
	client := testClientWithResponder(t, caller, result, nil)

	items, err := CallListUploadQueue(caller, client)
	if err != nil {
		t.Fatalf("CallListUploadQueue returned error: %v", err)
	}

	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].ID != "abc123" {
		t.Errorf("items[0].ID = %q, want abc123", items[0].ID)
	}
	if items[0].Headers["Authorization"] != "Bearer tok" {
		t.Errorf("items[0].Headers[Authorization] = %q, want Bearer tok", items[0].Headers["Authorization"])
	}
	if items[1].Progress != 0.42 {
		t.Errorf("items[1].Progress = %v, want 0.42", items[1].Progress)
	}
	if !items[1].Current {
		t.Errorf("items[1].Current = false, want true")
	}
	if !items[1].AllowCellular {
		t.Errorf("items[1].AllowCellular = false, want true")
	}
	if items[1].RetryCount != 2 {
		t.Errorf("items[1].RetryCount = %d, want 2", items[1].RetryCount)
	}
}

func TestCallListUploadQueue_Empty(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, []UploadItem{}, nil)

	items, err := CallListUploadQueue(caller, client)
	if err != nil {
		t.Fatalf("CallListUploadQueue returned error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty queue, got %d items", len(items))
	}
}

func TestCallListUploadQueue_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "queue unavailable")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallListUploadQueue(caller, client)
	if err == nil {
		t.Fatal("expected error from CallListUploadQueue")
	}
}

func TestCallListUploadQueue_FromRawDict(t *testing.T) {
	// Emulate the device sending back athenad's UploadItemDict shape (a list
	// of plain maps) and make sure the decoder still lands in UploadItem.
	caller := NewRPCCaller()
	dictResult := []map[string]interface{}{
		{
			"id":             "xyz",
			"path":           "/p",
			"url":            "https://u",
			"headers":        map[string]string{"A": "B"},
			"priority":       float64(7),
			"retry_count":    float64(1),
			"created_at":     float64(123),
			"current":        true,
			"progress":       0.5,
			"allow_cellular": true,
		},
	}
	client := testClientWithResponder(t, caller, dictResult, nil)

	items, err := CallListUploadQueue(caller, client)
	if err != nil {
		t.Fatalf("CallListUploadQueue returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].ID != "xyz" || items[0].Priority != 7 || items[0].RetryCount != 1 || items[0].CreatedAt != 123 {
		t.Errorf("decoded item mismatch: %+v", items[0])
	}
}

// cancelUploadParamsClient returns a Client that captures the params bytes of
// the first outgoing RPC request into out and responds with result.
func cancelUploadParamsClient(t *testing.T, caller *RPCCaller, out *json.RawMessage, result interface{}) *Client {
	t.Helper()
	hub := NewHub()
	c := &Client{
		DongleID: "cancel-params",
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
		// Copy the raw params so the test can inspect the wire shape.
		buf := make(json.RawMessage, len(req.Params))
		copy(buf, req.Params)
		*out = buf

		resp := &RPCResponse{
			JSONRPC: jsonRPCVersion,
			ID:      req.ID,
			Result:  result,
		}
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })
	return c
}

func TestCallCancelUpload_SingleID(t *testing.T) {
	caller := NewRPCCaller()
	var sentParams json.RawMessage
	client := cancelUploadParamsClient(t, caller, &sentParams, map[string]int{"success": 1})

	result, err := CallCancelUpload(caller, client, []string{"abc123"})
	if err != nil {
		t.Fatalf("CallCancelUpload returned error: %v", err)
	}

	// Single-ID branch must send a bare JSON string, not an array.
	var asString string
	if err := json.Unmarshal(sentParams, &asString); err != nil {
		t.Fatalf("params were not a JSON string: %s (err=%v)", string(sentParams), err)
	}
	if asString != "abc123" {
		t.Errorf("params = %q, want abc123", asString)
	}

	if v, ok := result["success"]; !ok || v.(float64) != 1 {
		t.Errorf("result[success] = %v (ok=%v), want 1", v, ok)
	}
}

func TestCallCancelUpload_MultipleIDs(t *testing.T) {
	caller := NewRPCCaller()
	var sentParams json.RawMessage
	client := cancelUploadParamsClient(t, caller, &sentParams, map[string]int{"success": 1})

	result, err := CallCancelUpload(caller, client, []string{"abc", "def", "ghi"})
	if err != nil {
		t.Fatalf("CallCancelUpload returned error: %v", err)
	}

	// Multi-ID branch must send a JSON array.
	var asList []string
	if err := json.Unmarshal(sentParams, &asList); err != nil {
		t.Fatalf("params were not a JSON array: %s (err=%v)", string(sentParams), err)
	}
	if len(asList) != 3 || asList[0] != "abc" || asList[2] != "ghi" {
		t.Errorf("params list = %v, want [abc def ghi]", asList)
	}

	// A bare string Unmarshal of the array should fail, confirming shape.
	var asString string
	if err := json.Unmarshal(sentParams, &asString); err == nil {
		t.Errorf("multi-ID params should not decode as a bare string, got %q", asString)
	}

	if result["success"].(float64) != 1 {
		t.Errorf("unexpected result: %v", result)
	}
}

func TestCallCancelUpload_EmptyIDs(t *testing.T) {
	// An empty slice should still take the list branch and send "[]".
	caller := NewRPCCaller()
	var sentParams json.RawMessage
	client := cancelUploadParamsClient(t, caller, &sentParams, map[string]interface{}{"success": 0, "error": "not found"})

	_, err := CallCancelUpload(caller, client, []string{})
	if err != nil {
		t.Fatalf("CallCancelUpload returned error: %v", err)
	}

	var asList []string
	if err := json.Unmarshal(sentParams, &asList); err != nil {
		t.Fatalf("params were not a JSON array: %s (err=%v)", string(sentParams), err)
	}
	if len(asList) != 0 {
		t.Errorf("expected empty list, got %v", asList)
	}
}

func TestCallCancelUpload_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "cancel failed")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallCancelUpload(caller, client, []string{"abc"})
	if err == nil {
		t.Fatal("expected error from CallCancelUpload")
	}
}

// Device-side handler tests.

func TestHandleListUploadQueue(t *testing.T) {
	result, rpcErr := handleListUploadQueue("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	items, ok := result.([]UploadItem)
	if !ok {
		t.Fatalf("result is not []UploadItem, got %T", result)
	}
	if len(items) != 0 {
		t.Errorf("expected empty queue stub, got %d items", len(items))
	}
}

func TestHandleCancelUpload_SingleString(t *testing.T) {
	result, rpcErr := handleCancelUpload("test-dongle", json.RawMessage(`"abc123"`))
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	m, ok := result.(map[string]int)
	if !ok {
		t.Fatalf("result is not map[string]int, got %T", result)
	}
	if m["success"] != 1 {
		t.Errorf("success = %d, want 1", m["success"])
	}
}

func TestHandleCancelUpload_List(t *testing.T) {
	result, rpcErr := handleCancelUpload("test-dongle", json.RawMessage(`["abc","def"]`))
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	m, ok := result.(map[string]int)
	if !ok {
		t.Fatalf("result is not map[string]int, got %T", result)
	}
	if m["success"] != 1 {
		t.Errorf("success = %d, want 1", m["success"])
	}
}

func TestHandleCancelUpload_InvalidJSON(t *testing.T) {
	_, rpcErr := handleCancelUpload("test-dongle", json.RawMessage(`{not valid`))
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}
