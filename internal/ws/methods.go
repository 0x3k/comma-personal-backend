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

// CallGetVersion asks the device for its openpilot version metadata. It
// returns a map with the fields openpilot_version, openpilot_agnos_version,
// openpilot_git_commit, and openpilot_git_branch.
func CallGetVersion(caller *RPCCaller, client *Client) (map[string]string, error) {
	resp, err := caller.Call(client, "getVersion", nil)
	if err != nil {
		return nil, fmt.Errorf("getVersion failed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("getVersion returned error: %w", resp.Error)
	}

	return decodeStringMap(resp.Result, "getVersion")
}

// CallGetPublicKey asks the device for its SSH public key.
func CallGetPublicKey(caller *RPCCaller, client *Client) (string, error) {
	resp, err := caller.Call(client, "getPublicKey", nil)
	if err != nil {
		return "", fmt.Errorf("getPublicKey failed: %w", err)
	}

	if resp.Error != nil {
		return "", fmt.Errorf("getPublicKey returned error: %w", resp.Error)
	}

	return decodeString(resp.Result, "getPublicKey")
}

// CallGetSshAuthorizedKeys asks the device for the contents of its
// authorized_keys file (the SSH keys imported from GitHub).
func CallGetSshAuthorizedKeys(caller *RPCCaller, client *Client) (string, error) {
	resp, err := caller.Call(client, "getSshAuthorizedKeys", nil)
	if err != nil {
		return "", fmt.Errorf("getSshAuthorizedKeys failed: %w", err)
	}

	if resp.Error != nil {
		return "", fmt.Errorf("getSshAuthorizedKeys returned error: %w", resp.Error)
	}

	return decodeString(resp.Result, "getSshAuthorizedKeys")
}

// CallGetGithubUsername asks the device for the GitHub username configured
// for SSH key import.
func CallGetGithubUsername(caller *RPCCaller, client *Client) (string, error) {
	resp, err := caller.Call(client, "getGithubUsername", nil)
	if err != nil {
		return "", fmt.Errorf("getGithubUsername failed: %w", err)
	}

	if resp.Error != nil {
		return "", fmt.Errorf("getGithubUsername returned error: %w", resp.Error)
	}

	return decodeString(resp.Result, "getGithubUsername")
}

// decodeString converts an arbitrary JSON-RPC result into a string. A nil
// result is treated as the empty string to match openpilot's behavior (the
// device returns None when the underlying Param is unset).
func decodeString(result interface{}, method string) (string, error) {
	if result == nil {
		return "", nil
	}
	s, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("%s returned non-string result: %T", method, result)
	}
	return s, nil
}

// decodeStringMap converts a JSON-RPC result into a map[string]string. The
// raw result may be a map[string]interface{} (from a round-tripped JSON
// decode) or the typed map[string]string returned by local stub handlers.
func decodeStringMap(result interface{}, method string) (map[string]string, error) {
	if result == nil {
		return nil, fmt.Errorf("%s returned nil result", method)
	}
	switch v := result.(type) {
	case map[string]string:
		out := make(map[string]string, len(v))
		for k, val := range v {
			out[k] = val
		}
		return out, nil
	case map[string]interface{}:
		out := make(map[string]string, len(v))
		for k, raw := range v {
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s result field %q is not a string: %T", method, k, raw)
			}
			out[k] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s returned unexpected result type: %T", method, result)
	}
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
	handlers["getVersion"] = handleGetVersion
	handlers["getPublicKey"] = handleGetPublicKey
	handlers["getSshAuthorizedKeys"] = handleGetSshAuthorizedKeys
	handlers["getGithubUsername"] = handleGetGithubUsername
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

// handleGetVersion is a device-side stub handler for getVersion. Real
// devices return the fields openpilot_version, openpilot_agnos_version,
// openpilot_git_commit, and openpilot_git_branch; the stub returns the
// same shape with empty values so callers can exercise round-trip parsing.
func handleGetVersion(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return map[string]string{
		"openpilot_version":       "",
		"openpilot_agnos_version": "",
		"openpilot_git_commit":    "",
		"openpilot_git_branch":    "",
	}, nil
}

// handleGetPublicKey is a device-side stub handler for getPublicKey.
func handleGetPublicKey(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return "", nil
}

// handleGetSshAuthorizedKeys is a device-side stub handler for
// getSshAuthorizedKeys.
func handleGetSshAuthorizedKeys(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return "", nil
}

// handleGetGithubUsername is a device-side stub handler for getGithubUsername.
func handleGetGithubUsername(_ string, _ json.RawMessage) (interface{}, *RPCError) {
	return "", nil
}
