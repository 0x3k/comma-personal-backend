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

// UploadItem mirrors athenad's UploadItemDict, describing a single entry in
// the device's upload queue. Field ordering matches openpilot's dataclass
// (system/athena/athenad.py) but the JSON tags are what determine wire
// compatibility with the device.
type UploadItem struct {
	ID            string            `json:"id"`
	Path          string            `json:"path"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Priority      int               `json:"priority"`
	RetryCount    int               `json:"retry_count"`
	CreatedAt     int64             `json:"created_at"`
	Current       bool              `json:"current"`
	Progress      float64           `json:"progress"`
	AllowCellular bool              `json:"allow_cellular"`
}

// CallListUploadQueue asks the device for the current contents of its upload
// queue. Returns the decoded list of UploadItem entries.
func CallListUploadQueue(caller *RPCCaller, client *Client) ([]UploadItem, error) {
	resp, err := caller.Call(client, "listUploadQueue", nil)
	if err != nil {
		return nil, fmt.Errorf("listUploadQueue failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("listUploadQueue returned error: %w", resp.Error)
	}

	// The result is a JSON array of upload items. Re-marshal the decoded
	// interface{} so we can decode it into our typed slice.
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to re-marshal listUploadQueue result: %w", err)
	}

	var items []UploadItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("failed to unmarshal listUploadQueue result: %w", err)
	}

	return items, nil
}

// CallCancelUpload asks the device to remove the given upload IDs from its
// queue. athenad's cancelUpload accepts either a single string or a list of
// strings, so when exactly one ID is provided it is sent as a bare string;
// otherwise the full list is sent.
func CallCancelUpload(caller *RPCCaller, client *Client, ids []string) (map[string]interface{}, error) {
	var params interface{}
	if len(ids) == 1 {
		params = ids[0]
	} else {
		params = ids
	}

	resp, err := caller.Call(client, "cancelUpload", params)
	if err != nil {
		return nil, fmt.Errorf("cancelUpload failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("cancelUpload returned error: %w", resp.Error)
	}

	// Re-marshal so the caller gets a consistently-typed map regardless of
	// how the JSON decoder presented resp.Result.
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to re-marshal cancelUpload result: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cancelUpload result: %w", err)
	}

	return result, nil
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
	handlers["getNetworkType"] = handleGetNetworkType
	handlers["getSimInfo"] = handleGetSimInfo
	handlers["setNavDestination"] = handleSetNavDestination
	handlers["listUploadQueue"] = handleListUploadQueue
	handlers["cancelUpload"] = handleCancelUpload
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

// handleListUploadQueue is a device-side stub handler for listUploadQueue.
// A real device would return its actual queue; this stub returns an empty list.
func handleListUploadQueue(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return []UploadItem{}, nil
}

// handleCancelUpload is a device-side stub handler for cancelUpload. athenad
// accepts either a single upload_id string or a list of strings, so the stub
// tolerates either shape and returns a success acknowledgement.
func handleCancelUpload(_ string, params json.RawMessage) (interface{}, *RPCError) {
	// Try list first; on failure, fall back to a single string.
	var ids []string
	if err := json.Unmarshal(params, &ids); err != nil {
		var single string
		if err := json.Unmarshal(params, &single); err != nil {
			return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
		}
	}

	return map[string]int{"success": 1}, nil
}
