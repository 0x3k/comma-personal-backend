package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// stubUsageReporter is a test double for UsageReporter. It records how many
// times Usage was called and whether forceRefresh was requested, so tests can
// assert on cache semantics without walking a real filesystem.
type stubUsageReporter struct {
	report           *storage.UsageReport
	err              error
	calls            atomic.Int32
	lastForceRefresh bool
}

func (s *stubUsageReporter) Usage(_ context.Context, forceRefresh bool) (*storage.UsageReport, error) {
	s.calls.Add(1)
	s.lastForceRefresh = forceRefresh
	if s.err != nil {
		return nil, s.err
	}
	// Return a copy so a test mutating the response body cannot poison the
	// stored fixture.
	cp := *s.report
	cp.Devices = append([]storage.DeviceUsage(nil), s.report.Devices...)
	return &cp, nil
}

func sampleUsageReport() *storage.UsageReport {
	return &storage.UsageReport{
		Devices: []storage.DeviceUsage{
			{DongleID: "alpha", Bytes: 1024, RouteCount: 2},
			{DongleID: "beta", Bytes: 512, RouteCount: 1},
		},
		TotalBytes:               1536,
		FilesystemTotalBytes:     1000000,
		FilesystemAvailableBytes: 500000,
		ComputedAt:               time.Unix(1700000000, 0).UTC(),
	}
}

func TestGetUsageReturnsReport(t *testing.T) {
	stub := &stubUsageReporter{report: sampleUsageReport()}
	handler := NewStorageHandler(stub)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/storage/usage", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(middleware.ContextKeyDongleID, "alpha")

	if err := handler.GetUsage(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body storage.UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v; raw=%s", err, rec.Body.String())
	}
	if body.TotalBytes != 1536 {
		t.Errorf("totalBytes = %d, want 1536", body.TotalBytes)
	}
	if len(body.Devices) != 2 {
		t.Fatalf("len(devices) = %d, want 2", len(body.Devices))
	}
	if body.Devices[0].DongleID != "alpha" {
		t.Errorf("devices[0].dongleId = %q, want alpha", body.Devices[0].DongleID)
	}
	if body.Devices[0].Bytes != 1024 {
		t.Errorf("devices[0].bytes = %d, want 1024", body.Devices[0].Bytes)
	}
	if body.Devices[0].RouteCount != 2 {
		t.Errorf("devices[0].routeCount = %d, want 2", body.Devices[0].RouteCount)
	}
	if body.FilesystemTotalBytes != 1000000 {
		t.Errorf("filesystemTotalBytes = %d, want 1000000", body.FilesystemTotalBytes)
	}
	if body.FilesystemAvailableBytes != 500000 {
		t.Errorf("filesystemAvailableBytes = %d, want 500000", body.FilesystemAvailableBytes)
	}

	if stub.calls.Load() != 1 {
		t.Errorf("Usage called %d times, want 1", stub.calls.Load())
	}
	if stub.lastForceRefresh {
		t.Error("lastForceRefresh = true without ?refresh=1, want false")
	}
}

func TestGetUsageHonorsRefreshQuery(t *testing.T) {
	cases := []struct {
		name        string
		query       string
		wantRefresh bool
	}{
		{name: "no query", query: "", wantRefresh: false},
		{name: "refresh=1", query: "?refresh=1", wantRefresh: true},
		{name: "refresh=true", query: "?refresh=true", wantRefresh: true},
		{name: "refresh=yes", query: "?refresh=yes", wantRefresh: true},
		{name: "refresh=0", query: "?refresh=0", wantRefresh: false},
		{name: "refresh=false", query: "?refresh=false", wantRefresh: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubUsageReporter{report: sampleUsageReport()}
			handler := NewStorageHandler(stub)

			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/v1/storage/usage"+tc.query, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.Set(middleware.ContextKeyDongleID, "alpha")

			if err := handler.GetUsage(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if stub.lastForceRefresh != tc.wantRefresh {
				t.Errorf("forceRefresh = %v, want %v", stub.lastForceRefresh, tc.wantRefresh)
			}
		})
	}
}

func TestGetUsageReturns500OnError(t *testing.T) {
	stub := &stubUsageReporter{err: fmt.Errorf("statfs failed")}
	handler := NewStorageHandler(stub)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/storage/usage", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(middleware.ContextKeyDongleID, "alpha")

	if err := handler.GetUsage(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error body: %v", err)
	}
	if !strings.Contains(body.Error, "usage") {
		t.Errorf("error body = %q, want it to mention 'usage'", body.Error)
	}
	if body.Code != http.StatusInternalServerError {
		t.Errorf("error code = %d, want 500", body.Code)
	}
}

func TestStorageHandlerRegisterRoutes(t *testing.T) {
	stub := &stubUsageReporter{report: sampleUsageReport()}
	handler := NewStorageHandler(stub)

	e := echo.New()
	g := e.Group("/v1")
	handler.RegisterRoutes(g)

	routes := e.Routes()
	wantPaths := map[string]bool{
		"/v1/storage/usage":  false,
		"/v1/storage/usage/": false,
	}
	for _, r := range routes {
		if r.Method == http.MethodGet {
			if _, ok := wantPaths[r.Path]; ok {
				wantPaths[r.Path] = true
			}
		}
	}
	for path, found := range wantPaths {
		if !found {
			t.Errorf("expected route GET %s to be registered", path)
		}
	}
}

// TestGetUsageEndToEndWithRealStorage exercises the full pipeline: JWT auth
// middleware -> storage handler -> real storage.Storage walking a tmp
// STORAGE_PATH. This covers the auth-required criterion without touching
// the database (pilotauth's device row is provided by an in-memory mock).
func TestGetUsageEndToEndWithRealStorage(t *testing.T) {
	base := t.TempDir()

	// Fabricate one device with one route and one file.
	dongleID := "e2edevice"
	routeDir := filepath.Join(base, dongleID, "2024-07-04--09-00-00", "0")
	if err := os.MkdirAll(routeDir, 0o755); err != nil {
		t.Fatalf("failed to create route dir: %v", err)
	}
	content := []byte("end-to-end usage test payload")
	if err := os.WriteFile(filepath.Join(routeDir, "rlog"), content, 0o644); err != nil {
		t.Fatalf("failed to write rlog: %v", err)
	}

	store := storage.New(base)

	priv, pubPEM := testDeviceKey(t)
	device := newTestDevice(dongleID, "SERIAL0001", pubPEM)
	mock := &mockDBTX{device: device}
	queries := db.New(mock)

	handler := NewStorageHandler(store)

	e := echo.New()
	// Apply the real JWT auth middleware used in main.go so the test
	// exercises the auth path end-to-end.
	g := e.Group("/v1", middleware.JWTAuthFromDB(queries))
	handler.RegisterRoutes(g)

	tokenString := signDeviceJWT(t, priv, dongleID)

	t.Run("authorized request returns the report", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/storage/usage", nil)
		req.Header.Set("Authorization", "JWT "+tokenString)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var body storage.UsageReport
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse body: %v; raw=%s", err, rec.Body.String())
		}
		if len(body.Devices) != 1 {
			t.Fatalf("len(devices) = %d, want 1", len(body.Devices))
		}
		got := body.Devices[0]
		if got.DongleID != dongleID {
			t.Errorf("dongleId = %q, want %q", got.DongleID, dongleID)
		}
		if got.Bytes != int64(len(content)) {
			t.Errorf("bytes = %d, want %d", got.Bytes, len(content))
		}
		if got.RouteCount != 1 {
			t.Errorf("routeCount = %d, want 1", got.RouteCount)
		}
		if body.TotalBytes != int64(len(content)) {
			t.Errorf("totalBytes = %d, want %d", body.TotalBytes, len(content))
		}
		if body.FilesystemTotalBytes == 0 {
			t.Error("filesystemTotalBytes = 0, want > 0 from statfs")
		}
	})

	t.Run("missing auth token returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/storage/usage", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("refresh=1 still reaches the handler", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/storage/usage?refresh=1", nil)
		req.Header.Set("Authorization", "JWT "+tokenString)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})
}
