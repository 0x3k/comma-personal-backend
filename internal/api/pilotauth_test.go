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

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
)

const testJWTSecret = "test-secret-key-for-unit-tests"

// mockDBTX implements db.DBTX for testing. It returns a mockRow from QueryRow
// that yields the device fields passed at construction time.
type mockDBTX struct {
	device *db.Device
	err    error
}

func (m *mockDBTX) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *mockDBTX) Query(_ context.Context, _ string, _ ...interface{}) (pgx.Rows, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockDBTX) QueryRow(_ context.Context, _ string, _ ...interface{}) pgx.Row {
	return &mockRow{device: m.device, err: m.err}
}

// mockRow implements pgx.Row. It scans the mock device fields into the
// destination pointers in the same order sqlc generates.
type mockRow struct {
	device *db.Device
	err    error
}

func (r *mockRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if r.device == nil {
		return fmt.Errorf("no device")
	}
	if len(dest) < 5 {
		return fmt.Errorf("expected 5 scan destinations, got %d", len(dest))
	}

	*dest[0].(*string) = r.device.DongleID
	*dest[1].(*pgtype.Text) = r.device.Serial
	*dest[2].(*pgtype.Text) = r.device.PublicKey
	*dest[3].(*pgtype.Timestamptz) = r.device.CreatedAt
	*dest[4].(*pgtype.Timestamptz) = r.device.UpdatedAt

	return nil
}

func newTestHandler(mock *mockDBTX) *PilotAuthHandler {
	queries := db.New(mock)
	cfg := &config.Config{JWTSecret: testJWTSecret}
	return NewPilotAuthHandler(queries, testJWTSecret, cfg)
}

func newTestDevice(dongleID, serial, publicKey string) *db.Device {
	now := time.Now()
	return &db.Device{
		DongleID:  dongleID,
		Serial:    pgtype.Text{String: serial, Valid: serial != ""},
		PublicKey: pgtype.Text{String: publicKey, Valid: publicKey != ""},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func TestPilotAuth(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		contentType    string
		mockDevice     *db.Device
		mockErr        error
		wantStatus     int
		wantToken      bool
		wantErrMessage string
	}{
		{
			name:        "successful registration",
			body:        `{"dongle_id":"abc123","public_key":"-----BEGIN PUBLIC KEY-----\ntest\n-----END PUBLIC KEY-----","serial":"SERIAL001"}`,
			contentType: "application/json",
			mockDevice:  newTestDevice("abc123", "SERIAL001", "-----BEGIN PUBLIC KEY-----\ntest\n-----END PUBLIC KEY-----"),
			wantStatus:  http.StatusOK,
			wantToken:   true,
		},
		{
			name:        "registration without serial",
			body:        `{"dongle_id":"abc123","public_key":"-----BEGIN PUBLIC KEY-----\ntest\n-----END PUBLIC KEY-----"}`,
			contentType: "application/json",
			mockDevice:  newTestDevice("abc123", "", "-----BEGIN PUBLIC KEY-----\ntest\n-----END PUBLIC KEY-----"),
			wantStatus:  http.StatusOK,
			wantToken:   true,
		},
		{
			name:        "duplicate device upsert",
			body:        `{"dongle_id":"existing123","public_key":"-----BEGIN PUBLIC KEY-----\nnewkey\n-----END PUBLIC KEY-----","serial":"SERIAL002"}`,
			contentType: "application/json",
			mockDevice:  newTestDevice("existing123", "SERIAL002", "-----BEGIN PUBLIC KEY-----\nnewkey\n-----END PUBLIC KEY-----"),
			wantStatus:  http.StatusOK,
			wantToken:   true,
		},
		{
			name:           "missing dongle_id",
			body:           `{"public_key":"-----BEGIN PUBLIC KEY-----\ntest\n-----END PUBLIC KEY-----"}`,
			contentType:    "application/json",
			wantStatus:     http.StatusBadRequest,
			wantErrMessage: "dongle_id is required",
		},
		{
			name:           "missing public_key",
			body:           `{"dongle_id":"abc123"}`,
			contentType:    "application/json",
			wantStatus:     http.StatusBadRequest,
			wantErrMessage: "public_key is required",
		},
		{
			name:           "empty body",
			body:           `{}`,
			contentType:    "application/json",
			wantStatus:     http.StatusBadRequest,
			wantErrMessage: "dongle_id is required",
		},
		{
			name:           "database error",
			body:           `{"dongle_id":"abc123","public_key":"-----BEGIN PUBLIC KEY-----\ntest\n-----END PUBLIC KEY-----"}`,
			contentType:    "application/json",
			mockDevice:     nil,
			mockErr:        fmt.Errorf("connection refused"),
			wantStatus:     http.StatusInternalServerError,
			wantErrMessage: "failed to register device",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockDBTX{device: tt.mockDevice, err: tt.mockErr}
			handler := newTestHandler(mock)

			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/v2/pilotauth/", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			err := handler.Handle(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantToken {
				var resp pilotAuthResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.Token == "" {
					t.Error("expected non-empty token")
				}

				// Verify the token is valid and contains the dongle_id.
				parsed, err := jwt.Parse(resp.Token, func(token *jwt.Token) (interface{}, error) {
					if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
						return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
					}
					return []byte(testJWTSecret), nil
				})
				if err != nil {
					t.Fatalf("failed to parse token: %v", err)
				}

				claims, ok := parsed.Claims.(jwt.MapClaims)
				if !ok {
					t.Fatal("failed to cast claims")
				}

				dongleID, ok := claims["dongle_id"].(string)
				if !ok || dongleID == "" {
					t.Error("token missing dongle_id claim")
				}
				if dongleID != tt.mockDevice.DongleID {
					t.Errorf("dongle_id = %q, want %q", dongleID, tt.mockDevice.DongleID)
				}

				identity, ok := claims["identity"].(string)
				if !ok || identity != dongleID {
					t.Errorf("identity claim = %q, want %q", identity, dongleID)
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

func TestPilotAuthTokenExpiry(t *testing.T) {
	mock := &mockDBTX{
		device: newTestDevice("abc123", "S001", "pubkey"),
	}
	handler := newTestHandler(mock)

	e := echo.New()
	body := `{"dongle_id":"abc123","public_key":"pubkey","serial":"S001"}`
	req := httptest.NewRequest(http.MethodPost, "/v2/pilotauth/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var resp pilotAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	parsed, err := jwt.Parse(resp.Token, func(token *jwt.Token) (interface{}, error) {
		return []byte(testJWTSecret), nil
	})
	if err != nil {
		t.Fatalf("failed to parse token: %v", err)
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("failed to cast claims")
	}

	exp, err := claims.GetExpirationTime()
	if err != nil {
		t.Fatalf("failed to get expiration: %v", err)
	}

	// Token should expire roughly 90 days from now.
	expectedExp := time.Now().Add(90 * 24 * time.Hour)
	diff := exp.Time.Sub(expectedExp)
	if diff < -time.Minute || diff > time.Minute {
		t.Errorf("token expiry = %v, want approximately %v", exp.Time, expectedExp)
	}
}

func TestRegisterRoutes(t *testing.T) {
	mock := &mockDBTX{
		device: newTestDevice("abc123", "S001", "pubkey"),
	}
	handler := newTestHandler(mock)

	e := echo.New()
	handler.RegisterRoutes(e)

	routes := e.Routes()
	found := false
	for _, r := range routes {
		if r.Path == "/v2/pilotauth/" && r.Method == http.MethodPost {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected route POST /v2/pilotauth/ to be registered")
	}
}
