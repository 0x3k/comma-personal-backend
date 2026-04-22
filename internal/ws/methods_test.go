package ws

import (
	"encoding/json"
	"strings"
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
		"listDataDirectory",
		"takeSnapshot",
		"getMessage",
		"listUploadQueue",
		"cancelUpload",
		"setRouteViewed",
		"getVersion",
		"getPublicKey",
		"getSshAuthorizedKeys",
		"getGithubUsername",
	}

	for _, method := range expected {
		if _, ok := handlers[method]; !ok {
			t.Errorf("handler not registered for method %q", method)
		}
	}
}

func TestCallListDataDirectory_NoPrefix(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "list-no-prefix",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	expected := []string{"a/rlog.bz2", "b/qlog.bz2"}

	go func() {
		msg := <-c.sendCh
		var req RPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		// When prefix is empty, the client should omit params entirely or send
		// an empty/null value. Either is acceptable; reject any non-empty prefix.
		if len(req.Params) > 0 && string(req.Params) != "null" {
			var p ListDataDirectoryParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp.Error = NewRPCError(CodeInvalidParams, "bad params")
				caller.HandleResponse(resp)
				return
			}
			if p.Prefix != "" {
				resp.Error = NewRPCError(CodeInvalidParams, "unexpected non-empty prefix")
				caller.HandleResponse(resp)
				return
			}
		}
		resp.Result = expected
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })

	files, err := CallListDataDirectory(caller, c, "")
	if err != nil {
		t.Fatalf("CallListDataDirectory returned error: %v", err)
	}
	if len(files) != len(expected) {
		t.Fatalf("files length = %d, want %d", len(files), len(expected))
	}
	for i, want := range expected {
		if files[i] != want {
			t.Errorf("files[%d] = %q, want %q", i, files[i], want)
		}
	}
}

func TestCallListDataDirectory_WithPrefix(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "list-with-prefix",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	expected := []string{"2024-01-01--12-00-00--0/rlog.bz2"}

	go func() {
		msg := <-c.sendCh
		var req RPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		var p ListDataDirectoryParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = NewRPCError(CodeInvalidParams, "bad params")
			caller.HandleResponse(resp)
			return
		}
		if p.Prefix != "2024-01-01" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected prefix")
			caller.HandleResponse(resp)
			return
		}
		resp.Result = expected
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })

	files, err := CallListDataDirectory(caller, c, "2024-01-01")
	if err != nil {
		t.Fatalf("CallListDataDirectory returned error: %v", err)
	}
	if len(files) != len(expected) {
		t.Fatalf("files length = %d, want %d", len(files), len(expected))
	}
	if files[0] != expected[0] {
		t.Errorf("files[0] = %q, want %q", files[0], expected[0])
	}
}

func TestCallListDataDirectory_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "list failed")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallListDataDirectory(caller, client, "")
	if err == nil {
		t.Fatal("expected error from CallListDataDirectory")
	}
}

func TestHandleListDataDirectory_NoPrefix(t *testing.T) {
	result, rpcErr := handleListDataDirectory("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	files, ok := result.([]string)
	if !ok {
		t.Fatalf("result is not []string, got %T", result)
	}
	if len(files) != len(stubDataDirectoryFiles) {
		t.Errorf("files length = %d, want %d", len(files), len(stubDataDirectoryFiles))
	}
}

func TestHandleListDataDirectory_EmptyPrefixParam(t *testing.T) {
	params := json.RawMessage(`{"prefix":""}`)
	result, rpcErr := handleListDataDirectory("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	files, ok := result.([]string)
	if !ok {
		t.Fatalf("result is not []string, got %T", result)
	}
	if len(files) != len(stubDataDirectoryFiles) {
		t.Errorf("files length = %d, want %d (full list)", len(files), len(stubDataDirectoryFiles))
	}
}

func TestHandleListDataDirectory_WithPrefix(t *testing.T) {
	params := json.RawMessage(`{"prefix":"2024-01-01--12-00-00--0"}`)
	result, rpcErr := handleListDataDirectory("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	files, ok := result.([]string)
	if !ok {
		t.Fatalf("result is not []string, got %T", result)
	}
	if len(files) == 0 {
		t.Fatal("expected at least one file matching prefix")
	}
	for _, f := range files {
		if !strings.HasPrefix(f, "2024-01-01--12-00-00--0") {
			t.Errorf("file %q does not match prefix", f)
		}
	}
}

func TestHandleListDataDirectory_PrefixNoMatch(t *testing.T) {
	params := json.RawMessage(`{"prefix":"no-such-prefix"}`)
	result, rpcErr := handleListDataDirectory("test-dongle", params)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	files, ok := result.([]string)
	if !ok {
		t.Fatalf("result is not []string, got %T", result)
	}
	if len(files) != 0 {
		t.Errorf("expected empty list for non-matching prefix, got %d files", len(files))
	}
}

func TestHandleListDataDirectory_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`not json`)

	_, rpcErr := handleListDataDirectory("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
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
func TestCallSetRouteViewed(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, map[string]bool{"success": true}, nil)

	err := CallSetRouteViewed(caller, client, "a2a0ccea32023010|2023-07-13--11-06-38")
	if err != nil {
		t.Fatalf("CallSetRouteViewed returned error: %v", err)
	}
}

func TestCallSetRouteViewed_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInvalidParams, "route is required")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	err := CallSetRouteViewed(caller, client, "")
	if err == nil {
		t.Fatal("expected error from CallSetRouteViewed")
	}
}

func TestCallSetRouteViewed_ParamsMarshaled(t *testing.T) {
	caller := NewRPCCaller()
	hub := NewHub()
	c := &Client{
		DongleID: "route-viewed-params",
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
		var p SetRouteViewedParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return
		}
		resp := &RPCResponse{JSONRPC: jsonRPCVersion, ID: req.ID}
		if p.Route != "a2a0ccea32023010|2023-07-13--11-06-38" {
			resp.Error = NewRPCError(CodeInvalidParams, "unexpected params")
		} else {
			resp.Result = map[string]bool{"success": true}
		}
		caller.HandleResponse(resp)
	}()

	t.Cleanup(func() { c.Close() })

	err := CallSetRouteViewed(caller, c, "a2a0ccea32023010|2023-07-13--11-06-38")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHandleSetRouteViewed_Valid(t *testing.T) {
	params := json.RawMessage(`{"route":"a2a0ccea32023010|2023-07-13--11-06-38"}`)

	result, rpcErr := handleSetRouteViewed("test-dongle", params)
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

func TestHandleSetRouteViewed_EmptyRoute(t *testing.T) {
	params := json.RawMessage(`{"route":""}`)

	_, rpcErr := handleSetRouteViewed("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for empty route")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleSetRouteViewed_MissingRoute(t *testing.T) {
	params := json.RawMessage(`{}`)

	_, rpcErr := handleSetRouteViewed("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for missing route")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestHandleSetRouteViewed_InvalidJSON(t *testing.T) {
	params := json.RawMessage(`not json`)

	_, rpcErr := handleSetRouteViewed("test-dongle", params)
	if rpcErr == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("error code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}
func TestCallGetVersion(t *testing.T) {
	caller := NewRPCCaller()
	versionResult := map[string]interface{}{
		"openpilot_version":       "0.9.8",
		"openpilot_agnos_version": "10.1",
		"openpilot_git_commit":    "abc123",
		"openpilot_git_branch":    "release3",
	}
	client := testClientWithResponder(t, caller, versionResult, nil)

	result, err := CallGetVersion(caller, client)
	if err != nil {
		t.Fatalf("CallGetVersion returned error: %v", err)
	}
	if result["openpilot_version"] != "0.9.8" {
		t.Errorf("openpilot_version = %q, want 0.9.8", result["openpilot_version"])
	}
	if result["openpilot_agnos_version"] != "10.1" {
		t.Errorf("openpilot_agnos_version = %q, want 10.1", result["openpilot_agnos_version"])
	}
	if result["openpilot_git_commit"] != "abc123" {
		t.Errorf("openpilot_git_commit = %q, want abc123", result["openpilot_git_commit"])
	}
	if result["openpilot_git_branch"] != "release3" {
		t.Errorf("openpilot_git_branch = %q, want release3", result["openpilot_git_branch"])
	}
}

func TestCallGetVersion_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "version error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetVersion(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetVersion")
	}
}

func TestCallGetVersion_NonStringField(t *testing.T) {
	caller := NewRPCCaller()
	result := map[string]interface{}{
		"openpilot_version": 123,
	}
	client := testClientWithResponder(t, caller, result, nil)

	_, err := CallGetVersion(caller, client)
	if err == nil {
		t.Fatal("expected error when a version field is non-string")
	}
}

func TestCallGetPublicKey(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, "ssh-rsa AAAAB3Nza... comma", nil)

	result, err := CallGetPublicKey(caller, client)
	if err != nil {
		t.Fatalf("CallGetPublicKey returned error: %v", err)
	}
	if result != "ssh-rsa AAAAB3Nza... comma" {
		t.Errorf("public key = %q, want ssh-rsa AAAAB3Nza... comma", result)
	}
}

func TestCallGetPublicKey_Nil(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, nil, nil)

	result, err := CallGetPublicKey(caller, client)
	if err != nil {
		t.Fatalf("CallGetPublicKey returned error: %v", err)
	}
	if result != "" {
		t.Errorf("public key = %q, want empty string for nil result", result)
	}
}

func TestCallGetPublicKey_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "no key")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetPublicKey(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetPublicKey")
	}
}

func TestCallGetPublicKey_WrongType(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, 42, nil)

	_, err := CallGetPublicKey(caller, client)
	if err == nil {
		t.Fatal("expected error when getPublicKey returns a non-string result")
	}
}

func TestCallGetSshAuthorizedKeys(t *testing.T) {
	caller := NewRPCCaller()
	keys := "ssh-rsa AAA user1\nssh-ed25519 BBB user2\n"
	client := testClientWithResponder(t, caller, keys, nil)

	result, err := CallGetSshAuthorizedKeys(caller, client)
	if err != nil {
		t.Fatalf("CallGetSshAuthorizedKeys returned error: %v", err)
	}
	if result != keys {
		t.Errorf("authorized keys = %q, want %q", result, keys)
	}
}

func TestCallGetSshAuthorizedKeys_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "keys error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetSshAuthorizedKeys(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetSshAuthorizedKeys")
	}
}

func TestCallGetGithubUsername(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, "commauser", nil)

	result, err := CallGetGithubUsername(caller, client)
	if err != nil {
		t.Fatalf("CallGetGithubUsername returned error: %v", err)
	}
	if result != "commauser" {
		t.Errorf("github username = %q, want commauser", result)
	}
}

func TestCallGetGithubUsername_Empty(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, "", nil)

	result, err := CallGetGithubUsername(caller, client)
	if err != nil {
		t.Fatalf("CallGetGithubUsername returned error: %v", err)
	}
	if result != "" {
		t.Errorf("github username = %q, want empty string", result)
	}
}

func TestCallGetGithubUsername_RPCError(t *testing.T) {
	caller := NewRPCCaller()
	rpcErr := NewRPCError(CodeInternalError, "username error")
	client := testClientWithResponder(t, caller, nil, rpcErr)

	_, err := CallGetGithubUsername(caller, client)
	if err == nil {
		t.Fatal("expected error from CallGetGithubUsername")
	}
}

func TestHandleGetVersion(t *testing.T) {
	result, rpcErr := handleGetVersion("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}

	m, ok := result.(map[string]string)
	if !ok {
		t.Fatalf("result is not map[string]string, got %T", result)
	}

	for _, field := range []string{"openpilot_version", "openpilot_agnos_version", "openpilot_git_commit", "openpilot_git_branch"} {
		if _, present := m[field]; !present {
			t.Errorf("expected field %q in result", field)
		}
	}
}

func TestHandleGetPublicKey(t *testing.T) {
	result, rpcErr := handleGetPublicKey("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	if _, ok := result.(string); !ok {
		t.Fatalf("result is not string, got %T", result)
	}
}

func TestHandleGetSshAuthorizedKeys(t *testing.T) {
	result, rpcErr := handleGetSshAuthorizedKeys("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	if _, ok := result.(string); !ok {
		t.Fatalf("result is not string, got %T", result)
	}
}

func TestHandleGetGithubUsername(t *testing.T) {
	result, rpcErr := handleGetGithubUsername("test-dongle", nil)
	if rpcErr != nil {
		t.Fatalf("unexpected error: %v", rpcErr)
	}
	if _, ok := result.(string); !ok {
		t.Fatalf("result is not string, got %T", result)
	}
}
