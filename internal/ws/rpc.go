package ws

import (
	"encoding/json"
	"fmt"
)

const jsonRPCVersion = "2.0"

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// RPCRequest represents a JSON-RPC 2.0 request or notification.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
}

// RPCResponse represents a JSON-RPC 2.0 response.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      json.RawMessage `json:"id"`
}

// RPCError represents a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// NewRPCError creates a new RPCError with the given code and message.
func NewRPCError(code int, message string) *RPCError {
	return &RPCError{Code: code, Message: message}
}

// NewRPCErrorWithData creates a new RPCError with the given code, message, and data.
func NewRPCErrorWithData(code int, message string, data interface{}) *RPCError {
	return &RPCError{Code: code, Message: message, Data: data}
}

// NewRPCResponse creates a successful JSON-RPC 2.0 response.
func NewRPCResponse(id json.RawMessage, result interface{}) *RPCResponse {
	return &RPCResponse{
		JSONRPC: jsonRPCVersion,
		Result:  result,
		ID:      id,
	}
}

// NewRPCErrorResponse creates an error JSON-RPC 2.0 response.
func NewRPCErrorResponse(id json.RawMessage, rpcErr *RPCError) *RPCResponse {
	return &RPCResponse{
		JSONRPC: jsonRPCVersion,
		Error:   rpcErr,
		ID:      id,
	}
}

// ParseRequest parses raw bytes into an RPCRequest and validates the envelope.
func ParseRequest(data []byte) (*RPCRequest, error) {
	var req RPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("failed to parse JSON-RPC request: %w", err)
	}

	if req.JSONRPC != jsonRPCVersion {
		return nil, fmt.Errorf("failed to validate JSON-RPC version: expected %q, got %q", jsonRPCVersion, req.JSONRPC)
	}

	if req.Method == "" {
		return nil, fmt.Errorf("failed to validate JSON-RPC request: method is required")
	}

	return &req, nil
}

// IsNotification returns true if the request has no ID (JSON-RPC notification).
func (r *RPCRequest) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// MarshalResponse serializes an RPCResponse to JSON bytes.
func MarshalResponse(resp *RPCResponse) ([]byte, error) {
	data, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal JSON-RPC response: %w", err)
	}
	return data, nil
}
