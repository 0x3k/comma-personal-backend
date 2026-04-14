package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
)

const testJWTSecret = "test-secret-key-for-ws-tests"

func signTestToken(t *testing.T, dongleID string, secret string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"dongle_id": dongleID,
		"identity":  dongleID,
		"iat":       time.Now().Unix(),
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to sign test token: %v", err)
	}
	return signed
}

func setupTestServer(t *testing.T, handlers map[string]MethodHandler) (*httptest.Server, *Hub) {
	t.Helper()
	hub := NewHub()
	e := echo.New()
	h := NewHandler(hub, testJWTSecret, handlers)
	h.RegisterRoutes(e)

	server := httptest.NewServer(e)
	t.Cleanup(func() { server.Close() })
	return server, hub
}

func wsURL(server *httptest.Server, dongleID, token string) string {
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/" + dongleID
	if token != "" {
		url += "?token=" + token
	}
	return url
}

func TestHandleWebSocket_AuthWithQueryParam(t *testing.T) {
	server, hub := setupTestServer(t, nil)

	dongleID := "test-dongle-001"
	token := signTestToken(t, dongleID, testJWTSecret)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial WebSocket: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	// Give the goroutine a moment to register.
	time.Sleep(50 * time.Millisecond)

	_, ok := hub.GetClient(dongleID)
	if !ok {
		t.Error("client not found in hub after connection")
	}
}

func TestHandleWebSocket_AuthWithBearerHeader(t *testing.T) {
	server, hub := setupTestServer(t, nil)

	dongleID := "header-auth-dongle"
	token := signTestToken(t, dongleID, testJWTSecret)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	// Dial without query param, use header instead.
	url := wsURL(server, dongleID, "")
	conn, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("failed to dial WebSocket: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	time.Sleep(50 * time.Millisecond)

	_, ok := hub.GetClient(dongleID)
	if !ok {
		t.Error("client not found in hub after header auth connection")
	}
}

func TestHandleWebSocket_MissingToken(t *testing.T) {
	server, _ := setupTestServer(t, nil)

	url := wsURL(server, "some-dongle", "")
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("expected dial to fail without token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleWebSocket_InvalidToken(t *testing.T) {
	server, _ := setupTestServer(t, nil)

	url := wsURL(server, "some-dongle", "invalid.token.value")
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("expected dial to fail with invalid token")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestHandleWebSocket_WrongDongleID(t *testing.T) {
	server, _ := setupTestServer(t, nil)

	// Token is for "dongle-a" but we connect to "dongle-b".
	token := signTestToken(t, "dongle-a", testJWTSecret)
	url := wsURL(server, "dongle-b", token)

	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("expected dial to fail with mismatched dongle_id")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

func TestHandleWebSocket_WrongSecret(t *testing.T) {
	server, _ := setupTestServer(t, nil)

	dongleID := "wrong-secret-dongle"
	token := signTestToken(t, dongleID, "wrong-secret")

	url := wsURL(server, dongleID, token)
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("expected dial to fail with wrong secret")
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

	server, _ := setupTestServer(t, handlers)

	dongleID := "rpc-test-dongle"
	token := signTestToken(t, dongleID, testJWTSecret)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	// Send a JSON-RPC request.
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

	// Read the response.
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
	server, _ := setupTestServer(t, nil)

	dongleID := "method-test-dongle"
	token := signTestToken(t, dongleID, testJWTSecret)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
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
	server, _ := setupTestServer(t, nil)

	dongleID := "invalid-rpc-dongle"
	token := signTestToken(t, dongleID, testJWTSecret)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	// Send garbage that is not valid JSON-RPC.
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
	server, hub := setupTestServer(t, nil)

	dongleID := "cleanup-dongle"
	token := signTestToken(t, dongleID, testJWTSecret)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if hub.Count() != 1 {
		t.Fatalf("hub count = %d, want 1 before close", hub.Count())
	}

	conn.Close()

	// Wait for the read pump to detect the close and clean up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Count() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("hub count did not reach 0 after disconnect")
}

func TestHandleWebSocket_DuplicateConnectionReplacesExisting(t *testing.T) {
	server, hub := setupTestServer(t, nil)

	dongleID := "dup-dongle"
	token := signTestToken(t, dongleID, testJWTSecret)

	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial first connection: %v", err)
	}
	defer conn1.Close()

	time.Sleep(50 * time.Millisecond)

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(server, dongleID, token), nil)
	if err != nil {
		t.Fatalf("failed to dial second connection: %v", err)
	}
	defer conn2.Close()

	time.Sleep(50 * time.Millisecond)

	if hub.Count() != 1 {
		t.Errorf("hub count = %d, want 1 (duplicate should replace)", hub.Count())
	}
}
