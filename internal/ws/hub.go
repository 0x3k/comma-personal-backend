package ws

import (
	"sync"
)

// Hub tracks active WebSocket connections indexed by dongle ID.
// It enforces at most one active connection per device: registering a new
// client for an already-connected dongle_id closes the previous connection.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

// NewHub creates an empty Hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[string]*Client),
	}
}

// Register adds a client to the hub. If there is already an active connection
// for the same dongle_id, the existing connection is closed first.
func (h *Hub) Register(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.clients[c.DongleID]; ok {
		existing.Close()
	}
	h.clients[c.DongleID] = c
}

// Unregister removes a client from the hub. It only removes the entry if the
// stored client matches the one being unregistered (to avoid removing a newer
// connection that replaced this one).
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.clients[c.DongleID]; ok && existing == c {
		delete(h.clients, c.DongleID)
	}
}

// GetClient returns the active client for a dongle ID, or nil if not connected.
func (h *Hub) GetClient(dongleID string) (*Client, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c, ok := h.clients[dongleID]
	return c, ok
}

// Count returns the number of active connections.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
