package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/storage"
	"comma-personal-backend/internal/worker"
)

// newThumbnailRequest builds an Echo context targeting the thumbnail
// endpoint. authMode selects how the request authenticated; pass
// middleware.AuthModeSession to impersonate an operator, or pass an
// empty string with a matching authDongleID to impersonate a device JWT.
func newThumbnailRequest(t *testing.T, store *storage.Storage, dongleID, routeName, authDongleID, authMode string) (*httptest.ResponseRecorder, echo.Context, *ThumbnailHandler) {
	t.Helper()

	handler := NewThumbnailHandler(store)

	e := echo.New()
	target := fmt.Sprintf("/v1/routes/%s/%s/thumbnail", dongleID, routeName)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongleID, routeName)
	if authMode != "" {
		c.Set(middleware.ContextKeyAuthMode, authMode)
	}
	c.Set(middleware.ContextKeyDongleID, authDongleID)

	return rec, c, handler
}

func writeThumbnail(t *testing.T, tmp, dongleID, route, segment string, data []byte) {
	t.Helper()
	dir := filepath.Join(tmp, dongleID, route, segment)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, worker.ThumbnailFileName)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write thumbnail: %v", err)
	}
}

func TestThumbnailHandler_ServesJPEGWithHeaders(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)

	dongle := "abc123"
	route := "2024-01-15--12-30-00"
	body := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}
	writeThumbnail(t, tmp, dongle, route, "0", body)

	rec, c, handler := newThumbnailRequest(t, store, dongle, route, dongle, "")

	if err := handler.GetThumbnail(c); err != nil {
		t.Fatalf("GetThumbnail returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want 'public, max-age=86400'", got)
	}
	if etag := rec.Header().Get("ETag"); etag == "" {
		t.Error("ETag header missing")
	}
	if rec.Body.Len() != len(body) {
		t.Errorf("body length = %d, want %d", rec.Body.Len(), len(body))
	}
}

func TestThumbnailHandler_404WhenMissing(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)

	rec, c, handler := newThumbnailRequest(t, store, "abc", "route", "abc", "")
	if err := handler.GetThumbnail(c); err != nil {
		t.Fatalf("GetThumbnail returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestThumbnailHandler_FallsBackToLowestSegment(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)

	dongle := "abc"
	route := "r1"
	// Only segment 2 has a thumbnail.
	writeThumbnail(t, tmp, dongle, route, "2", []byte{0xff, 0xd8})
	writeThumbnail(t, tmp, dongle, route, "7", []byte{0xff, 0xd8})

	rec, c, handler := newThumbnailRequest(t, store, dongle, route, dongle, "")
	if err := handler.GetThumbnail(c); err != nil {
		t.Fatalf("GetThumbnail returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestThumbnailHandler_IfNoneMatchReturns304(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)

	dongle := "abc"
	route := "r1"
	writeThumbnail(t, tmp, dongle, route, "0", []byte{0xff, 0xd8})

	// First call to capture the ETag.
	rec1, c1, handler := newThumbnailRequest(t, store, dongle, route, dongle, "")
	if err := handler.GetThumbnail(c1); err != nil {
		t.Fatalf("GetThumbnail call 1 error: %v", err)
	}
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on initial response")
	}

	// Second call with If-None-Match must return 304.
	rec2, c2, _ := newThumbnailRequest(t, store, dongle, route, dongle, "")
	c2.Request().Header.Set("If-None-Match", etag)
	if err := handler.GetThumbnail(c2); err != nil {
		t.Fatalf("GetThumbnail call 2 error: %v", err)
	}
	if rec2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec2.Code)
	}
}

func TestThumbnailHandler_ForbidsCrossDeviceJWT(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)

	dongle := "target"
	route := "r1"
	writeThumbnail(t, tmp, dongle, route, "0", []byte{0xff, 0xd8})

	// Auth dongle is a different device; mode is empty (JWT semantics).
	rec, c, handler := newThumbnailRequest(t, store, dongle, route, "attacker", "")
	if err := handler.GetThumbnail(c); err != nil {
		t.Fatalf("GetThumbnail error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestThumbnailHandler_AllowsSessionOperator(t *testing.T) {
	tmp := t.TempDir()
	store := storage.New(tmp)

	dongle := "somedevice"
	route := "r1"
	writeThumbnail(t, tmp, dongle, route, "0", []byte{0xff, 0xd8})

	// Session operator with no matching device JWT must be allowed.
	rec, c, handler := newThumbnailRequest(t, store, dongle, route, "", middleware.AuthModeSession)
	if err := handler.GetThumbnail(c); err != nil {
		t.Fatalf("GetThumbnail error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
