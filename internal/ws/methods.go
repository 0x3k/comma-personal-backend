package ws

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// rpcCallTimeout is the default timeout for waiting on an RPC response.
const rpcCallTimeout = 30 * time.Second

// pendingCall tracks an in-flight RPC request waiting for a response.
type pendingCall struct {
	ch chan *RPCResponse
}

// RPCCaller manages outgoing JSON-RPC requests to a device and correlates
// responses back to callers. It must be installed as a handler on the Client
// to intercept RPC responses from the device.
type RPCCaller struct {
	mu      sync.Mutex
	pending map[string]*pendingCall
	nextID  atomic.Int64
}

// NewRPCCaller creates a new RPCCaller.
func NewRPCCaller() *RPCCaller {
	return &RPCCaller{
		pending: make(map[string]*pendingCall),
	}
}

// nextRequestID generates a unique numeric ID for an RPC request.
func (rc *RPCCaller) nextRequestID() json.RawMessage {
	id := rc.nextID.Add(1)
	return json.RawMessage(fmt.Sprintf("%d", id))
}

// Call sends a JSON-RPC request to the client and waits for the response.
// It returns the RPCResponse or an error if the call times out or the
// request cannot be sent.
func (rc *RPCCaller) Call(c *Client, method string, params interface{}) (*RPCResponse, error) {
	return rc.CallWithTimeout(c, method, params, rpcCallTimeout)
}

// CallWithTimeout sends a JSON-RPC request and waits up to the given duration.
func (rc *RPCCaller) CallWithTimeout(c *Client, method string, params interface{}, timeout time.Duration) (*RPCResponse, error) {
	id := rc.nextRequestID()
	idStr := string(id)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		rawParams = b
	}

	pc := &pendingCall{
		ch: make(chan *RPCResponse, 1),
	}

	rc.mu.Lock()
	rc.pending[idStr] = pc
	rc.mu.Unlock()

	defer func() {
		rc.mu.Lock()
		delete(rc.pending, idStr)
		rc.mu.Unlock()
	}()

	req := &RPCRequest{
		JSONRPC: jsonRPCVersion,
		Method:  method,
		Params:  rawParams,
		ID:      id,
	}

	if err := c.SendRPCRequest(req); err != nil {
		return nil, fmt.Errorf("failed to send RPC request: %w", err)
	}

	select {
	case resp := <-pc.ch:
		return resp, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("rpc call %q (id=%s) timed out after %s", method, idStr, timeout)
	case <-c.done:
		return nil, fmt.Errorf("client disconnected during rpc call %q", method)
	}
}

// HandleResponse should be called when a JSON-RPC response is received from
// the device. It matches the response to a pending call and delivers it.
// Returns true if the response was matched to a pending call.
func (rc *RPCCaller) HandleResponse(resp *RPCResponse) bool {
	if resp.ID == nil {
		return false
	}
	idStr := string(resp.ID)

	rc.mu.Lock()
	pc, ok := rc.pending[idStr]
	rc.mu.Unlock()

	if !ok {
		return false
	}

	// Non-blocking send; the channel is buffered with size 1.
	select {
	case pc.ch <- resp:
	default:
	}
	return true
}

// UploadFileToUrlParams are the parameters for the uploadFileToUrl RPC method.
type UploadFileToUrlParams struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Path    string            `json:"fn"`
}

// CallUploadFileToUrl instructs the device to upload a file to the given URL.
func CallUploadFileToUrl(caller *RPCCaller, client *Client, url string, headers map[string]string, path string) error {
	params := UploadFileToUrlParams{
		URL:     url,
		Headers: headers,
		Path:    path,
	}

	resp, err := caller.Call(client, "uploadFileToUrl", params)
	if err != nil {
		return fmt.Errorf("uploadFileToUrl failed: %w", err)
	}

	if resp.Error != nil {
		return fmt.Errorf("uploadFileToUrl returned error: %w", resp.Error)
	}

	return nil
}

// CallGetNetworkType asks the device for its current network type.
func CallGetNetworkType(caller *RPCCaller, client *Client) (interface{}, error) {
	resp, err := caller.Call(client, "getNetworkType", nil)
	if err != nil {
		return nil, fmt.Errorf("getNetworkType failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("getNetworkType returned error: %w", resp.Error)
	}

	return resp.Result, nil
}

// CallGetSimInfo asks the device for its SIM card information.
func CallGetSimInfo(caller *RPCCaller, client *Client) (interface{}, error) {
	resp, err := caller.Call(client, "getSimInfo", nil)
	if err != nil {
		return nil, fmt.Errorf("getSimInfo failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("getSimInfo returned error: %w", resp.Error)
	}

	return resp.Result, nil
}

// SetNavDestinationParams are the parameters for the setNavDestination RPC method.
type SetNavDestinationParams struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Place     string  `json:"place_name"`
}

// CallSetNavDestination instructs the device to set a navigation destination.
func CallSetNavDestination(caller *RPCCaller, client *Client, lat, lng float64, place string) error {
	params := SetNavDestinationParams{
		Latitude:  lat,
		Longitude: lng,
		Place:     place,
	}

	resp, err := caller.Call(client, "setNavDestination", params)
	if err != nil {
		return fmt.Errorf("setNavDestination failed: %w", err)
	}

	if resp.Error != nil {
		return fmt.Errorf("setNavDestination returned error: %w", resp.Error)
	}

	return nil
}

// takeSnapshotTimeout is the RPC timeout for takeSnapshot. Snapshots can be
// slow on a cold start (camerad has to hand off buffers), so a longer deadline
// than the default is used.
const takeSnapshotTimeout = 30 * time.Second

// SnapshotResult holds the payload returned by the takeSnapshot RPC. openpilot
// historically returned either a single base64 JPEG string or an object of the
// shape {jpegBack, jpegFront}; this struct captures both possibilities with
// optional fields. At least one of RawString, JpegBack, or JpegFront will be
// populated on a successful call.
type SnapshotResult struct {
	// JpegBack is the base64-encoded JPEG from the rear (road-facing) camera.
	JpegBack string `json:"jpegBack,omitempty"`
	// JpegFront is the base64-encoded JPEG from the front (driver-facing) camera.
	JpegFront string `json:"jpegFront,omitempty"`
	// RawString holds the payload when the device returns a single base64
	// JPEG string instead of an object.
	RawString string `json:"raw_string,omitempty"`
}

// snapshotObjectPayload is the object shape the device may return for
// takeSnapshot. It is decoded with DisallowUnknownFields disabled so new
// fields added by future openpilot versions do not break parsing.
type snapshotObjectPayload struct {
	JpegBack  string `json:"jpegBack"`
	JpegFront string `json:"jpegFront"`
}

// CallTakeSnapshot instructs the device to capture a still image from its
// cameras and return it as base64-encoded JPEG data. The device may respond
// with either a single base64 string or an object containing jpegBack and/or
// jpegFront fields; both shapes are parsed into SnapshotResult.
func CallTakeSnapshot(caller *RPCCaller, client *Client) (*SnapshotResult, error) {
	resp, err := caller.CallWithTimeout(client, "takeSnapshot", nil, takeSnapshotTimeout)
	if err != nil {
		return nil, fmt.Errorf("takeSnapshot failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("takeSnapshot returned error: %w", resp.Error)
	}

	return parseSnapshotResult(resp.Result)
}

// parseSnapshotResult coerces an RPC result value into a SnapshotResult.
// The result may arrive as a Go value (when a test delivers a response
// directly) or as a json.RawMessage / map (after wire decoding), so the
// function re-marshals to JSON and then inspects the first non-whitespace
// byte to decide between the string and object shapes.
func parseSnapshotResult(result interface{}) (*SnapshotResult, error) {
	if result == nil {
		return nil, fmt.Errorf("takeSnapshot returned no result")
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal snapshot result: %w", err)
	}

	// Peek past whitespace to determine the JSON shape.
	trimmed := bytesTrimLeadingWS(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("takeSnapshot returned empty result")
	}

	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("failed to parse snapshot string: %w", err)
		}
		return &SnapshotResult{RawString: s}, nil
	case '{':
		var obj snapshotObjectPayload
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("failed to parse snapshot object: %w", err)
		}
		return &SnapshotResult{JpegBack: obj.JpegBack, JpegFront: obj.JpegFront}, nil
	default:
		return nil, fmt.Errorf("takeSnapshot returned unexpected result shape: %s", string(raw))
	}
}

// bytesTrimLeadingWS returns b with any leading JSON whitespace removed.
func bytesTrimLeadingWS(b []byte) []byte {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return b[i:]
		}
	}
	return nil
}

// RegisterDefaultHandlers installs device-side method handlers on a Client
// for common methods that the device is expected to handle. These are useful
// for testing or when the backend acts as a simulated device.
func RegisterDefaultHandlers(handlers map[string]MethodHandler) {
	handlers["uploadFileToUrl"] = handleUploadFileToUrl
	handlers["getNetworkType"] = handleGetNetworkType
	handlers["getSimInfo"] = handleGetSimInfo
	handlers["setNavDestination"] = handleSetNavDestination
	handlers["takeSnapshot"] = handleTakeSnapshot
}

// stubSnapshotJPEG is a tiny hard-coded base64 JPEG payload used by the
// device-side takeSnapshot stub. This is the smallest syntactically-valid JPEG
// (SOI + EOI markers) so it is cheap to embed and easy to recognise in tests.
const stubSnapshotJPEG = "/9j/2Q=="

// handleUploadFileToUrl is a device-side stub handler for uploadFileToUrl.
// A real device would perform the upload; this handler acknowledges the request.
func handleUploadFileToUrl(_ string, params json.RawMessage) (interface{}, *RPCError) {
	var p UploadFileToUrlParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
	}

	if p.URL == "" {
		return nil, NewRPCError(CodeInvalidParams, "url is required")
	}
	if p.Path == "" {
		return nil, NewRPCError(CodeInvalidParams, "fn is required")
	}

	return map[string]int{"enqueued": 1}, nil
}

// handleGetNetworkType is a device-side stub handler for getNetworkType.
func handleGetNetworkType(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return map[string]interface{}{
		"network_type": 5, // LTE
		"wifi_ip":      "",
	}, nil
}

// handleGetSimInfo is a device-side stub handler for getSimInfo.
func handleGetSimInfo(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return map[string]interface{}{
		"sim_id": 0,
		"state":  "",
	}, nil
}

// handleSetNavDestination is a device-side stub handler for setNavDestination.
func handleSetNavDestination(_ string, params json.RawMessage) (interface{}, *RPCError) {
	var p SetNavDestinationParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
	}

	return map[string]bool{"success": true}, nil
}

// handleTakeSnapshot is a device-side stub handler for takeSnapshot. It
// returns the object shape ({jpegBack, jpegFront}) with a tiny hard-coded
// base64 JPEG in both fields so round-trip tests can exercise the full path.
func handleTakeSnapshot(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return map[string]string{
		"jpegBack":  stubSnapshotJPEG,
		"jpegFront": stubSnapshotJPEG,
	}, nil
}
