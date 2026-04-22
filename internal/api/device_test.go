package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

func newDeviceHandler(mock *mockDBTX) *DeviceHandler {
	queries := db.New(mock)
	return NewDeviceHandler(queries)
}

func TestGetDevice(t *testing.T) {
	testDevice := newTestDevice("abc123", "SERIAL001", "ssh-rsa AAAA...")

	tests := []struct {
		name           string
		dongleID       string
		mockDevice     *db.Device
		mockErr        error
		wantStatus     int
		wantErrMessage string
	}{
		{
			name:       "successful device lookup",
			dongleID:   "abc123",
			mockDevice: testDevice,
			wantStatus: http.StatusOK,
		},
		{
			name:           "device not found",
			dongleID:       "unknown",
			mockDevice:     nil,
			mockErr:        pgx.ErrNoRows,
			wantStatus:     http.StatusNotFound,
			wantErrMessage: "device not found",
		},
		{
			name:           "database error",
			dongleID:       "abc123",
			mockDevice:     nil,
			mockErr:        fmt.Errorf("connection refused"),
			wantStatus:     http.StatusInternalServerError,
			wantErrMessage: "failed to retrieve device",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDBTX{device: tt.mockDevice, err: tt.mockErr}
			handler := newDeviceHandler(mock)

			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/v1.1/devices/"+tt.dongleID+"/", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id")
			c.SetParamValues(tt.dongleID)
			c.Set(middleware.ContextKeyDongleID, tt.dongleID)

			err := handler.GetDevice(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantStatus == http.StatusOK {
				var resp deviceResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.DongleID != tt.mockDevice.DongleID {
					t.Errorf("dongle_id = %q, want %q", resp.DongleID, tt.mockDevice.DongleID)
				}
				if resp.Serial != tt.mockDevice.Serial.String {
					t.Errorf("serial = %q, want %q", resp.Serial, tt.mockDevice.Serial.String)
				}
				if resp.PublicKey != tt.mockDevice.PublicKey.String {
					t.Errorf("public_key = %q, want %q", resp.PublicKey, tt.mockDevice.PublicKey.String)
				}
				if resp.CreatedAt.IsZero() {
					t.Error("expected non-zero created_at")
				}
				if resp.UpdatedAt.IsZero() {
					t.Error("expected non-zero updated_at")
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
				if errResp.Code != tt.wantStatus {
					t.Errorf("error code = %d, want %d", errResp.Code, tt.wantStatus)
				}
			}
		})
	}
}

func TestGetDeviceWithAuth(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	testDevice := newTestDevice("abc123", "SERIAL001", pubPEM)

	validToken := signDeviceJWT(t, priv, "abc123")

	tests := []struct {
		name       string
		authHeader string
		mockDevice *db.Device
		mockErr    error
		wantStatus int
	}{
		{
			name:       "authenticated request succeeds",
			authHeader: "JWT " + validToken,
			mockDevice: testDevice,
			wantStatus: http.StatusOK,
		},
		{
			name:       "bearer scheme also accepted",
			authHeader: "Bearer " + validToken,
			mockDevice: testDevice,
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing auth returns 401",
			authHeader: "",
			mockDevice: testDevice,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid token returns 401",
			authHeader: "JWT invalid.token.here",
			mockDevice: testDevice,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDBTX{device: tt.mockDevice, err: tt.mockErr}
			handler := newDeviceHandler(mock)

			e := echo.New()
			g := e.Group("/v1.1", middleware.JWTAuthFromDB(db.New(mock)))
			handler.RegisterRoutes(g)

			req := httptest.NewRequest(http.MethodGet, "/v1.1/devices/abc123/", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			e.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantStatus == http.StatusUnauthorized {
				var errResp errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to unmarshal error response: %v", err)
				}
				if errResp.Code != http.StatusUnauthorized {
					t.Errorf("error code = %d, want %d", errResp.Code, http.StatusUnauthorized)
				}
				if errResp.Error == "" {
					t.Error("expected non-empty error message")
				}
			}

			if tt.wantStatus == http.StatusOK {
				var resp deviceResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.DongleID != "abc123" {
					t.Errorf("dongle_id = %q, want %q", resp.DongleID, "abc123")
				}
			}
		})
	}
}

func TestDeviceRegisterRoutes(t *testing.T) {
	mock := &mockDBTX{
		device: newTestDevice("abc123", "S001", "pubkey"),
	}
	handler := newDeviceHandler(mock)

	e := echo.New()
	g := e.Group("/v1.1")
	handler.RegisterRoutes(g)

	routes := e.Routes()
	found := false
	for _, r := range routes {
		if r.Path == "/v1.1/devices/:dongle_id/" && r.Method == http.MethodGet {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected route GET /v1.1/devices/:dongle_id/ to be registered")
	}
}
