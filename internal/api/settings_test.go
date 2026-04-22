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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// fakeSettingsQuerier implements settings.Querier for these tests. It keeps
// a single stored value because the retention endpoints only touch one key.
type fakeSettingsQuerier struct {
	value      string
	hasValue   bool
	getErr     error
	upsertErr  error
	lastSetKey string
	lastSetVal string
}

func (f *fakeSettingsQuerier) GetSetting(_ context.Context, key string) (db.Setting, error) {
	if f.getErr != nil {
		return db.Setting{}, f.getErr
	}
	if !f.hasValue {
		return db.Setting{}, pgx.ErrNoRows
	}
	return db.Setting{
		Key:       key,
		Value:     f.value,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}, nil
}

func (f *fakeSettingsQuerier) UpsertSetting(_ context.Context, arg db.UpsertSettingParams) (db.Setting, error) {
	if f.upsertErr != nil {
		return db.Setting{}, f.upsertErr
	}
	f.value = arg.Value
	f.hasValue = true
	f.lastSetKey = arg.Key
	f.lastSetVal = arg.Value
	return db.Setting{
		Key:       arg.Key,
		Value:     arg.Value,
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}, nil
}

func (f *fakeSettingsQuerier) InsertSettingIfMissing(_ context.Context, arg db.InsertSettingIfMissingParams) error {
	if f.hasValue {
		return nil
	}
	f.value = arg.Value
	f.hasValue = true
	return nil
}

func newSettingsHandlerForTest(q *fakeSettingsQuerier, envDefault int) *SettingsHandler {
	return NewSettingsHandler(settings.New(q), envDefault)
}

func TestGetRetention_StoredValue(t *testing.T) {
	q := &fakeSettingsQuerier{value: "14", hasValue: true}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/retention", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.GetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp retentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.RetentionDays != 14 {
		t.Errorf("retention_days = %d, want 14", resp.RetentionDays)
	}
}

func TestGetRetention_FallsBackToEnvWhenUnset(t *testing.T) {
	q := &fakeSettingsQuerier{} // no stored value
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/retention", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.GetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp retentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.RetentionDays != 30 {
		t.Errorf("retention_days = %d, want 30 (env fallback)", resp.RetentionDays)
	}
}

func TestGetRetention_DatabaseError(t *testing.T) {
	q := &fakeSettingsQuerier{getErr: fmt.Errorf("boom")}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/settings/retention", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.GetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestSetRetention_Success(t *testing.T) {
	q := &fakeSettingsQuerier{}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/retention",
		strings.NewReader(`{"retention_days":7}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.SetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp retentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.RetentionDays != 7 {
		t.Errorf("retention_days = %d, want 7", resp.RetentionDays)
	}
	if q.lastSetKey != settings.KeyRetentionDays {
		t.Errorf("stored key = %q, want %q", q.lastSetKey, settings.KeyRetentionDays)
	}
	if q.lastSetVal != "7" {
		t.Errorf("stored value = %q, want %q", q.lastSetVal, "7")
	}
}

func TestSetRetention_ZeroIsAllowed(t *testing.T) {
	q := &fakeSettingsQuerier{}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/retention",
		strings.NewReader(`{"retention_days":0}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.SetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if q.lastSetVal != "0" {
		t.Errorf("stored value = %q, want %q", q.lastSetVal, "0")
	}
}

func TestSetRetention_NegativeRejected(t *testing.T) {
	q := &fakeSettingsQuerier{}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/retention",
		strings.NewReader(`{"retention_days":-1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.SetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if q.hasValue {
		t.Error("store was written despite validation failure")
	}
}

func TestSetRetention_MissingFieldRejected(t *testing.T) {
	q := &fakeSettingsQuerier{}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/retention",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.SetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSetRetention_InvalidJSONRejected(t *testing.T) {
	q := &fakeSettingsQuerier{}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/retention",
		strings.NewReader(`{"retention_days":"oops"`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.SetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestSetRetention_DatabaseError(t *testing.T) {
	q := &fakeSettingsQuerier{upsertErr: fmt.Errorf("boom")}
	handler := newSettingsHandlerForTest(q, 30)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/v1/settings/retention",
		strings.NewReader(`{"retention_days":5}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.SetRetention(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestSettingsRegisterRoutes(t *testing.T) {
	q := &fakeSettingsQuerier{}
	handler := newSettingsHandlerForTest(q, 0)

	e := echo.New()
	g := e.Group("/v1")
	handler.RegisterRoutes(g)

	expected := map[string]bool{
		"GET /v1/settings/retention": true,
		"PUT /v1/settings/retention": true,
	}
	for _, r := range e.Routes() {
		delete(expected, r.Method+" "+r.Path)
	}
	for route := range expected {
		t.Errorf("expected route %s to be registered", route)
	}
}

// TestSettingsWithJWTAuth verifies the retention endpoints are gated by the
// shared JWT middleware. A missing token must produce 401, a valid token
// must reach the handler.
func TestSettingsWithJWTAuth(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	validToken := signDeviceJWT(t, priv, "abc123")

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "authenticated GET succeeds",
			authHeader: "JWT " + validToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing auth returns 401",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "invalid token returns 401",
			authHeader: "JWT bad.token.here",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The JWT middleware shares the db.Queries that backs device
			// lookup. Pair it with a mock that returns the device record
			// matching the signed token.
			mock := &mockDBTX{device: newTestDevice("abc123", "SERIAL001", pubPEM)}
			queries := db.New(mock)

			q := &fakeSettingsQuerier{}
			handler := newSettingsHandlerForTest(q, 0)

			e := echo.New()
			g := e.Group("/v1", middleware.JWTAuthFromDB(queries))
			handler.RegisterRoutes(g)

			req := httptest.NewRequest(http.MethodGet, "/v1/settings/retention", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()

			e.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s",
					rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}
