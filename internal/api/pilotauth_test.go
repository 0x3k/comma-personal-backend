package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// pilotAuthMockDB dispatches QueryRow between four query shapes used by the
// pilotauth handler so tests can exercise the comma and sunnylink branches
// without spinning up a real database:
//
//   - SELECT ... WHERE public_key = $1  (comma flow lookup)
//   - SELECT ... WHERE dongle_id  = $1  (sunnylink flow lookup)
//   - INSERT INTO devices ...           (comma flow insert)
//   - UPDATE devices SET sunnylink_*    (sunnylink flow update)
//
// The simpler mockDBTX in helpers_test.go handles tests that exercise only
// one query at a time.
type pilotAuthMockDB struct {
	// existingDevice is returned from SELECT WHERE public_key lookups. If
	// nil, that SELECT yields pgx.ErrNoRows.
	existingDevice *db.Device
	// newDevice is returned from INSERT with its DongleID overwritten by
	// capturedInsertDongleID, so the handler's response reflects the
	// server-generated ID.
	newDevice *db.Device
	// selectErr, if non-nil, is returned from SELECT queries instead of a
	// row. Used to exercise non-ErrNoRows lookup failures.
	selectErr error
	// insertErr, if non-nil, is returned from INSERT queries.
	insertErr error
	// capturedInsertDongleID records the dongle_id the handler generated
	// and passed to INSERT, so tests can assert on it.
	capturedInsertDongleID string

	// Sunnylink fields. existingByCommaDongleID is returned from SELECT
	// WHERE dongle_id lookups (the lookup at the start of the sunnylink
	// branch). updatedDevice is returned from the UPDATE that links the
	// sunnylink identity onto the existing comma row.
	existingByCommaDongleID    *db.Device
	updatedDevice              *db.Device
	updateErr                  error
	capturedUpdateCommaDongle  string
	capturedUpdateSunnylinkID  string
	capturedUpdateSunnylinkKey string
}

func (m *pilotAuthMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *pilotAuthMockDB) Query(_ context.Context, _ string, _ ...interface{}) (pgx.Rows, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *pilotAuthMockDB) QueryRow(_ context.Context, sqlText string, args ...interface{}) pgx.Row {
	// sqlc prefixes each query with a `-- name: ...` comment, so prefix
	// matching on trimmed text breaks. Test the body substring instead.
	switch {
	case strings.Contains(sqlText, "INSERT INTO"):
		if len(args) > 0 {
			if s, ok := args[0].(string); ok {
				m.capturedInsertDongleID = s
			}
		}
		if m.insertErr != nil {
			return &mockRow{err: m.insertErr}
		}
		device := *m.newDevice
		device.DongleID = m.capturedInsertDongleID
		return &mockRow{device: &device}

	case strings.Contains(sqlText, "UPDATE devices"):
		if len(args) >= 3 {
			if s, ok := args[0].(string); ok {
				m.capturedUpdateCommaDongle = s
			}
			if t, ok := args[1].(pgtype.Text); ok && t.Valid {
				m.capturedUpdateSunnylinkID = t.String
			}
			if t, ok := args[2].(pgtype.Text); ok && t.Valid {
				m.capturedUpdateSunnylinkKey = t.String
			}
		}
		if m.updateErr != nil {
			return &mockRow{err: m.updateErr}
		}
		if m.updatedDevice != nil {
			return &mockRow{device: m.updatedDevice}
		}
		// Synthesize an updated device from the captured args so tests don't
		// have to hand-build the response shape.
		device := db.Device{
			DongleID: m.capturedUpdateCommaDongle,
			SunnylinkDongleID: pgtype.Text{
				String: m.capturedUpdateSunnylinkID,
				Valid:  true,
			},
			SunnylinkPublicKey: pgtype.Text{
				String: m.capturedUpdateSunnylinkKey,
				Valid:  true,
			},
		}
		return &mockRow{device: &device}

	case strings.Contains(sqlText, "WHERE dongle_id"):
		if m.selectErr != nil {
			return &mockRow{err: m.selectErr}
		}
		if m.existingByCommaDongleID == nil {
			return &mockRow{err: pgx.ErrNoRows}
		}
		return &mockRow{device: m.existingByCommaDongleID}

	default: // SELECT WHERE public_key (the comma lookup)
		if m.selectErr != nil {
			return &mockRow{err: m.selectErr}
		}
		if m.existingDevice == nil {
			return &mockRow{err: pgx.ErrNoRows}
		}
		return &mockRow{device: m.existingDevice}
	}
}

func newTestHandler(mock *pilotAuthMockDB, cfg *config.Config) *PilotAuthHandler {
	if cfg == nil {
		cfg = &config.Config{}
	}
	queries := db.New(mock)
	return NewPilotAuthHandler(queries, cfg)
}

// generateTestKeypair returns an RSA keypair and the PEM-encoded public key.
func generateTestKeypair(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes})
	return priv, string(pubPEM)
}

// signRegisterToken produces a JWT matching openpilot's registration.py
// payload: {register: true, exp: now+1h}.
func signRegisterToken(t *testing.T, priv *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}
	return signed
}

func defaultRegisterClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"register": true,
		"exp":      time.Now().Add(1 * time.Hour).Unix(),
	}
}

// newPilotAuthRequest builds a POST with query params, matching openpilot's
// requests.request(method='POST', params=...) behavior.
func newPilotAuthRequest(params url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v2/pilotauth/?"+params.Encode(), nil)
	return req
}

func newDeviceFixture(dongleID, serial, publicKey string) *db.Device {
	now := time.Now()
	return &db.Device{
		DongleID:  dongleID,
		Serial:    pgtype.Text{String: serial, Valid: serial != ""},
		PublicKey: pgtype.Text{String: publicKey, Valid: publicKey != ""},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func TestPilotAuth_NewRegistration(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	registerToken := signRegisterToken(t, priv, defaultRegisterClaims())

	mock := &pilotAuthMockDB{
		newDevice: newDeviceFixture("", "SERIAL001", pubPEM),
	}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("imei", "111111111111111")
	params.Set("imei2", "222222222222222")
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", registerToken)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp pilotAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.DongleID == "" {
		t.Fatal("response missing dongle_id")
	}
	if len(resp.DongleID) != 16 {
		t.Errorf("dongle_id length = %d, want 16", len(resp.DongleID))
	}
	if resp.DongleID != mock.capturedInsertDongleID {
		t.Errorf("response dongle_id = %q, want inserted %q", resp.DongleID, mock.capturedInsertDongleID)
	}
}

func TestPilotAuth_ExistingDeviceReturnsSameDongleID(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	registerToken := signRegisterToken(t, priv, defaultRegisterClaims())

	existing := newDeviceFixture("abc123def4567890", "SERIAL001", pubPEM)
	mock := &pilotAuthMockDB{existingDevice: existing}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", registerToken)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp pilotAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp.DongleID != existing.DongleID {
		t.Errorf("dongle_id = %q, want existing %q", resp.DongleID, existing.DongleID)
	}
	if mock.capturedInsertDongleID != "" {
		t.Errorf("insert was called when existing device should have been reused (captured dongle_id %q)", mock.capturedInsertDongleID)
	}
}

func TestPilotAuth_ValidationErrors(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	validToken := signRegisterToken(t, priv, defaultRegisterClaims())

	tests := []struct {
		name       string
		params     url.Values
		wantStatus int
		wantErrSub string
	}{
		{
			name: "missing public_key",
			params: url.Values{
				"serial":         {"SERIAL001"},
				"register_token": {validToken},
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "public_key is required",
		},
		{
			name: "missing register_token",
			params: url.Values{
				"serial":     {"SERIAL001"},
				"public_key": {pubPEM},
			},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "register_token is required",
		},
		{
			name:       "empty request",
			params:     url.Values{},
			wantStatus: http.StatusBadRequest,
			wantErrSub: "public_key is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &pilotAuthMockDB{}
			handler := newTestHandler(mock, nil)

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(newPilotAuthRequest(tt.params), rec)
			if err := handler.Handle(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			var errResp errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("failed to unmarshal error response: %v", err)
			}
			if !strings.Contains(errResp.Error, tt.wantErrSub) {
				t.Errorf("error %q does not contain %q", errResp.Error, tt.wantErrSub)
			}
		})
	}
}

func TestPilotAuth_RegisterTokenRejections(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	otherPriv, _ := generateTestKeypair(t)

	tests := []struct {
		name       string
		claims     jwt.MapClaims
		signingKey *rsa.PrivateKey
		wantStatus int
		wantErrSub string
	}{
		{
			name:       "wrong key signature",
			claims:     defaultRegisterClaims(),
			signingKey: otherPriv,
			wantStatus: http.StatusUnauthorized,
			wantErrSub: "register_token",
		},
		{
			name: "register claim false",
			claims: jwt.MapClaims{
				"register": false,
				"exp":      time.Now().Add(1 * time.Hour).Unix(),
			},
			signingKey: priv,
			wantStatus: http.StatusUnauthorized,
			wantErrSub: "register",
		},
		{
			name: "register claim missing",
			claims: jwt.MapClaims{
				"exp": time.Now().Add(1 * time.Hour).Unix(),
			},
			signingKey: priv,
			wantStatus: http.StatusUnauthorized,
			wantErrSub: "register",
		},
		{
			name: "expired token",
			claims: jwt.MapClaims{
				"register": true,
				"exp":      time.Now().Add(-1 * time.Hour).Unix(),
			},
			signingKey: priv,
			wantStatus: http.StatusUnauthorized,
			wantErrSub: "register_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &pilotAuthMockDB{}
			handler := newTestHandler(mock, nil)
			token := signRegisterToken(t, tt.signingKey, tt.claims)

			params := url.Values{}
			params.Set("serial", "SERIAL001")
			params.Set("public_key", pubPEM)
			params.Set("register_token", token)

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(newPilotAuthRequest(params), rec)
			if err := handler.Handle(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			var errResp errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("failed to unmarshal error: %v", err)
			}
			if !strings.Contains(errResp.Error, tt.wantErrSub) {
				t.Errorf("error %q does not contain %q", errResp.Error, tt.wantErrSub)
			}
		})
	}
}

func TestPilotAuth_HMACAlgorithmRejected(t *testing.T) {
	_, pubPEM := generateTestKeypair(t)

	// Sign an HS256 token — should be rejected by the WithValidMethods
	// allowlist.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, defaultRegisterClaims())
	signed, err := token.SignedString([]byte("shared-secret"))
	if err != nil {
		t.Fatalf("failed to sign HS256 token: %v", err)
	}

	mock := &pilotAuthMockDB{}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", signed)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

func TestPilotAuth_SerialAllowlist(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	tests := []struct {
		name       string
		allowed    []string
		serial     string
		wantStatus int
	}{
		{
			name:       "empty allowlist accepts any serial",
			allowed:    nil,
			serial:     "ANY",
			wantStatus: http.StatusOK,
		},
		{
			name:       "serial in allowlist",
			allowed:    []string{"SERIAL001", "SERIAL002"},
			serial:     "SERIAL001",
			wantStatus: http.StatusOK,
		},
		{
			name:       "serial not in allowlist",
			allowed:    []string{"SERIAL001", "SERIAL002"},
			serial:     "EVIL",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &pilotAuthMockDB{
				newDevice: newDeviceFixture("", tt.serial, pubPEM),
			}
			cfg := &config.Config{AllowedSerials: tt.allowed}
			handler := newTestHandler(mock, cfg)

			params := url.Values{}
			params.Set("serial", tt.serial)
			params.Set("public_key", pubPEM)
			params.Set("register_token", token)

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(newPilotAuthRequest(params), rec)
			if err := handler.Handle(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestPilotAuth_DatabaseErrors(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	tests := []struct {
		name       string
		mock       *pilotAuthMockDB
		wantStatus int
	}{
		{
			name:       "select error (non-ErrNoRows)",
			mock:       &pilotAuthMockDB{selectErr: fmt.Errorf("connection refused")},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "insert error",
			mock: &pilotAuthMockDB{
				insertErr: fmt.Errorf("unique violation"),
			},
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newTestHandler(tt.mock, nil)

			params := url.Values{}
			params.Set("serial", "SERIAL001")
			params.Set("public_key", pubPEM)
			params.Set("register_token", token)

			rec := httptest.NewRecorder()
			c := echo.New().NewContext(newPilotAuthRequest(params), rec)
			if err := handler.Handle(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

func TestPilotAuth_DongleIDFormat(t *testing.T) {
	// Run the new-registration path a handful of times and check that every
	// generated dongle_id is 16 lowercase hex chars and unique.
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	seen := make(map[string]bool)
	for i := 0; i < 25; i++ {
		mock := &pilotAuthMockDB{newDevice: newDeviceFixture("", "SERIAL", pubPEM)}
		handler := newTestHandler(mock, nil)

		params := url.Values{}
		params.Set("serial", "SERIAL")
		params.Set("public_key", pubPEM)
		params.Set("register_token", token)

		rec := httptest.NewRecorder()
		c := echo.New().NewContext(newPilotAuthRequest(params), rec)
		if err := handler.Handle(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var resp pilotAuthResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if len(resp.DongleID) != 16 {
			t.Errorf("dongle_id %q length = %d, want 16", resp.DongleID, len(resp.DongleID))
		}
		for _, ch := range resp.DongleID {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
				t.Errorf("dongle_id %q contains non-hex char %q", resp.DongleID, ch)
				break
			}
		}
		if seen[resp.DongleID] {
			t.Errorf("duplicate dongle_id %q generated", resp.DongleID)
		}
		seen[resp.DongleID] = true
	}
}

func TestPilotAuth_SunnylinkRegistration_New(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	commaDongle := "abc123def4567890"
	existing := newDeviceFixture(commaDongle, "SERIAL001", pubPEM)
	mock := &pilotAuthMockDB{existingByCommaDongleID: existing}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("imei", "111111111111111")
	params.Set("imei2", "222222222222222")
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", token)
	params.Set("comma_dongle_id", commaDongle)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp sunnylinkPilotAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal sunnylink response: %v", err)
	}
	if resp.DeviceID == "" {
		t.Fatal("response missing device_id")
	}
	if len(resp.DeviceID) != 16 {
		t.Errorf("sunnylink device_id length = %d, want 16", len(resp.DeviceID))
	}
	if mock.capturedUpdateCommaDongle != commaDongle {
		t.Errorf("update keyed on %q, want %q", mock.capturedUpdateCommaDongle, commaDongle)
	}
	if mock.capturedUpdateSunnylinkID != resp.DeviceID {
		t.Errorf("update sunnylink_id %q, response device_id %q", mock.capturedUpdateSunnylinkID, resp.DeviceID)
	}
	if mock.capturedUpdateSunnylinkKey != pubPEM {
		t.Errorf("update did not record submitted public_key as sunnylink_public_key")
	}

	// The comma response shape must NOT be returned for the sunnylink branch.
	if !strings.Contains(rec.Body.String(), "device_id") {
		t.Errorf("response body %q missing device_id", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"dongle_id"`) {
		t.Errorf("sunnylink response unexpectedly carried dongle_id field: %s", rec.Body.String())
	}
}

func TestPilotAuth_SunnylinkRegistration_AlreadyLinkedIsIdempotent(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	commaDongle := "abc123def4567890"
	priorSunnylinkID := "f0e1d2c3b4a59687"
	existing := newDeviceFixture(commaDongle, "SERIAL001", pubPEM)
	existing.SunnylinkDongleID = pgtype.Text{String: priorSunnylinkID, Valid: true}
	existing.SunnylinkPublicKey = pgtype.Text{String: pubPEM, Valid: true}

	mock := &pilotAuthMockDB{existingByCommaDongleID: existing}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", token)
	params.Set("comma_dongle_id", commaDongle)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp sunnylinkPilotAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if resp.DeviceID != priorSunnylinkID {
		t.Errorf("device_id = %q, want existing %q", resp.DeviceID, priorSunnylinkID)
	}
	if mock.capturedUpdateCommaDongle != "" {
		t.Errorf("UPDATE was called for an already-linked device (captured %q)", mock.capturedUpdateCommaDongle)
	}
}

func TestPilotAuth_SunnylinkRegistration_UnknownCommaDongleReturns404(t *testing.T) {
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	// existingByCommaDongleID stays nil so the SELECT WHERE dongle_id lookup
	// yields pgx.ErrNoRows.
	mock := &pilotAuthMockDB{}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", token)
	params.Set("comma_dongle_id", "0000000000000000")

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestPilotAuth_SunnylinkRegistration_RejectsKeyMismatch(t *testing.T) {
	// Devices presenting a public_key that does not match the comma row's
	// stored public_key are rejected, so an attacker who learns only the
	// comma_dongle_id cannot claim its sunnylink slot.
	priv, pubPEM := generateTestKeypair(t)
	token := signRegisterToken(t, priv, defaultRegisterClaims())

	_, otherPEM := generateTestKeypair(t)

	commaDongle := "abc123def4567890"
	existing := newDeviceFixture(commaDongle, "SERIAL001", otherPEM)
	mock := &pilotAuthMockDB{existingByCommaDongleID: existing}
	handler := newTestHandler(mock, nil)

	params := url.Values{}
	params.Set("serial", "SERIAL001")
	params.Set("public_key", pubPEM)
	params.Set("register_token", token)
	params.Set("comma_dongle_id", commaDongle)

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(newPilotAuthRequest(params), rec)
	if err := handler.Handle(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if mock.capturedUpdateCommaDongle != "" {
		t.Errorf("UPDATE called despite key mismatch (captured %q)", mock.capturedUpdateCommaDongle)
	}
}

func TestRegisterRoutes(t *testing.T) {
	handler := newTestHandler(&pilotAuthMockDB{}, nil)

	e := echo.New()
	handler.RegisterRoutes(e)

	wantPaths := map[string]bool{
		"/v2/pilotauth/": false,
		"/v2/pilotauth":  false,
	}
	for _, r := range e.Routes() {
		if r.Method == http.MethodPost {
			if _, ok := wantPaths[r.Path]; ok {
				wantPaths[r.Path] = true
			}
		}
	}
	for path, found := range wantPaths {
		if !found {
			t.Errorf("expected route POST %s to be registered", path)
		}
	}
}
