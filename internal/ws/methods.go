package ws

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"comma-personal-backend/internal/metrics"
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
	metrics *metrics.Metrics
}

// NewRPCCaller creates a new RPCCaller with no metrics. Use
// NewRPCCallerWithMetrics to inject a *metrics.Metrics so each Call is
// observed.
func NewRPCCaller() *RPCCaller {
	return NewRPCCallerWithMetrics(nil)
}

// NewRPCCallerWithMetrics creates a new RPCCaller that records call latency
// and outcome to the given metrics instance. A nil m is treated as a no-op.
func NewRPCCallerWithMetrics(m *metrics.Metrics) *RPCCaller {
	return &RPCCaller{
		pending: make(map[string]*pendingCall),
		metrics: m,
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
	resp, err := rc.callWithTimeout(c, method, params, timeout)
	return resp, err
}

// callWithTimeout wraps the full send/wait cycle so we can observe the
// latency and outcome in a single place.
func (rc *RPCCaller) callWithTimeout(c *Client, method string, params interface{}, timeout time.Duration) (*RPCResponse, error) {
	start := time.Now()
	resp, err := rc.doCall(c, method, params, timeout)
	dur := time.Since(start)

	status := "success"
	switch {
	case err != nil && strings.Contains(err.Error(), "timed out"):
		status = "timeout"
	case err != nil:
		status = "error"
	case resp != nil && resp.Error != nil:
		status = "error"
	}
	rc.metrics.ObserveRPCCall(method, status, dur)
	return resp, err
}

// doCall is the original send-and-wait implementation.
func (rc *RPCCaller) doCall(c *Client, method string, params interface{}, timeout time.Duration) (*RPCResponse, error) {
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

// UploadFilesToUrlsParams is the batch variant of UploadFileToUrlParams; each
// entry specifies a file (fn), destination URL, and headers. This mirrors
// openpilot's athenad uploadFilesToUrls RPC.
type UploadFilesToUrlsParams []UploadFileToUrlParams

// CallUploadFilesToUrls instructs the device to enqueue multiple file uploads
// in a single RPC round-trip. It returns the device's response map, which
// typically contains "enqueued" (count), "items" (enqueued item descriptors),
// and optionally "failed" (list of file names that were rejected).
func CallUploadFilesToUrls(caller *RPCCaller, client *Client, items []UploadFileToUrlParams) (map[string]interface{}, error) {
	params := UploadFilesToUrlsParams(items)

	resp, err := caller.Call(client, "uploadFilesToUrls", params)
	if err != nil {
		return nil, fmt.Errorf("uploadFilesToUrls failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("uploadFilesToUrls returned error: %w", resp.Error)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("uploadFilesToUrls: unexpected result type %T", resp.Result)
	}
	return result, nil
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

// CallGetNetworkMetered asks the device whether its current connection is metered.
func CallGetNetworkMetered(caller *RPCCaller, client *Client) (bool, error) {
	resp, err := caller.Call(client, "getNetworkMetered", nil)
	if err != nil {
		return false, fmt.Errorf("getNetworkMetered failed: %w", err)
	}

	if resp.Error != nil {
		return false, fmt.Errorf("getNetworkMetered returned error: %w", resp.Error)
	}

	metered, ok := resp.Result.(bool)
	if !ok {
		return false, fmt.Errorf("getNetworkMetered: expected bool result, got %T", resp.Result)
	}

	return metered, nil
}

// CallGetNetworks asks the device for the list of available networks.
// Each entry is a dict describing a network (SSID, signal strength, etc.).
func CallGetNetworks(caller *RPCCaller, client *Client) ([]map[string]interface{}, error) {
	resp, err := caller.Call(client, "getNetworks", nil)
	if err != nil {
		return nil, fmt.Errorf("getNetworks failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("getNetworks returned error: %w", resp.Error)
	}

	if resp.Result == nil {
		return nil, nil
	}

	switch v := resp.Result.(type) {
	case []map[string]interface{}:
		return v, nil
	case []interface{}:
		networks := make([]map[string]interface{}, 0, len(v))
		for i, item := range v {
			m, ok := item.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("getNetworks: entry %d is not an object, got %T", i, item)
			}
			networks = append(networks, m)
		}
		return networks, nil
	default:
		return nil, fmt.Errorf("getNetworks: expected list result, got %T", resp.Result)
	}
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

// defaultGetMessageTimeoutMs is athenad's default timeout for getMessage (ms).
const defaultGetMessageTimeoutMs = 1000

// getMessageCallBufferMs is the extra buffer we add to the Go-side context
// timeout so the device has a chance to return its own TimeoutError before we
// cancel the call locally.
const getMessageCallBufferMs = 500

// GetMessageParams are the parameters for the getMessage RPC method.
type GetMessageParams struct {
	Service string `json:"service"`
	Timeout int    `json:"timeout,omitempty"`
}

// CallGetMessage asks the device for the next message on the given cereal
// service. If timeoutMs is <= 0, it defaults to 1000 ms to match athenad's
// default. The Go-side RPC deadline is set a few hundred ms after the
// device-side timeout so the device can surface its own timeout error first.
func CallGetMessage(caller *RPCCaller, client *Client, service string, timeoutMs int) (map[string]interface{}, error) {
	if timeoutMs <= 0 {
		timeoutMs = defaultGetMessageTimeoutMs
	}

	params := GetMessageParams{
		Service: service,
		Timeout: timeoutMs,
	}

	callTimeout := time.Duration(timeoutMs+getMessageCallBufferMs) * time.Millisecond

	resp, err := caller.CallWithTimeout(client, "getMessage", params, callTimeout)
	if err != nil {
		return nil, fmt.Errorf("getMessage failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("getMessage returned error: %w", resp.Error)
	}

	if resp.Result == nil {
		return nil, fmt.Errorf("getMessage returned no result")
	}

	// Result is an arbitrary JSON dict. Re-encode/decode so we hand back a
	// stable map[string]interface{} regardless of how the transport decoded it.
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal getMessage result: %w", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to decode getMessage result: %w", err)
	}
	return out, nil
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

// ListDataDirectoryParams are the parameters for the listDataDirectory RPC method.
// Prefix is optional; when empty, the device lists every file in its data
// directory. This mirrors the default argument in openpilot's athenad.
type ListDataDirectoryParams struct {
	Prefix string `json:"prefix,omitempty"`
}

// CallListDataDirectory asks the device for the list of filenames under its
// data directory (typically /data/media/0/realdata). When prefix is empty, no
// params are sent so the device applies its default of listing everything.
func CallListDataDirectory(caller *RPCCaller, client *Client, prefix string) ([]string, error) {
	var params interface{}
	if prefix != "" {
		params = ListDataDirectoryParams{Prefix: prefix}
	}

	resp, err := caller.Call(client, "listDataDirectory", params)
	if err != nil {
		return nil, fmt.Errorf("listDataDirectory failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("listDataDirectory returned error: %w", resp.Error)
	}

	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("listDataDirectory: failed to marshal result: %w", err)
	}

	var files []string
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("listDataDirectory: unexpected result type: %w", err)
	}

	return files, nil
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
	handlers["uploadFilesToUrls"] = handleUploadFilesToUrls
	handlers["getNetworkType"] = handleGetNetworkType
	handlers["getNetworkMetered"] = handleGetNetworkMetered
	handlers["getNetworks"] = handleGetNetworks
	handlers["getSimInfo"] = handleGetSimInfo
	handlers["setNavDestination"] = handleSetNavDestination
	handlers["listDataDirectory"] = handleListDataDirectory
	handlers["takeSnapshot"] = handleTakeSnapshot
	handlers["getMessage"] = handleGetMessage
}

// knownStubServices is a small allowlist of cereal service names the device
// stub knows how to fake. A real device uses the full cereal SERVICE_LIST.
var knownStubServices = map[string]bool{
	"carState":            true,
	"deviceState":         true,
	"liveLocationKalman":  true,
	"controlsState":       true,
	"pandaStates":         true,
	"gpsLocationExternal": true,
}

// handleGetMessage is a device-side stub handler for getMessage. It validates
// the service name and returns a canned message dict keyed by that service.
func handleGetMessage(_ string, params json.RawMessage) (interface{}, *RPCError) {
	var p GetMessageParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
	}

	if p.Service == "" {
		return nil, NewRPCError(CodeInvalidParams, "service is required")
	}
	if !knownStubServices[p.Service] {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid service: %q", p.Service))
	}

	return map[string]interface{}{
		p.Service: map[string]interface{}{
			"stub":    true,
			"service": p.Service,
		},
	}, nil
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

// handleUploadFilesToUrls is a device-side stub handler for uploadFilesToUrls.
// A real device would enqueue each file on its uploader; this handler echoes
// the request shape and reports enqueued == len(items) with no failures.
func handleUploadFilesToUrls(_ string, params json.RawMessage) (interface{}, *RPCError) {
	var items UploadFilesToUrlsParams
	if err := json.Unmarshal(params, &items); err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
	}

	echoed := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		echoed = append(echoed, map[string]interface{}{
			"fn":      it.Path,
			"url":     it.URL,
			"headers": it.Headers,
		})
	}

	return map[string]interface{}{
		"enqueued": len(items),
		"items":    echoed,
		"failed":   []string{},
	}, nil
}

// handleGetNetworkType is a device-side stub handler for getNetworkType.
func handleGetNetworkType(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return map[string]interface{}{
		"network_type": 5, // LTE
		"wifi_ip":      "",
	}, nil
}

// handleGetNetworkMetered is a device-side stub handler for getNetworkMetered.
func handleGetNetworkMetered(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return false, nil
}

// handleGetNetworks is a device-side stub handler for getNetworks.
func handleGetNetworks(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return []map[string]interface{}{}, nil
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

// stubDataDirectoryFiles is the fixed list returned by the device-side stub.
// It mimics a handful of segment artifacts that openpilot's athenad would
// surface under /data/media/0/realdata.
var stubDataDirectoryFiles = []string{
	"2024-01-01--12-00-00--0/rlog.bz2",
	"2024-01-01--12-00-00--0/qlog.bz2",
	"2024-01-01--12-00-00--0/qcamera.ts",
	"2024-01-01--12-00-00--1/rlog.bz2",
	"boot/boot.log",
}

// handleListDataDirectory is a device-side stub handler for listDataDirectory.
// It returns the stub file list, filtered by the optional prefix. Params are
// optional; an empty or missing body is treated as no filter, matching the
// athenad default where prefix defaults to an empty string.
func handleListDataDirectory(_ string, params json.RawMessage) (interface{}, *RPCError) {
	var p ListDataDirectoryParams
	if len(params) > 0 && string(params) != "null" {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
		}
	}

	if p.Prefix == "" {
		files := make([]string, len(stubDataDirectoryFiles))
		copy(files, stubDataDirectoryFiles)
		return files, nil
	}

	filtered := make([]string, 0, len(stubDataDirectoryFiles))
	for _, f := range stubDataDirectoryFiles {
		if strings.HasPrefix(f, p.Prefix) {
			filtered = append(filtered, f)
		}
	}
	return filtered, nil
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
