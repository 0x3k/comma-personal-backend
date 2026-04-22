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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/share"
	"comma-personal-backend/internal/storage"
)

// shareMockDB implements db.DBTX for the share handler tests. It dispatches
// by the SQL string: GetRoute / GetRouteByID return a canned route, the
// ListSegmentsByRoute path returns canned segments, and the geometry WKT
// lookup returns a canned pgtype.Text.
type shareMockDB struct {
	route       *db.Route
	routeErr    error
	segments    []db.Segment
	segmentsErr error
	geometry    pgtype.Text
	geometryErr error
}

func (m *shareMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *shareMockDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM segments") {
		if m.segmentsErr != nil {
			return nil, m.segmentsErr
		}
		return &mockSegmentRows{segments: m.segments}, nil
	}
	return nil, fmt.Errorf("unexpected Query: %s", sql)
}

func (m *shareMockDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	if strings.Contains(sql, "ST_AsText") {
		return &shareGeometryRow{text: m.geometry, err: m.geometryErr}
	}
	if strings.Contains(sql, "FROM routes") {
		if m.routeErr != nil {
			return &mockRouteRow{err: m.routeErr}
		}
		if m.route == nil {
			return &mockRouteRow{err: pgx.ErrNoRows}
		}
		return &mockRouteRow{route: m.route}
	}
	return &mockRouteRow{err: fmt.Errorf("unexpected QueryRow: %s", sql)}
}

type shareGeometryRow struct {
	text pgtype.Text
	err  error
}

func (r *shareGeometryRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return fmt.Errorf("expected 1 scan destination, got %d", len(dest))
	}
	target, ok := dest[0].(*pgtype.Text)
	if !ok {
		return fmt.Errorf("expected *pgtype.Text, got %T", dest[0])
	}
	*target = r.text
	return nil
}

// newShareTestRoute is a constructor for a db.Route wired with sensible
// defaults the share handler tests use across several cases.
func newShareTestRoute(id int32, dongleID, routeName string) *db.Route {
	now := time.Unix(1_700_000_000, 0)
	return &db.Route{
		ID:        id,
		DongleID:  dongleID,
		RouteName: routeName,
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func TestShareCreateHappyPath(t *testing.T) {
	route := newShareTestRoute(42, "dongle-1", "2024-01-01--12-00-00")
	mock := &shareMockDB{route: route}
	queries := db.New(mock)

	h := NewShareHandler(queries, nil, "topsecret")
	// Freeze time so the test can assert on the exp timestamp.
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	body := strings.NewReader(`{"expires_in_hours": 24}`)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/routes/dongle-1/2024-01-01--12-00-00/share", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues("dongle-1", "2024-01-01--12-00-00")
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)

	if err := h.CreateShare(c); err != nil {
		t.Fatalf("CreateShare returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp createShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("token is empty")
	}
	wantExp := frozen.Add(24 * time.Hour)
	if !resp.ExpiresAt.Equal(wantExp) {
		t.Errorf("expiresAt = %s, want %s", resp.ExpiresAt, wantExp)
	}
	if !strings.Contains(resp.URL, "/share/"+resp.Token) {
		t.Errorf("url = %q, want contains /share/%s", resp.URL, resp.Token)
	}

	// Round-trip the token through the share package to prove the handler
	// signed it with the right secret and the right route ID. We parse at
	// the same frozen "now" the handler used so the expiry is in range.
	gotRouteID, err := share.ParseAt([]byte("topsecret"), resp.Token, frozen)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if gotRouteID != 42 {
		t.Errorf("route_id = %d, want 42", gotRouteID)
	}
}

func TestShareCreateDefaultExpiry(t *testing.T) {
	route := newShareTestRoute(1, "d", "r")
	mock := &shareMockDB{route: route}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	// Empty body exercises the default-hours path.
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/routes/d/r/share", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues("d", "r")
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)

	if err := h.CreateShare(c); err != nil {
		t.Fatalf("CreateShare error: %v", err)
	}
	var resp createShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := frozen.Add(defaultShareExpiresHours * time.Hour)
	if !resp.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %s, want %s (default)", resp.ExpiresAt, want)
	}
}

func TestShareCreateClampsMaxExpiry(t *testing.T) {
	route := newShareTestRoute(1, "d", "r")
	mock := &shareMockDB{route: route}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	body := strings.NewReader(fmt.Sprintf(`{"expires_in_hours": %d}`, maxShareExpiresHours+100))
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/routes/d/r/share", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues("d", "r")
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)

	if err := h.CreateShare(c); err != nil {
		t.Fatalf("CreateShare error: %v", err)
	}
	var resp createShareResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := frozen.Add(maxShareExpiresHours * time.Hour)
	if !resp.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %s, want %s (clamped)", resp.ExpiresAt, want)
	}
}

func TestShareCreateReturns501WhenSecretUnset(t *testing.T) {
	mock := &shareMockDB{route: newShareTestRoute(1, "d", "r")}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "") // empty secret

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/routes/d/r/share", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues("d", "r")
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)

	if err := h.CreateShare(c); err != nil {
		t.Fatalf("CreateShare error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

func TestShareCreateRouteNotFound(t *testing.T) {
	mock := &shareMockDB{routeErr: pgx.ErrNoRows}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "secret")

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/routes/d/missing/share", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues("d", "missing")
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)

	if err := h.CreateShare(c); err != nil {
		t.Fatalf("CreateShare error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestShareGetHappyPath(t *testing.T) {
	// Use a distinctive dongle ID + route name so the body-leak check
	// does not match an incidental substring (e.g. "d" inside base64).
	dongleID := "DONGLEZZZ1234"
	routeName := "2024-01-01--12-00-00"
	route := newShareTestRoute(7, dongleID, routeName)
	mock := &shareMockDB{
		route: route,
		segments: []db.Segment{
			newTestSegment(10, 7, 0),
			newTestSegment(11, 7, 1),
		},
		geometry: pgtype.Text{String: "LINESTRING(-122.4 37.7, -122.41 37.71)", Valid: true},
	}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	tok, err := share.Sign([]byte("secret"), 7, frozen.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/share/"+tok, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues(tok)

	if err := h.GetShare(c); err != nil {
		t.Fatalf("GetShare error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp shareRouteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.RouteName != routeName {
		t.Errorf("routeName = %q, want %q", resp.RouteName, routeName)
	}
	if len(resp.Segments) != 2 {
		t.Errorf("segments = %d, want 2", len(resp.Segments))
	}
	if len(resp.Geometry) != 2 {
		t.Errorf("geometry = %d points, want 2", len(resp.Geometry))
	}
	if resp.Geometry[0][0] != 37.7 || resp.Geometry[0][1] != -122.4 {
		t.Errorf("geometry[0] = %v, want [37.7, -122.4]", resp.Geometry[0])
	}
	wantBase := "/v1/share/" + tok + "/segments"
	if resp.MediaBaseURL != wantBase {
		t.Errorf("mediaBaseUrl = %q, want %q", resp.MediaBaseURL, wantBase)
	}
	// The response must not leak the dongle ID.
	if strings.Contains(rec.Body.String(), dongleID) {
		t.Errorf("response body leaks dongleId %q: %s", dongleID, rec.Body.String())
	}
}

func TestShareGetExpiredReturns410(t *testing.T) {
	mock := &shareMockDB{route: newShareTestRoute(7, "d", "r")}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "secret")
	// "now" is far in the future of the token's expiry.
	h.now = func() time.Time { return time.Unix(2_000_000_000, 0) }

	tok, err := share.Sign([]byte("secret"), 7, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/share/"+tok, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues(tok)

	if err := h.GetShare(c); err != nil {
		t.Fatalf("GetShare error: %v", err)
	}
	if rec.Code != http.StatusGone {
		t.Errorf("status = %d, want 410; body=%s", rec.Code, rec.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(body.Error, "expired") {
		t.Errorf("error = %q, want contains 'expired'", body.Error)
	}
}

func TestShareGetTamperedReturns401(t *testing.T) {
	mock := &shareMockDB{route: newShareTestRoute(7, "d", "r")}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "secret")
	h.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	tok, err := share.Sign([]byte("secret"), 7, time.Unix(1_700_000_000, 0).Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}
	// Flip the last character of the signature segment.
	tampered := tok[:len(tok)-1]
	if tok[len(tok)-1] == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/share/"+tampered, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues(tampered)

	if err := h.GetShare(c); err != nil {
		t.Fatalf("GetShare error: %v", err)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestShareGetSecretUnsetReturns501(t *testing.T) {
	mock := &shareMockDB{route: newShareTestRoute(1, "d", "r")}
	queries := db.New(mock)
	h := NewShareHandler(queries, nil, "")

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/share/anything", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token")
	c.SetParamValues("anything")

	if err := h.GetShare(c); err != nil {
		t.Fatalf("GetShare error: %v", err)
	}
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

// TestShareMediaCrossRouteDenied verifies that a share token minted for
// route A cannot be used to fetch files belonging to route B: the
// handlers resolve the route from the token, not from a path parameter,
// so the underlying storage path never references the attacker-chosen
// route.
func TestShareMediaCrossRouteDenied(t *testing.T) {
	tmpDir := t.TempDir()
	// Two routes live side-by-side on disk. Route A's token must not let
	// the viewer reach files under route B's directory.
	routeAQcamera := filepath.Join(tmpDir, "dongle-1", "route-a", "0", "qcamera.ts")
	routeBQcamera := filepath.Join(tmpDir, "dongle-1", "route-b", "0", "qcamera.ts")
	if err := os.MkdirAll(filepath.Dir(routeAQcamera), 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(routeBQcamera), 0o755); err != nil {
		t.Fatalf("mkdir b: %v", err)
	}
	if err := os.WriteFile(routeAQcamera, []byte("ROUTE-A"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(routeBQcamera, []byte("ROUTE-B"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	// Mock returns route-a when GetRouteByID is called with the id baked
	// into the token. The handler never looks up route-b.
	mock := &shareMockDB{route: newShareTestRoute(100, "dongle-1", "route-a")}
	queries := db.New(mock)
	store := storage.New(tmpDir)
	h := NewShareHandler(queries, store, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	// Token for route-a.
	tok, err := share.Sign([]byte("secret"), 100, frozen.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	e := echo.New()
	// Simulate the router call: path parameters are :token, :segment_num,
	// :file. The file name is a whitelisted artifact.
	req := httptest.NewRequest(http.MethodGet,
		"/v1/share/"+tok+"/segments/0/qcamera.ts", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token", "segment_num", "file")
	c.SetParamValues(tok, "0", "qcamera.ts")

	if err := h.GetShareMedia(c); err != nil {
		t.Fatalf("GetShareMedia error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ROUTE-A") {
		t.Errorf("expected route-a content, got %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ROUTE-B") {
		t.Errorf("response leaked route-b content: %q", rec.Body.String())
	}
}

func TestShareMediaRejectsBlockedFile(t *testing.T) {
	tmpDir := t.TempDir()
	mock := &shareMockDB{route: newShareTestRoute(1, "d", "r")}
	queries := db.New(mock)
	store := storage.New(tmpDir)
	h := NewShareHandler(queries, store, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	tok, err := share.Sign([]byte("secret"), 1, frozen.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	// rlog.bz2 is in shareMediaFiles but marked false -- it must not be
	// served.
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/share/"+tok+"/segments/0/rlog.bz2", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token", "segment_num", "file")
	c.SetParamValues(tok, "0", "rlog.bz2")

	if err := h.GetShareMedia(c); err != nil {
		t.Fatalf("GetShareMedia error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (file blocked)", rec.Code)
	}
}

func TestShareMediaRejectsPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	mock := &shareMockDB{route: newShareTestRoute(1, "d", "r")}
	queries := db.New(mock)
	store := storage.New(tmpDir)
	h := NewShareHandler(queries, store, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	tok, err := share.Sign([]byte("secret"), 1, frozen.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	// Path traversal in the HLS file parameter.
	cases := []string{
		"../../../etc/passwd",
		"..",
		"/etc/passwd",
		"foo/bar.ts",
	}
	for _, file := range cases {
		t.Run(file, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet,
				"/v1/share/"+tok+"/segments/0/fcamera/"+file, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("token", "segment_num", "camera", "file")
			c.SetParamValues(tok, "0", "fcamera", file)

			if err := h.GetShareCameraMedia(c); err != nil {
				t.Fatalf("GetShareCameraMedia error: %v", err)
			}
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 for %q", rec.Code, file)
			}
		})
	}
}

func TestShareMediaHLSPlaylistServed(t *testing.T) {
	tmpDir := t.TempDir()
	// Lay down a real HLS playlist under the per-camera directory.
	hlsDir := filepath.Join(tmpDir, "d", "r", "0", "fcamera")
	if err := os.MkdirAll(hlsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	playlist := []byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:6.0,\nseg_000.ts\n#EXT-X-ENDLIST\n")
	if err := os.WriteFile(filepath.Join(hlsDir, "index.m3u8"), playlist, 0o644); err != nil {
		t.Fatalf("write playlist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hlsDir, "seg_000.ts"), []byte("TS-BYTES"), 0o644); err != nil {
		t.Fatalf("write ts: %v", err)
	}

	mock := &shareMockDB{route: newShareTestRoute(1, "d", "r")}
	queries := db.New(mock)
	store := storage.New(tmpDir)
	h := NewShareHandler(queries, store, "secret")
	frozen := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return frozen }

	tok, err := share.Sign([]byte("secret"), 1, frozen.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}

	// Request the playlist.
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/share/"+tok+"/segments/0/fcamera/index.m3u8", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("token", "segment_num", "camera", "file")
	c.SetParamValues(tok, "0", "fcamera", "index.m3u8")
	if err := h.GetShareCameraMedia(c); err != nil {
		t.Fatalf("GetShareCameraMedia error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "EXTM3U") {
		t.Errorf("expected playlist content, got %q", rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); !strings.Contains(ct, "mpegurl") {
		t.Errorf("content-type = %q, want mpegurl", ct)
	}

	// Request a .ts chunk.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet,
		"/v1/share/"+tok+"/segments/0/fcamera/seg_000.ts", nil)
	c2 := e.NewContext(req2, rec2)
	c2.SetParamNames("token", "segment_num", "camera", "file")
	c2.SetParamValues(tok, "0", "fcamera", "seg_000.ts")
	if err := h.GetShareCameraMedia(c2); err != nil {
		t.Fatalf("GetShareCameraMedia ts error: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "TS-BYTES") {
		t.Errorf("expected ts content, got %q", rec2.Body.String())
	}
	if ct := rec2.Header().Get(echo.HeaderContentType); !strings.Contains(ct, "mp2t") {
		t.Errorf("content-type = %q, want mp2t", ct)
	}
}

func TestIsAllowedHLSFile(t *testing.T) {
	cases := map[string]bool{
		"":                    false,
		"index.m3u8":          true,
		"seg_000.ts":          true,
		"../escape.ts":        false,
		"foo/bar":             false,
		"foo\\bar":            false,
		"index.m3u8.tampered": false,
		"..":                  false,
	}
	for name, want := range cases {
		if got := isAllowedHLSFile(name); got != want {
			t.Errorf("isAllowedHLSFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsAllowedShareMediaFile(t *testing.T) {
	cases := map[string]bool{
		"qcamera.ts":   true,
		"index.m3u8":   true,
		"fcamera.hevc": false,
		"rlog":         false,
		"rlog.bz2":     false,
		"unknown":      false,
		"":             false,
		"../boot.ini":  false,
	}
	for name, want := range cases {
		if got := isAllowedShareMediaFile(name); got != want {
			t.Errorf("isAllowedShareMediaFile(%q) = %v, want %v", name, got, want)
		}
	}
}
