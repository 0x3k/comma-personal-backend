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

func TestCallGetNetworkMetered(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, true, nil)

	metered, err := CallGetNetworkMetered(caller, client)
	if err != nil {
		t.Fatalf("CallGetNetworkMetered returned error: %v", err)
	}
	if !metered {
		t.Errorf("metered = %v, want true", metered)
	}
}

func TestCallGetNetworkMetered_False(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, false, nil)

	metered, err := CallGetNetworkMetered(caller, client)
	if err != nil {
		t.Fatalf("CallGetNetworkMetered returned error: %v", err)
	}
	if metered {
		t.Errorf("metered = %v, want false", metered)
	}
}

func TestCallGetNetworkMetered_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "metered error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetNetworkMetered(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetNetworkMetered")
	}
}

func TestCallGetNetworkMetered_WrongType(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, "not-a-bool", nil)

	_, err := CallGetNetworkMetered(caller, client)
	if err == nil {
		t.Fatal("expected error for non-bool result")
	}
}

func TestCallGetNetworks(t *testing.T) {
	caller := NewRPCCaller()
	networksResult := []map[string]interface{}{
		{"SSID": "HomeNet", "strength": float64(80)},
		{"SSID": "Guest", "strength": float64(40)},
	}
	client := testClientWithResponder(t, caller, networksResult, nil)

	networks, err := CallGetNetworks(caller, client)
	if err != nil {
		t.Fatalf("CallGetNetworks returned error: %v", err)
	}
	if len(networks) != 2 {
		t.Fatalf("networks length = %d, want 2", len(networks))
	}
	if networks[0]["SSID"] != "HomeNet" {
		t.Errorf("networks[0].SSID = %v, want HomeNet", networks[0]["SSID"])
	}
	if networks[1]["strength"] != float64(40) {
		t.Errorf("networks[1].strength = %v, want 40", networks[1]["strength"])
	}
}

func TestCallGetNetworks_SliceOfInterface(t *testing.T) {
	// Simulate the production path where json.Unmarshal decodes arrays of
	// objects as []interface{} of map[string]interface{}.
	caller := NewRPCCaller()
	networksResult := []interface{}{
		map[string]interface{}{"SSID": "HomeNet", "strength": float64(80)},
	}
	client := testClientWithResponder(t, caller, networksResult, nil)

	networks, err := CallGetNetworks(caller, client)
	if err != nil {
		t.Fatalf("CallGetNetworks returned error: %v", err)
	}
	if len(networks) != 1 {
		t.Fatalf("networks length = %d, want 1", len(networks))
	}
	if networks[0]["SSID"] != "HomeNet" {
		t.Errorf("networks[0].SSID = %v, want HomeNet", networks[0]["SSID"])
	}
}

func TestCallGetNetworks_Empty(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, []map[string]interface{}{}, nil)

	networks, err := CallGetNetworks(caller, client)
	if err != nil {
		t.Fatalf("CallGetNetworks returned error: %v", err)
	}
	if len(networks) != 0 {
		t.Errorf("networks length = %d, want 0", len(networks))
	}
}

func TestCallGetNetworks_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "networks error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetNetworks(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetNetworks")
	}
}

func TestCallGetNetworks_WrongType(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, "not-a-list", nil)

	_, err := CallGetNetworks(caller, client)
	if err == nil {
		t.Fatal("expected error for non-list result")
	}
}

func TestCallGetNetworks_BadEntry(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, []interface{}{"not-an-object"}, nil)

	_, err := CallGetNetworks(caller, client)
	if err == nil {
		t.Fatal("expected error for non-object entry")
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

func TestCallUploadFilesToUrls_Empty(t *testing.T) {
	caller := NewRPCCaller()
	deviceResult := map[string]interface{}{
		"enqueued": float64(0),
		"items":    []interface{}{},
		"failed":   []interface{}{},
	}
	client := testClientWithResponder(t, caller, deviceResult, nil)

	result, err := CallUploadFilesToUrls(caller, client, []UploadFileToUrlParams{})
	if err != nil {
		t.Fatalf("CallUploadFilesToUrls returned error: %v", err)
	}
	if result["enqueued"] != float64(0) {
		t.Errorf("enqueued = %v, want 0", result["enqueued"])
	}
}

func TestCallUploadFilesToUrls_SingleItem(t *testing.T) {
	caller := NewRPCCaller()
	deviceResult := map[string]interface{}{
		"enqueued": float64(1),
		"items": []interface{}{
			map[string]interface{}{"fn": "/data/rlog", "url": "https://example.com/upload"},
		},
		"failed": []interface{}{},
	}
	client := testClientWithResponder(t, caller, deviceResult, nil)

	items := []UploadFileToUrlParams{
		{URL: "https://example.com/upload", Headers: map[string]string{"Authorization": "Bearer tok"}, Path: "/data/rlog"},
	}
	result, err := CallUploadFilesToUrls(caller, client, items)
	if err != nil {
		t.Fatalf("CallUploadFilesToUrls returned error: %v", err)
	}
	if result["enqueued"] != float64(1) {
		t.Errorf("enqueued = %v, want 1", result["enqueued"])
	}
}

func TestCallUploadFilesToUrls_ThreeItems(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "batch-check",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	// Responder that verifies the outgoing params shape and dispatches through
	// the shared device-side handler.
	go func() {
		msg := <-c.sendCh
		var req RPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		var p UploadFilesToUrlsParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		if len(p) != 3 || p[0].Path != "/data/a" || p[2].URL != "https://example.com/c" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected params")
			caller.HandleResponse(resp)
			return
		}
		result, rpcErr := handleUploadFilesToUrls("batch-check", req.Params)
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			// Round-trip through JSON so the client sees the same decoded
			// types (float64, []interface{}) it would from a real device.
			b, _ := json.Marshal(result)
			var decoded map[string]interface{}
			_ = json.Unmarshal(b, &decoded)
			resp.Result = decoded
		}
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })

	items := []UploadFileToUrlParams{
		{URL: "https://example.com/a", Headers: map[string]string{"X-A": "1"}, Path: "/data/a"},
		{URL: "https://example.com/b", Headers: map[string]string{"X-B": "2"}, Path: "/data/b"},
		{URL: "https://example.com/c", Headers: map[string]string{"X-C": "3"}, Path: "/data/c"},
	}
	result, err := CallUploadFilesToUrls(caller, c, items)
	if err != nil {
		t.Fatalf("CallUploadFilesToUrls returned error: %v", err)
	}
	if result["enqueued"] != float64(3) {
		t.Errorf("enqueued = %v, want 3", result["enqueued"])
	}
	echoed, ok := result["items"].([]interface{})
	if !ok {
		t.Fatalf("items is not []interface{}, got %T", result["items"])
	}
	if len(echoed) != 3 {
		t.Errorf("echoed items = %d, want 3", len(echoed))
	}
}

func TestCallUploadFilesToUrls_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "batch upload failed")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallUploadFilesToUrls(caller, client, []UploadFileToUrlParams{
		{URL: "https://example.com/upload", Path: "/data/rlog"},
	})
	if err == nil {
		t.Fatal("expected error from CallUploadFilesToUrls")
	}
}

func TestHandleUploadFilesToUrls_Valid(t *testing.T) {
	params := json.RawMessage(`[{"url":"https://example.com/a","headers":{"X-A":"1"},"fn":"/data/a"},{"url":"https://example.com/b","headers":{"X-B":"2"},"fn":"/data/b"}]`)

	result, rpcErr := handleUploadFilesToUrls("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map[string]interface{}, got %T", result)
	}
	if m["enqueued"] != 2 {
		t.Errorf("enqueued = %v, want 2", m["enqueued"])
	}
	echoed, ok := m["items"].([]map[string]interface{})
	if !ok {
		t.Fatalf("items is not []map[string]interface{}, got %T", m["items"])
	}
	if len(echoed) != 2 {
		t.Errorf("echoed items = %d, want 2", len(echoed))
	}
	if echoed[0]["fn"] != "/data/a" {
		t.Errorf("echoed[0].fn = %v, want /data/a", echoed[0]["fn"])
	}
}

func TestHandleUploadFilesToUrls_Empty(t *testing.T) {
	params := json.RawMessage(`[]`)

	result, rpcErr := handleUploadFilesToUrls("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map[string]interface{}, got %T", result)
	}
	if m["enqueued"] != 0 {
		t.Errorf("enqueued = %v, want 0", m["enqueued"])
	}
}

func TestHandleUploadFilesToUrls_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`not json`)

	_, rpcErr := handleUploadFilesToUrls("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
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

func TestHandleGetNetworkMetered(t *testing.T) {
	result, rpcErr := handleGetNetworkMetered("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	b, ok := result.(bool)
	if !ok {
		t.Fatalf("result is not bool, got %T", result)
	}
	if b {
		t.Error("expected metered=false stub value")
	}
}

func TestHandleGetNetworks(t *testing.T) {
	result, rpcErr := handleGetNetworks("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	networks, ok := result.([]map[string]interface{})
	if !ok {
		t.Fatalf("result is not []map[string]interface{}, got %T", result)
	}
	if len(networks) != 0 {
		t.Errorf("expected empty stub list, got %d entries", len(networks))
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
		"uploadFilesToUrls",
		"getNetworkType",
		"getNetworkMetered",
		"getNetworks",
		"getSimInfo",
		"setNavDestination",
		"takeSnapshot",
	}

	for _, method := range expected {
		if _, ok := handlers[method]; !ok {
			t.Errorf("handler not registered for method %q", method)
		}
	}
}

func TestCallTakeSnapshot_StringResponse(t *testing.T) {
	caller := NewRPCCaller()
	// Device returns the older single-string shape.
	client := testClientWithResponder(t, caller, "AAAA", nil)

	result, err := CallTakeSnapshot(caller, client)
	if err != nil {
		t.Fatalf("CallTakeSnapshot returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.RawString != "AAAA" {
		t.Errorf("RawString = %q, want %q", result.RawString, "AAAA")
	}
	if result.JpegBack != "" || result.JpegFront != "" {
		t.Errorf("expected jpeg fields empty for string response, got back=%q front=%q", result.JpegBack, result.JpegFront)
	}
}

func TestCallTakeSnapshot_ObjectResponse(t *testing.T) {
	caller := NewRPCCaller()
	obj := map[string]string{
		"jpegBack":  "BACKDATA",
		"jpegFront": "FRONTDATA",
	}
	client := testClientWithResponder(t, caller, obj, nil)

	result, err := CallTakeSnapshot(caller, client)
	if err != nil {
		t.Fatalf("CallTakeSnapshot returned error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.JpegBack != "BACKDATA" {
		t.Errorf("JpegBack = %q, want %q", result.JpegBack, "BACKDATA")
	}
	if result.JpegFront != "FRONTDATA" {
		t.Errorf("JpegFront = %q, want %q", result.JpegFront, "FRONTDATA")
	}
	if result.RawString != "" {
		t.Errorf("RawString = %q, want empty", result.RawString)
	}
}

func TestCallTakeSnapshot_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "not available while camerad is started")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallTakeSnapshot(caller, client)
	if err == nil {
		t.Fatal("expected error from CallTakeSnapshot")
	}
}

func TestCallTakeSnapshot_NilResult(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, nil, nil)

	_, err := CallTakeSnapshot(caller, client)
	if err == nil {
		t.Fatal("expected error for nil result")
	}
}

func TestHandleTakeSnapshot_Stub(t *testing.T) {
	result, rpcErr := handleTakeSnapshot("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]string)
	if !ok {
		t.Fatalf("result is not map[string]string, got %T", result)
	}
	if m["jpegBack"] == "" {
		t.Error("expected non-empty jpegBack")
	}
	if m["jpegFront"] == "" {
		t.Error("expected non-empty jpegFront")
	}
}

func TestParseSnapshotResult_StringFromRawJSON(t *testing.T) {
	// Simulate a wire-decoded string value.
	r, err := parseSnapshotResult("aGVsbG8=")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.RawString != "aGVsbG8=" {
		t.Errorf("RawString = %q, want %q", r.RawString, "aGVsbG8=")
	}
}

func TestParseSnapshotResult_ObjectFromMap(t *testing.T) {
	// Simulate a wire-decoded map[string]interface{} value, which is what
	// json.Unmarshal produces for a JSON object into interface{}.
	r, err := parseSnapshotResult(map[string]interface{}{
		"jpegBack":  "B",
		"jpegFront": "F",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.JpegBack != "B" || r.JpegFront != "F" {
		t.Errorf("unexpected fields: back=%q front=%q", r.JpegBack, r.JpegFront)
	}
}
