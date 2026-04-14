package ws

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait is the maximum time allowed for a write to complete.
	writeWait = 10 * time.Second

	// pongWait is the maximum time to wait for a pong from the peer.
	pongWait = 60 * time.Second

	// pingPeriod sends pings at this interval. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// sendChSize is the buffer size for the outgoing message channel.
	sendChSize = 64

	// maxMessageSize is the maximum size in bytes for incoming WebSocket messages.
	maxMessageSize = 1 << 20 // 1MB
)

// MethodHandler processes a JSON-RPC request and returns the result or an error.
// The dongleID parameter identifies the device that sent the request.
type MethodHandler func(dongleID string, params json.RawMessage) (interface{}, *RPCError)

// Client wraps a single WebSocket connection for a device.
type Client struct {
	DongleID  string
	conn      *websocket.Conn
	hub       *Hub
	sendCh    chan []byte
	done      chan struct{}
	handlers  map[string]MethodHandler
	rpcCaller *RPCCaller
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewClient creates a Client for the given WebSocket connection and dongle ID.
// The handlers map provides method dispatch for incoming JSON-RPC requests.
// The rpcCaller, if non-nil, receives RPC responses from the device so that
// outgoing calls made via RPCCaller.Call can complete.
func NewClient(dongleID string, conn *websocket.Conn, hub *Hub, handlers map[string]MethodHandler, rpcCaller *RPCCaller) *Client {
	return &Client{
		DongleID:  dongleID,
		conn:      conn,
		hub:       hub,
		sendCh:    make(chan []byte, sendChSize),
		done:      make(chan struct{}),
		handlers:  handlers,
		rpcCaller: rpcCaller,
	}
}

// Run starts the read and write pumps. It blocks until both have exited.
func (c *Client) Run() {
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		c.writePump()
	}()
	go func() {
		defer c.wg.Done()
		c.readPump()
	}()
	c.wg.Wait()
}

// Close performs a graceful shutdown of the client connection.
// It is safe to call multiple times.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.conn != nil {
			c.conn.Close()
		}
	})
}

// Send enqueues a message for sending over the WebSocket.
// If the send channel is full, the message is dropped.
func (c *Client) Send(data []byte) {
	select {
	case c.sendCh <- data:
	default:
		log.Printf("dropped message for %s: send channel full", c.DongleID)
	}
}

// SendRPCRequest sends a JSON-RPC request to the connected device.
func (c *Client) SendRPCRequest(req *RPCRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal RPC request: %w", err)
	}
	c.Send(data)
	return nil
}

// readPump reads messages from the WebSocket and dispatches them.
func (c *Client) readPump() {
	defer func() {
		c.hub.Unregister(c)
		c.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("unexpected close for %s: %v", c.DongleID, err)
			}
			return
		}

		c.handleMessage(message)
	}
}

// writePump sends messages from the send channel to the WebSocket.
// All writes to the WebSocket are serialized through this single goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Close()
	}()

	for {
		select {
		case msg, ok := <-c.sendCh:
			if !ok {
				return
			}
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// handleMessage processes a single incoming WebSocket message. It first checks
// if the message is a JSON-RPC response (has id, no method) and routes it to
// the RPCCaller. Otherwise it is handled as a JSON-RPC request.
func (c *Client) handleMessage(data []byte) {
	if c.rpcCaller != nil {
		var peek struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if json.Unmarshal(data, &peek) == nil && peek.Method == "" && len(peek.ID) > 0 && string(peek.ID) != "null" {
			var resp RPCResponse
			if err := json.Unmarshal(data, &resp); err == nil {
				c.rpcCaller.HandleResponse(&resp)
				return
			}
		}
	}

	req, err := ParseRequest(data)
	if err != nil {
		resp := NewRPCErrorResponse(nil, NewRPCError(CodeParseError, err.Error()))
		c.sendResponse(resp)
		return
	}

	handler, ok := c.handlers[req.Method]
	if !ok {
		if !req.IsNotification() {
			resp := NewRPCErrorResponse(req.ID, NewRPCError(CodeMethodNotFound, fmt.Sprintf("method %q not found", req.Method)))
			c.sendResponse(resp)
		}
		return
	}

	result, rpcErr := handler(c.DongleID, req.Params)

	// Notifications do not receive responses.
	if req.IsNotification() {
		return
	}

	if rpcErr != nil {
		c.sendResponse(NewRPCErrorResponse(req.ID, rpcErr))
		return
	}

	c.sendResponse(NewRPCResponse(req.ID, result))
}

// sendResponse marshals and enqueues a JSON-RPC response.
func (c *Client) sendResponse(resp *RPCResponse) {
	data, err := MarshalResponse(resp)
	if err != nil {
		log.Printf("failed to marshal response for %s: %v", c.DongleID, err)
		return
	}
	c.Send(data)
}
