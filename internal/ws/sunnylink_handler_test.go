package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// fakeSunnylinkLookup is a SunnylinkDeviceLookup test double. It also
// satisfies middleware.DeviceLookup so the same value can be passed where
// NewHandler expects the broader interface.
type fakeSunnylinkLookup struct {
	mu      sync.Mutex
	devices map[string]db.Device // keyed by sunnylink_dongle_id
	commaIx map[string]db.Device // keyed by comma_dongle_id
}

func newFakeSunnylinkLookup(devices ...db.Device) *fakeSunnylinkLookup {
	f := &fakeSunnylinkLookup{
		devices: make(map[string]db.Device, len(devices)),
		commaIx: make(map[string]db.Device, len(devices)),
	}
	for _, d := range devices {
		if d.SunnylinkDongleID.Valid {
			f.devices[d.SunnylinkDongleID.String] = d
		}
		f.commaIx[d.DongleID] = d
	}
	return f
}

func (f *fakeSunnylinkLookup) GetDevice(_ context.Context, dongleID string) (db.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.commaIx[dongleID]
	if !ok {
		return db.Device{}, pgx.ErrNoRows
	}
	return d, nil
}

func (f *fakeSunnylinkLookup) GetDeviceBySunnylinkDongleID(_ context.Context, sunnylinkID pgtype.Text) (db.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !sunnylinkID.Valid {
		return db.Device{}, pgx.ErrNoRows
	}
	d, ok := f.devices[sunnylinkID.String]
	if !ok {
		return db.Device{}, pgx.ErrNoRows
	}
	return d, nil
}

// newSunnylinkSetup builds an httptest server with the sunnylink WS path
// registered. The returned device has both a comma_dongle_id and a
// sunnylink_dongle_id; callers connect via the latter using a Bearer JWT
// signed by the device's key.
func newSunnylinkSetup(t *testing.T, commaID, sunnylinkID string, handlers map[string]MethodHandler) testSetup {
	t.Helper()
	priv, pubPEM := testKeyPair(t)
	device := testDevice(commaID, pubPEM)
	device.SunnylinkDongleID = pgtype.Text{String: sunnylinkID, Valid: true}
	device.SunnylinkPublicKey = pgtype.Text{String: pubPEM, Valid: true}

	hub := NewHub()
	lookup := newFakeSunnylinkLookup(device)
	e := echo.New()
	h := NewHandler(hub, lookup, handlers, nil)
	h.RegisterRoutes(e)
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	return testSetup{server: server, hub: hub, priv: priv, dongleID: sunnylinkID}
}

func sunnylinkWSURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/sunnylink"
}

func TestHandleSunnylinkWebSocket_BearerHeaderAuth(t *testing.T) {
	s := newSunnylinkSetup(t, "abc123def4567890", "f0e1d2c3b4a59687", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	conn, resp, err := websocket.DefaultDialer.Dial(sunnylinkWSURL(s.server), header)
	if err != nil {
		t.Fatalf("failed to dial sunnylink WebSocket: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := s.hub.GetClient(s.dongleID); !ok {
		t.Errorf("sunnylink client not found in hub under sunnylink_dongle_id %q", s.dongleID)
	}
}

func TestHandleSunnylinkWebSocket_RejectsUnknownDongle(t *testing.T) {
	s := newSunnylinkSetup(t, "abc123def4567890", "f0e1d2c3b4a59687", nil)

	// Sign a token claiming an identity that doesn't match the registered
	// sunnylink_dongle_id.
	token := signTestToken(t, "ffffffffffffffff", s.priv)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	_, resp, err := websocket.DefaultDialer.Dial(sunnylinkWSURL(s.server), header)
	if err == nil {
		t.Fatal("expected dial to fail for unknown sunnylink dongle")
	}
	if resp == nil {
		t.Fatal("expected an HTTP error response")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleSunnylinkWebSocket_RejectsTokenWithMismatchedKey(t *testing.T) {
	s := newSunnylinkSetup(t, "abc123def4567890", "f0e1d2c3b4a59687", nil)

	// Sign a token with a DIFFERENT private key than the one stored in
	// devices.sunnylink_public_key. The signature check must fail.
	otherPriv, _ := testKeyPair(t)
	token := signTestToken(t, s.dongleID, otherPriv)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	_, resp, err := websocket.DefaultDialer.Dial(sunnylinkWSURL(s.server), header)
	if err == nil {
		t.Fatal("expected dial to fail for token signed by wrong key")
	}
	if resp == nil {
		t.Fatal("expected an HTTP error response")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleSunnylinkWebSocket_RejectsMissingToken(t *testing.T) {
	s := newSunnylinkSetup(t, "abc123def4567890", "f0e1d2c3b4a59687", nil)

	_, resp, err := websocket.DefaultDialer.Dial(sunnylinkWSURL(s.server), nil)
	if err == nil {
		t.Fatal("expected dial to fail without an auth token")
	}
	if resp == nil {
		t.Fatal("expected an HTTP error response")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleSunnylinkWebSocket_DisabledWhenLookupNotProvided(t *testing.T) {
	// Pass a lookup that does NOT satisfy SunnylinkDeviceLookup. The route
	// is still registered (so a misconfigured deployment doesn't 404), but
	// every connect attempt has to be rejected at handshake time.
	priv, pubPEM := testKeyPair(t)
	hub := NewHub()
	lookup := newFakeLookup(testDevice("abc123", pubPEM))
	e := echo.New()
	h := NewHandler(hub, lookup, nil, nil)
	h.RegisterRoutes(e)
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)

	token := signTestToken(t, "abc123", priv)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	_, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/ws/sunnylink",
		header,
	)
	if err == nil {
		t.Fatal("expected dial to fail when sunnylink lookup is not configured")
	}
	if resp == nil {
		t.Fatal("expected an HTTP error response")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestHandleSunnylinkWebSocket_HubKeyedBySunnylinkDongle(t *testing.T) {
	// A device with both a comma athena and a sunnylink WS connection open
	// should appear in the hub under both keys -- not collide.
	commaID := "abc123def4567890"
	sunnylinkID := "f0e1d2c3b4a59687"
	priv, pubPEM := testKeyPair(t)

	device := testDevice(commaID, pubPEM)
	device.SunnylinkDongleID = pgtype.Text{String: sunnylinkID, Valid: true}
	device.SunnylinkPublicKey = pgtype.Text{String: pubPEM, Valid: true}

	hub := NewHub()
	lookup := newFakeSunnylinkLookup(device)
	e := echo.New()
	h := NewHandler(hub, lookup, nil, nil)
	h.RegisterRoutes(e)
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)

	// Open the comma athena connection.
	commaToken := signTestToken(t, commaID, priv)
	commaConn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/ws/v2/"+commaID+"?token="+commaToken,
		nil,
	)
	if err != nil {
		t.Fatalf("failed to dial comma WS: %v", err)
	}
	defer commaConn.Close()

	// Open the sunnylink connection in parallel.
	sunnylinkToken := signTestToken(t, sunnylinkID, priv)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+sunnylinkToken)
	sunnylinkConn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/ws/sunnylink",
		header,
	)
	if err != nil {
		t.Fatalf("failed to dial sunnylink WS: %v", err)
	}
	defer sunnylinkConn.Close()

	time.Sleep(100 * time.Millisecond)

	if _, ok := hub.GetClient(commaID); !ok {
		t.Errorf("comma client not in hub under %q", commaID)
	}
	if _, ok := hub.GetClient(sunnylinkID); !ok {
		t.Errorf("sunnylink client not in hub under %q", sunnylinkID)
	}
	if hub.Count() != 2 {
		t.Errorf("hub.Count() = %d, want 2 (one comma, one sunnylink)", hub.Count())
	}
}
