package ws

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// fakeLookup is a test DeviceLookup that returns devices from a map keyed by
// dongle_id. An empty map means "unknown device" (pgx.ErrNoRows).
type fakeLookup struct {
	mu      sync.Mutex
	devices map[string]db.Device
}

func (f *fakeLookup) GetDevice(_ context.Context, dongleID string) (db.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[dongleID]
	if !ok {
		return db.Device{}, pgx.ErrNoRows
	}
	return d, nil
}

func newFakeLookup(devices ...db.Device) *fakeLookup {
	m := make(map[string]db.Device, len(devices))
	for _, d := range devices {
		m[d.DongleID] = d
	}
	return &fakeLookup{devices: m}
}

// testKeyPair generates an RSA key and returns it alongside the PEM-encoded
// public key that pilotauth would store in devices.public_key.
func testKeyPair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate rsa key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	return priv, string(pemBytes)
}

func testDevice(dongleID, pubPEM string) db.Device {
	now := time.Now()
	return db.Device{
		DongleID:  dongleID,
		PublicKey: pgtype.Text{String: pubPEM, Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func signTestToken(t *testing.T, dongleID string, priv *rsa.PrivateKey) string {
	t.Helper()
	claims := jwt.MapClaims{
		"identity": dongleID,
		"iat":      time.Now().Unix(),
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("failed to sign test token: %v", err)
	}
	return signed
}

// testSetup is a small bundle of the pieces most tests need: a running WS
// server, the hub it registers clients to, the device's private key, and the
// dongle_id that key was registered under.
type testSetup struct {
	server   *httptest.Server
	hub      *Hub
	priv     *rsa.PrivateKey
	dongleID string
}

func newTestSetup(t *testing.T, dongleID string, handlers map[string]MethodHandler) testSetup {
	t.Helper()
	priv, pubPEM := testKeyPair(t)
	hub := NewHub()
	lookup := newFakeLookup(testDevice(dongleID, pubPEM))
	e := echo.New()
	h := NewHandler(hub, lookup, handlers, nil)
	h.RegisterRoutes(e)
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	return testSetup{server: server, hub: hub, priv: priv, dongleID: dongleID}
}

func wsURL(server *httptest.Server, dongleID, token string) string {
	u := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/" + dongleID
	if token != "" {
		u += "?token=" + token
	}
	return u
}

func dialWithCookie(t *testing.T, server *httptest.Server, dongleID, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/" + dongleID
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("failed to parse server url: %v", err)
	}
	jar, err := newCookieJar(parsed, token)
	if err != nil {
		t.Fatalf("failed to build cookie jar: %v", err)
	}
	dialer := *websocket.DefaultDialer
	dialer.Jar = jar
	return dialer.Dial(u, nil)
}

func newCookieJar(serverURL *url.URL, token string) (*cookieJar, error) {
	return &cookieJar{url: serverURL, cookies: []*http.Cookie{{Name: "jwt", Value: token}}}, nil
}

// cookieJar is a minimal http.CookieJar that returns a fixed cookie for any
// URL whose scheme+host matches the one it was constructed with. The standard
// library's cookiejar.New rejects "ws://" URLs, so we use this instead.
type cookieJar struct {
	url     *url.URL
	cookies []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *url.URL, _ []*http.Cookie) {}

func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	return j.cookies
}

func TestHandleWebSocket_AuthWithQueryParam(t *testing.T) {
	s := newTestSetup(t, "test-dongle-001", nil)

	token := signTestToken(t, s.dongleID, s.priv)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial WebSocket: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := s.hub.GetClient(s.dongleID); !ok {
		t.Error("client not found in hub after connection")
	}
}

func TestHandleWebSocket_AuthWithCookie(t *testing.T) {
	s := newTestSetup(t, "cookie-auth-dongle", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	conn, resp, err := dialWithCookie(t, s.server, s.dongleID, token)
	if err != nil {
		t.Fatalf("failed to dial WebSocket with cookie: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := s.hub.GetClient(s.dongleID); !ok {
		t.Error("client not found in hub after cookie auth connection")
	}
}

func TestHandleWebSocket_AuthWithJWTHeader(t *testing.T) {
	s := newTestSetup(t, "header-auth-dongle", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	header := http.Header{}
	header.Set("Authorization", "JWT "+token)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, ""), header)
	if err != nil {
		t.Fatalf("failed to dial WebSocket: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	time.Sleep(50 * time.Millisecond)

	if _, ok := s.hub.GetClient(s.dongleID); !ok {
		t.Error("client not found in hub after header auth connection")
	}
}

func TestHandleWebSocket_MissingToken(t *testing.T) {
	s := newTestSetup(t, "some-dongle", nil)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, "some-dongle", ""), nil)
	if err == nil {
		t.Fatal("expected dial to fail without token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleWebSocket_InvalidToken(t *testing.T) {
	s := newTestSetup(t, "some-dongle", nil)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, "some-dongle", "invalid.token.value"), nil)
	if err == nil {
		t.Fatal("expected dial to fail with invalid token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleWebSocket_WrongDongleID(t *testing.T) {
	s := newTestSetup(t, "dongle-a", nil)

	// Token identity is "dongle-a" (matching the setup), but the URL asks
	// for "dongle-b". The middleware authenticates for dongle-a, then the
	// handler rejects the mismatch with 403.
	token := signTestToken(t, "dongle-a", s.priv)
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, "dongle-b", token), nil)
	if err == nil {
		t.Fatal("expected dial to fail with mismatched dongle_id")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestHandleWebSocket_WrongKey(t *testing.T) {
	s := newTestSetup(t, "wrong-key-dongle", nil)

	// Sign with a different private key than the one registered for this
	// dongle — signature verification must fail.
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate other key: %v", err)
	}
	token := signTestToken(t, s.dongleID, otherKey)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err == nil {
		t.Fatal("expected dial to fail with wrong signing key")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleWebSocket_UnknownDongle(t *testing.T) {
	// Server knows about "known-dongle" only; sign a valid token for an
	// unregistered identity.
	s := newTestSetup(t, "known-dongle", nil)
	token := signTestToken(t, "not-registered", s.priv)

	_, resp, err := websocket.DefaultDialer.Dial(wsURL(s.server, "not-registered", token), nil)
	if err == nil {
		t.Fatal("expected dial to fail with unknown dongle")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleWebSocket_JSONRPCMethodDispatch(t *testing.T) {
	handlers := map[string]MethodHandler{
		"echo": func(dongleID string, params json.RawMessage) (interface{}, *RPCError) {
			var p map[string]string
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, NewRPCError(CodeInvalidParams, "bad params")
			}
			return p, nil
		},
	}

	s := newTestSetup(t, "rpc-test-dongle", handlers)
	token := signTestToken(t, s.dongleID, s.priv)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	req := RPCRequest{
		JSONRPC: "2.0",
		Method:  "echo",
		Params:  json.RawMessage(`{"msg":"hello"}`),
		ID:      json.RawMessage(`1`),
	}
	reqData, _ := json.Marshal(req)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("failed to write message: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, respData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}

	var resp RPCResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp.JSONRPC != "2.0" {
		t.Errorf("response JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
	}
	if resp.Error != nil {
		t.Errorf("unexpected error in response: %v", resp.Error)
	}
	if string(resp.ID) != "1" {
		t.Errorf("response ID = %s, want 1", string(resp.ID))
	}
}

func TestHandleWebSocket_MethodNotFound(t *testing.T) {
	s := newTestSetup(t, "method-test-dongle", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	req := RPCRequest{
		JSONRPC: "2.0",
		Method:  "nonexistent",
		ID:      json.RawMessage(`2`),
	}
	reqData, _ := json.Marshal(req)
	if err := conn.WriteMessage(websocket.TextMessage, reqData); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, respData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	var resp RPCResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected error response for unknown method")
	}
	if resp.Error.Code != CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, CodeMethodNotFound)
	}
}

func TestHandleWebSocket_InvalidJSONRPC(t *testing.T) {
	s := newTestSetup(t, "invalid-rpc-dongle", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"not":"valid rpc"}`)); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, respData, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read: %v", err)
	}

	var resp RPCResponse
	if err := json.Unmarshal(respData, &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if resp.Error == nil {
		t.Fatal("expected parse error response")
	}
	if resp.Error.Code != CodeParseError {
		t.Errorf("error code = %d, want %d", resp.Error.Code, CodeParseError)
	}
}

func TestHandleWebSocket_DisconnectCleansUp(t *testing.T) {
	s := newTestSetup(t, "cleanup-dongle", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if s.hub.Count() != 1 {
		t.Fatalf("hub count = %d, want 1 before close", s.hub.Count())
	}

	conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.hub.Count() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("hub count did not reach 0 after disconnect")
}

func TestHandleWebSocket_DuplicateConnectionReplacesExisting(t *testing.T) {
	s := newTestSetup(t, "dup-dongle", nil)
	token := signTestToken(t, s.dongleID, s.priv)

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial first connection: %v", err)
	}
	defer conn1.Close()

	time.Sleep(50 * time.Millisecond)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(s.server, s.dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial second connection: %v", err)
	}
	defer conn2.Close()

	time.Sleep(50 * time.Millisecond)

	if s.hub.Count() != 1 {
		t.Errorf("hub count = %d, want 1 (duplicate should replace)", s.hub.Count())
	}
}
