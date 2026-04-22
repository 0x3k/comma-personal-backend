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
}

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
