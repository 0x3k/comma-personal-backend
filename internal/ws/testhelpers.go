package ws

import (
	"encoding/json"
)

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
// returned slices (accessed via the pointers). The goroutine exits when
// the client's done channel is closed.
func TestDrainResponder(c *Client, caller *RPCCaller) (methods *[]string, params *[]json.RawMessage) {
	var m []string
	var p []json.RawMessage
	methods = &m
	params = &p

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
				m = append(m, req.Method)
				p = append(p, req.Params)
				*methods = m
				*params = p
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

	return methods, params
}
