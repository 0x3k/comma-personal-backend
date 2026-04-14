package ws

import (
	"encoding/json"
	"sync"
)

// TestRPCRecorder collects RPC method names and raw params from a
// TestDrainResponder goroutine. All access is synchronized with a mutex.
type TestRPCRecorder struct {
	mu      sync.Mutex
	methods []string
	params  []json.RawMessage
}

// Len returns the number of recorded RPC calls.
func (r *TestRPCRecorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.methods)
}

// Method returns the method name at the given index.
func (r *TestRPCRecorder) Method(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.methods[i]
}

// Params returns the raw params at the given index.
func (r *TestRPCRecorder) Params(i int) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(json.RawMessage(nil), r.params[i]...)
}

func (r *TestRPCRecorder) append(method string, p json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods = append(r.methods, method)
	r.params = append(r.params, p)
}

// TestNewClient creates a Client with a buffered send channel and no real
// WebSocket connection. It is intended for use in tests that need to verify
// RPC calls are sent to a device. The caller must close the client when done.
func TestNewClient(dongleID string, hub *Hub) *Client {
	return &Client{
		DongleID: dongleID,
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}
}

// TestDrainResponder starts a goroutine that reads RPC requests from the
// client's send channel and sends back successful responses through the
// RPCCaller. It records each received method name and raw params in the
// returned recorder. The goroutine exits when the client's done channel
// is closed.
func TestDrainResponder(c *Client, caller *RPCCaller) *TestRPCRecorder {
	rec := &TestRPCRecorder{}

	go func() {
		for {
			select {
			case msg, ok := <-c.sendCh:
				if !ok {
					return
				}
				var req RPCRequest
				if err := json.Unmarshal(msg, &req); err != nil {
					continue
				}
				rec.append(req.Method, req.Params)
				resp := &RPCResponse{
					JSONRPC: jsonRPCVersion,
					ID:      req.ID,
					Result:  map[string]bool{"success": true},
				}
				caller.HandleResponse(resp)
			case <-c.done:
				return
			}
		}
	}()

	return rec
}
