package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/ws"
)

// configMockDBTX implements db.DBTX for config handler tests.
// It supports the three query shapes used by device_params.sql.go:
//   - Query (ListDeviceParams)
//   - QueryRow (SetDeviceParam)
//   - Exec (DeleteDeviceParam)
type configMockDBTX struct {
	params   []db.DeviceParam
	setParam *db.DeviceParam
	err      error
}

func (m *configMockDBTX) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	if m.err != nil {
		return pgconn.CommandTag{}, m.err
	}
	return pgconn.CommandTag{}, nil
}

func (m *configMockDBTX) Query(_ context.Context, _ string, _ ...interface{}) (pgx.Rows, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &configMockRows{params: m.params, idx: -1}, nil
}

func (m *configMockDBTX) QueryRow(_ context.Context, _ string, _ ...interface{}) pgx.Row {
	return &configMockRow{param: m.setParam, err: m.err}
}

// configMockRows implements pgx.Rows for the ListDeviceParams query.
type configMockRows struct {
	params []db.DeviceParam
	idx    int
}

func (r *configMockRows) Close()                                       {}
func (r *configMockRows) Err() error                                   { return nil }
func (r *configMockRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *configMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *configMockRows) RawValues() [][]byte                          { return nil }
func (r *configMockRows) Conn() *pgx.Conn                              { return nil }
func (r *configMockRows) Values() ([]interface{}, error)               { return nil, nil }

func (r *configMockRows) Next() bool {
	r.idx++
	return r.idx < len(r.params)
}

func (r *configMockRows) Scan(dest ...interface{}) error {
	if r.idx < 0 || r.idx >= len(r.params) {
		return fmt.Errorf("no current row")
	}
	p := r.params[r.idx]
	if len(dest) < 5 {
		return fmt.Errorf("expected 5 scan destinations, got %d", len(dest))
	}
	*dest[0].(*int32) = p.ID
	*dest[1].(*string) = p.DongleID
	*dest[2].(*string) = p.Key
	*dest[3].(*string) = p.Value
	*dest[4].(*pgtype.Timestamptz) = p.UpdatedAt
	return nil
}

// configMockRow implements pgx.Row for the SetDeviceParam query.
type configMockRow struct {
	param *db.DeviceParam
	err   error
}

func (r *configMockRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if r.param == nil {
		return fmt.Errorf("no param")
	}
	if len(dest) < 5 {
		return fmt.Errorf("expected 5 scan destinations, got %d", len(dest))
	}
	*dest[0].(*int32) = r.param.ID
	*dest[1].(*string) = r.param.DongleID
	*dest[2].(*string) = r.param.Key
	*dest[3].(*string) = r.param.Value
	*dest[4].(*pgtype.Timestamptz) = r.param.UpdatedAt
	return nil
}

func newConfigHandler(mock *configMockDBTX, hub *ws.Hub, rpc *ws.RPCCaller) *ConfigHandler {
	queries := db.New(mock)
	return NewConfigHandler(queries, hub, rpc)
}

func newTestParam(id int32, dongleID, key, value string) db.DeviceParam {
	return db.DeviceParam{
		ID:        id,
		DongleID:  dongleID,
		Key:       key,
		Value:     value,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

func TestListParams(t *testing.T) {
	tests := []struct {
		name           string
		dongleID       string
		params         []db.DeviceParam
		err            error
		wantStatus     int
		wantCount      int
		wantErrMessage string
	}{
		{
			name:     "returns all params",
			dongleID: "abc123",
			params: []db.DeviceParam{
				newTestParam(1, "abc123", "OpenpilotEnabledToggle", "1"),
				newTestParam(2, "abc123", "IsMetric", "0"),
			},
			wantStatus: http.StatusOK,
			wantCount:  2,
		},
		{
			name:       "returns empty list for device with no params",
			dongleID:   "abc123",
			params:     nil,
			wantStatus: http.StatusOK,
			wantCount:  0,
		},
		{
			name:           "database error",
			dongleID:       "abc123",
			err:            fmt.Errorf("connection refused"),
			wantStatus:     http.StatusInternalServerError,
			wantErrMessage: "failed to list device params",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &configMockDBTX{params: tt.params, err: tt.err}
			handler := newConfigHandler(mock, nil, nil)

			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/v1/devices/"+tt.dongleID+"/params", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id")
			c.SetParamValues(tt.dongleID)

			err := handler.ListParams(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var result []paramResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if len(result) != tt.wantCount {
					t.Errorf("count = %d, want %d", len(result), tt.wantCount)
				}
				for i, p := range result {
					if p.Key != tt.params[i].Key {
						t.Errorf("param[%d].key = %q, want %q", i, p.Key, tt.params[i].Key)
					}
					if p.Value != tt.params[i].Value {
						t.Errorf("param[%d].value = %q, want %q", i, p.Value, tt.params[i].Value)
					}
				}
			}

			if tt.wantErrMessage != "" {
				var errResp errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to unmarshal error response: %v", err)
				}
				if errResp.Error != tt.wantErrMessage {
					t.Errorf("error = %q, want %q", errResp.Error, tt.wantErrMessage)
				}
			}
		})
	}
}

func TestSetParam(t *testing.T) {
	tests := []struct {
		name           string
		dongleID       string
		key            string
		body           string
		setParam       *db.DeviceParam
		err            error
		wantStatus     int
		wantErrMessage string
	}{
		{
			name:     "sets a new param",
			dongleID: "abc123",
			key:      "OpenpilotEnabledToggle",
			body:     `{"value":"1"}`,
			setParam: func() *db.DeviceParam {
				p := newTestParam(1, "abc123", "OpenpilotEnabledToggle", "1")
				return &p
			}(),
			wantStatus: http.StatusOK,
		},
		{
			name:     "sets empty value",
			dongleID: "abc123",
			key:      "SomeKey",
			body:     `{"value":""}`,
			setParam: func() *db.DeviceParam {
				p := newTestParam(2, "abc123", "SomeKey", "")
				return &p
			}(),
			wantStatus: http.StatusOK,
		},
		{
			name:           "database error",
			dongleID:       "abc123",
			key:            "OpenpilotEnabledToggle",
			body:           `{"value":"1"}`,
			err:            fmt.Errorf("connection refused"),
			wantStatus:     http.StatusInternalServerError,
			wantErrMessage: "failed to set device param",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &configMockDBTX{setParam: tt.setParam, err: tt.err}
			handler := newConfigHandler(mock, nil, nil)

			e := echo.New()
			req := httptest.NewRequest(http.MethodPut, "/v1/devices/"+tt.dongleID+"/params/"+tt.key, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id", "key")
			c.SetParamValues(tt.dongleID, tt.key)

			err := handler.SetParam(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantStatus == http.StatusOK {
				var resp paramResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.Key != tt.setParam.Key {
					t.Errorf("key = %q, want %q", resp.Key, tt.setParam.Key)
				}
				if resp.Value != tt.setParam.Value {
					t.Errorf("value = %q, want %q", resp.Value, tt.setParam.Value)
				}
			}

			if tt.wantErrMessage != "" {
				var errResp errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to unmarshal error response: %v", err)
				}
				if errResp.Error != tt.wantErrMessage {
					t.Errorf("error = %q, want %q", errResp.Error, tt.wantErrMessage)
				}
			}
		})
	}
}

func TestDeleteParam(t *testing.T) {
	tests := []struct {
		name           string
		dongleID       string
		key            string
		err            error
		wantStatus     int
		wantErrMessage string
	}{
		{
			name:       "deletes a param",
			dongleID:   "abc123",
			key:        "OpenpilotEnabledToggle",
			wantStatus: http.StatusNoContent,
		},
		{
			name:           "database error",
			dongleID:       "abc123",
			key:            "OpenpilotEnabledToggle",
			err:            fmt.Errorf("connection refused"),
			wantStatus:     http.StatusInternalServerError,
			wantErrMessage: "failed to delete device param",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &configMockDBTX{err: tt.err}
			handler := newConfigHandler(mock, nil, nil)

			e := echo.New()
			req := httptest.NewRequest(http.MethodDelete, "/v1/devices/"+tt.dongleID+"/params/"+tt.key, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id", "key")
			c.SetParamValues(tt.dongleID, tt.key)

			err := handler.DeleteParam(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantErrMessage != "" {
				var errResp errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to unmarshal error response: %v", err)
				}
				if errResp.Error != tt.wantErrMessage {
					t.Errorf("error = %q, want %q", errResp.Error, tt.wantErrMessage)
				}
			}
		})
	}
}

func TestConfigRegisterRoutes(t *testing.T) {
	mock := &configMockDBTX{}
	handler := newConfigHandler(mock, nil, nil)

	e := echo.New()
	g := e.Group("/v1")
	handler.RegisterRoutes(g)

	routes := e.Routes()

	expected := map[string]bool{
		"GET /v1/devices/:dongle_id/params":         true,
		"PUT /v1/devices/:dongle_id/params/:key":    true,
		"DELETE /v1/devices/:dongle_id/params/:key": true,
	}

	for _, r := range routes {
		key := r.Method + " " + r.Path
		delete(expected, key)
	}

	for route := range expected {
		t.Errorf("expected route %s to be registered", route)
	}
}

func TestConfigWithJWTAuth(t *testing.T) {
	const secret = "test-config-auth-secret"

	testParams := []db.DeviceParam{
		newTestParam(1, "abc123", "OpenpilotEnabledToggle", "1"),
	}

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "authenticated request succeeds",
			authHeader: "Bearer " + signTestToken(t, secret, "abc123"),
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing auth returns 401",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid token returns 401",
			authHeader: "Bearer bad.token.here",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &configMockDBTX{params: testParams}
			handler := newConfigHandler(mock, nil, nil)

			e := echo.New()
			g := e.Group("/v1", middleware.JWTAuthHMAC(secret))
			handler.RegisterRoutes(g)

			req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/params", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			e.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestSetParamPushesWebSocket verifies that setting a parameter sends an
// RPC call to a connected device via the WebSocket hub.
func TestSetParamPushesWebSocket(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()

	// Create a test client with a buffered send channel (no real WebSocket).
	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	// Start a responder goroutine that reads RPC requests from the client's
	// send channel and sends back successful responses.
	methods, params := ws.TestDrainResponder(client, rpc)

	setP := newTestParam(1, "abc123", "OpenpilotEnabledToggle", "1")
	mock := &configMockDBTX{setParam: &setP}
	handler := newConfigHandler(mock, hub, rpc)

	e := echo.New()
	body := `{"value":"1"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/devices/abc123/params/OpenpilotEnabledToggle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "key")
	c.SetParamValues("abc123", "OpenpilotEnabledToggle")

	if err := handler.SetParam(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	// Wait for the async RPC push to be processed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(*methods) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(*methods) == 0 {
		t.Fatal("expected at least one RPC call to be sent")
	}

	if (*methods)[0] != "setParam" {
		t.Errorf("RPC method = %q, want %q", (*methods)[0], "setParam")
	}

	var p map[string]string
	if err := json.Unmarshal((*params)[0], &p); err != nil {
		t.Fatalf("failed to unmarshal RPC params: %v", err)
	}
	if p["key"] != "OpenpilotEnabledToggle" {
		t.Errorf("RPC param key = %q, want %q", p["key"], "OpenpilotEnabledToggle")
	}
	if p["value"] != "1" {
		t.Errorf("RPC param value = %q, want %q", p["value"], "1")
	}
}

// TestDeleteParamPushesWebSocket verifies that deleting a parameter sends
// an RPC call to a connected device via the WebSocket hub.
func TestDeleteParamPushesWebSocket(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()

	client := ws.TestNewClient("abc123", hub)
	hub.Register(client)
	t.Cleanup(func() { client.Close() })

	methods, params := ws.TestDrainResponder(client, rpc)

	mock := &configMockDBTX{}
	handler := newConfigHandler(mock, hub, rpc)

	e := echo.New()
	req := httptest.NewRequest(http.MethodDelete, "/v1/devices/abc123/params/OpenpilotEnabledToggle", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "key")
	c.SetParamValues("abc123", "OpenpilotEnabledToggle")

	if err := handler.DeleteParam(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(*methods) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(*methods) == 0 {
		t.Fatal("expected at least one RPC call to be sent")
	}

	if (*methods)[0] != "deleteParam" {
		t.Errorf("RPC method = %q, want %q", (*methods)[0], "deleteParam")
	}

	var p map[string]string
	if err := json.Unmarshal((*params)[0], &p); err != nil {
		t.Fatalf("failed to unmarshal RPC params: %v", err)
	}
	if p["key"] != "OpenpilotEnabledToggle" {
		t.Errorf("RPC param key = %q, want %q", p["key"], "OpenpilotEnabledToggle")
	}
}

// TestSetParamNoWebSocketConnection verifies that SetParam succeeds even
// when no device is connected via WebSocket.
func TestSetParamNoWebSocketConnection(t *testing.T) {
	hub := ws.NewHub()
	rpc := ws.NewRPCCaller()

	setP := newTestParam(1, "abc123", "OpenpilotEnabledToggle", "1")
	mock := &configMockDBTX{setParam: &setP}
	handler := newConfigHandler(mock, hub, rpc)

	e := echo.New()
	body := `{"value":"1"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/devices/abc123/params/OpenpilotEnabledToggle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "key")
	c.SetParamValues("abc123", "OpenpilotEnabledToggle")

	if err := handler.SetParam(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

// TestSetParamNilHub verifies that SetParam works when hub is nil.
func TestSetParamNilHub(t *testing.T) {
	setP := newTestParam(1, "abc123", "OpenpilotEnabledToggle", "1")
	mock := &configMockDBTX{setParam: &setP}
	handler := newConfigHandler(mock, nil, nil)

	e := echo.New()
	body := `{"value":"1"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/devices/abc123/params/OpenpilotEnabledToggle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "key")
	c.SetParamValues("abc123", "OpenpilotEnabledToggle")

	if err := handler.SetParam(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
