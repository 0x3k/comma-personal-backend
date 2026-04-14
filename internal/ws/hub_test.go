package ws

import (
	"testing"
)

func newTestClient(dongleID string, hub *Hub) *Client {
	return &Client{
		DongleID: dongleID,
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}
}

func TestHub_RegisterAndGet(t *testing.T) {
	hub := NewHub()
	c := newTestClient("abc123", hub)

	hub.Register(c)

	got, ok := hub.GetClient("abc123")
	if !ok {
		t.Fatal("GetClient returned false, expected true")
	}
	if got != c {
		t.Error("GetClient returned different client")
	}

	if hub.Count() != 1 {
		t.Errorf("Count() = %d, want 1", hub.Count())
	}
}

func TestHub_GetClient_NotFound(t *testing.T) {
	hub := NewHub()

	_, ok := hub.GetClient("nonexistent")
	if ok {
		t.Fatal("GetClient returned true for nonexistent dongle_id")
	}
}

func TestHub_Unregister(t *testing.T) {
	hub := NewHub()
	c := newTestClient("abc123", hub)

	hub.Register(c)
	hub.Unregister(c)

	_, ok := hub.GetClient("abc123")
	if ok {
		t.Fatal("GetClient returned true after Unregister")
	}

	if hub.Count() != 0 {
		t.Errorf("Count() = %d, want 0", hub.Count())
	}
}

func TestHub_RegisterDuplicate_ClosesExisting(t *testing.T) {
	hub := NewHub()
	c1 := newTestClient("abc123", hub)
	c2 := newTestClient("abc123", hub)

	hub.Register(c1)
	hub.Register(c2)

	// c1 should have been closed.
	select {
	case <-c1.done:
		// expected: c1 was closed
	default:
		t.Error("existing client was not closed on duplicate registration")
	}

	got, ok := hub.GetClient("abc123")
	if !ok {
		t.Fatal("GetClient returned false after re-register")
	}
	if got != c2 {
		t.Error("GetClient returned old client instead of new one")
	}

	if hub.Count() != 1 {
		t.Errorf("Count() = %d, want 1", hub.Count())
	}
}

func TestHub_Unregister_StaleClient(t *testing.T) {
	hub := NewHub()
	c1 := newTestClient("abc123", hub)
	c2 := newTestClient("abc123", hub)

	hub.Register(c1)
	hub.Register(c2)

	// Unregistering c1 should not remove c2.
	hub.Unregister(c1)

	got, ok := hub.GetClient("abc123")
	if !ok {
		t.Fatal("GetClient returned false, expected c2 to still be registered")
	}
	if got != c2 {
		t.Error("GetClient returned wrong client")
	}
}

func TestHub_MultipleDevices(t *testing.T) {
	hub := NewHub()
	c1 := newTestClient("device1", hub)
	c2 := newTestClient("device2", hub)
	c3 := newTestClient("device3", hub)

	hub.Register(c1)
	hub.Register(c2)
	hub.Register(c3)

	if hub.Count() != 3 {
		t.Errorf("Count() = %d, want 3", hub.Count())
	}

	hub.Unregister(c2)

	if hub.Count() != 2 {
		t.Errorf("Count() = %d, want 2", hub.Count())
	}

	_, ok := hub.GetClient("device2")
	if ok {
		t.Error("device2 should not be registered after unregister")
	}
}
