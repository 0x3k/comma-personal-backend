package ws

import (
	"encoding/json"
	"testing"
)

func TestParseRequest_Valid(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		method string
		hasID  bool
	}{
		{
			name:   "basic request",
			input:  `{"jsonrpc":"2.0","method":"echo","id":1}`,
			method: "echo",
			hasID:  true,
		},
		{
			name:   "request with params",
			input:  `{"jsonrpc":"2.0","method":"add","params":{"a":1,"b":2},"id":"abc"}`,
			method: "add",
			hasID:  true,
		},
		{
			name:   "notification (no id)",
			input:  `{"jsonrpc":"2.0","method":"notify"}`,
			method: "notify",
			hasID:  false,
		},
		{
			name:   "notification with null id",
			input:  `{"jsonrpc":"2.0","method":"notify","id":null}`,
			method: "notify",
			hasID:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ParseRequest([]byte(tt.input))
			if err != nil {
				t.Fatalf("ParseRequest returned unexpected error: %v", err)
			}
			if req.Method != tt.method {
				t.Errorf("Method = %q, want %q", req.Method, tt.method)
			}
			if req.IsNotification() == tt.hasID {
				t.Errorf("IsNotification() = %v, want %v", req.IsNotification(), !tt.hasID)
			}
		})
	}
}

func TestParseRequest_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "invalid json",
			input: `{not json}`,
		},
		{
			name:  "wrong version",
			input: `{"jsonrpc":"1.0","method":"foo","id":1}`,
		},
		{
			name:  "missing method",
			input: `{"jsonrpc":"2.0","id":1}`,
		},
		{
			name:  "empty method",
			input: `{"jsonrpc":"2.0","method":"","id":1}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(tt.input))
			if err == nil {
				t.Fatal("ParseRequest returned nil error, expected error")
			}
		})
	}
}

func TestNewRPCResponse(t *testing.T) {
	id := json.RawMessage(`1`)
	resp := NewRPCResponse(id, "hello")

	if resp.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.Error != nil {
		t.Errorf("Error should be nil, got %v", resp.Error)
	}
	if resp.Result != "hello" {
		t.Errorf("Result = %v, want %q", resp.Result, "hello")
	}
}

func TestNewRPCErrorResponse(t *testing.T) {
	id := json.RawMessage(`"abc"`)
	rpcErr := NewRPCError(CodeMethodNotFound, "method not found")
	resp := NewRPCErrorResponse(id, rpcErr)

	if resp.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.Result != nil {
		t.Errorf("Result should be nil, got %v", resp.Result)
	}
	if resp.Error == nil {
		t.Fatal("Error should not be nil")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Errorf("Error.Code = %d, want %d", resp.Error.Code, CodeMethodNotFound)
	}
}

func TestMarshalResponse_RoundTrip(t *testing.T) {
	id := json.RawMessage(`42`)
	resp := NewRPCResponse(id, map[string]string{"status": "ok"})

	data, err := MarshalResponse(resp)
	if err != nil {
		t.Fatalf("MarshalResponse returned error: %v", err)
	}

	var decoded RPCResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if decoded.JSONRPC != "2.0" {
		t.Errorf("decoded JSONRPC = %q, want %q", decoded.JSONRPC, "2.0")
	}
	if string(decoded.ID) != "42" {
		t.Errorf("decoded ID = %s, want 42", string(decoded.ID))
	}
}

func TestRPCError_Error(t *testing.T) {
	rpcErr := NewRPCError(CodeInternalError, "something broke")
	got := rpcErr.Error()
	want := "rpc error -32603: something broke"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestNewRPCErrorWithData(t *testing.T) {
	rpcErr := NewRPCErrorWithData(CodeInvalidParams, "bad params", map[string]string{"field": "name"})
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("Code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
	if rpcErr.Data == nil {
		t.Fatal("Data should not be nil")
	}
}
