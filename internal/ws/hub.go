package ws

import (
	"sync"

	"comma-personal-backend/internal/metrics"
)

// Hub tracks active WebSocket connections indexed by dongle ID.
// It enforces at most one active connection per device: registering a new
// client for an already-connected dongle_id closes the previous connection.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
	metrics *metrics.Metrics
}

// NewHub creates an empty Hub.
func NewHub() *Hub {
	return NewHubWithMetrics(nil)
}

// NewHubWithMetrics creates a Hub that keeps the ws_connected_devices gauge
// in sync with the number of registered clients. A nil m is treated as a
// no-op.
func NewHubWithMetrics(m *metrics.Metrics) *Hub {
	return &Hub{
		clients: make(map[string]*Client),
		metrics: m,
	}
}

// Register adds a client to the hub. If there is already an active connection
// for the same dongle_id, the existing connection is closed first.
func (h *Hub) Register(c *Client) {
	var existing *Client
	var count int

	h.mu.Lock()
	existing = h.clients[c.DongleID]
	h.clients[c.DongleID] = c
	count = len(h.clients)
	h.mu.Unlock()

	if existing != nil {
		existing.Close()
	}
	h.metrics.SetConnectedDevices(count)
}

// Unregister removes a client from the hub. It only removes the entry if the
// stored client matches the one being unregistered (to avoid removing a newer
// connection that replaced this one).
func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	if existing, ok := h.clients[c.DongleID]; ok && existing == c {
		delete(h.clients, c.DongleID)
	}
	count := len(h.clients)
	h.mu.Unlock()

	h.metrics.SetConnectedDevices(count)
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
