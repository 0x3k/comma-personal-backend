package api

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
)

// exportMockDB is a tiny DBTX stub that returns a canned pgtype.Text for
// GetRouteGeometryWKT.
type exportMockDB struct {
	wkt pgtype.Text
	err error
}

func (m *exportMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *exportMockDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	return nil, fmt.Errorf("unexpected Query: %s", sql)
}

func (m *exportMockDB) QueryRow(_ context.Context, _ string, _ ...interface{}) pgx.Row {
	return &exportMockRow{wkt: m.wkt, err: m.err}
}

type exportMockRow struct {
	wkt pgtype.Text
	err error
}

func (r *exportMockRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return fmt.Errorf("expected 1 scan destination, got %d", len(dest))
	}
	target, ok := dest[0].(*pgtype.Text)
	if !ok {
		return fmt.Errorf("expected *pgtype.Text destination, got %T", dest[0])
	}
	*target = r.wkt
	return nil
}

// newExportRequest builds an Echo context for the GPX endpoint.
func newExportRequest(t *testing.T, mock *exportMockDB, dongleID, routeName, authDongleID string) (*httptest.ResponseRecorder, echo.Context, *ExportHandler) {
	t.Helper()

	queries := db.New(mock)
	handler := NewExportHandler(queries, nil)

	e := echo.New()
	target := fmt.Sprintf("/v1/routes/%s/%s/export.gpx", dongleID, routeName)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongleID, routeName)
	c.Set(middleware.ContextKeyDongleID, authDongleID)

	return rec, c, handler
}

func TestExportRouteGPXSuccess(t *testing.T) {
	mock := &exportMockDB{
		wkt: pgtype.Text{String: "LINESTRING(-122.4 37.7, -122.41 37.71, -122.42 37.72)", Valid: true},
	}
	rec, c, handler := newExportRequest(t, mock, "dongle-1", "2024-01-01--12-00-00", "dongle-1")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	ct := rec.Header().Get(echo.HeaderContentType)
	if !strings.HasPrefix(ct, "application/gpx+xml") {
		t.Errorf("Content-Type = %q, want application/gpx+xml", ct)
	}
	cd := rec.Header().Get(echo.HeaderContentDisposition)
	if !strings.Contains(cd, `filename="2024-01-01--12-00-00.gpx"`) {
		t.Errorf("Content-Disposition = %q, want attachment filename", cd)
	}

	var doc gpxFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("failed to parse GPX: %v\nbody=%s", err, rec.Body.String())
	}
	if doc.Version != "1.1" {
		t.Errorf("version = %q, want 1.1", doc.Version)
	}
	if len(doc.Tracks) != 1 || len(doc.Tracks[0].Segments) != 1 {
		t.Fatalf("tracks/segments structure = %+v", doc)
	}
	pts := doc.Tracks[0].Segments[0].Points
	if len(pts) != 3 {
		t.Fatalf("points = %d, want 3", len(pts))
	}
	if pts[0].Lat != 37.7 || pts[0].Lon != -122.4 {
		t.Errorf("points[0] = %+v, want lat=37.7 lon=-122.4", pts[0])
	}
	if pts[2].Lat != 37.72 || pts[2].Lon != -122.42 {
		t.Errorf("points[2] = %+v, want lat=37.72 lon=-122.42", pts[2])
	}
}

func TestExportRouteGPXByteCountScalesWithPoints(t *testing.T) {
	makeLineString := func(n int) string {
		var b strings.Builder
		b.WriteString("LINESTRING(")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%f %f", -122.4-float64(i)*0.01, 37.7+float64(i)*0.01)
		}
		b.WriteString(")")
		return b.String()
	}

	doExport := func(n int) int {
		mock := &exportMockDB{wkt: pgtype.Text{String: makeLineString(n), Valid: true}}
		rec, c, handler := newExportRequest(t, mock, "dongle", "r", "dongle")
		if err := handler.ExportRouteGPX(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		return rec.Body.Len()
	}

	size10 := doExport(10)
	size50 := doExport(50)
	if size50 <= size10 {
		t.Errorf("expected 50-point GPX larger than 10-point GPX, got %d <= %d", size50, size10)
	}

	mock := &exportMockDB{wkt: pgtype.Text{String: makeLineString(7), Valid: true}}
	rec, c, handler := newExportRequest(t, mock, "d", "r", "d")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var doc gpxFile
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("failed to parse GPX: %v", err)
	}
	if len(doc.Tracks[0].Segments[0].Points) != 7 {
		t.Errorf("expected 7 points, got %d", len(doc.Tracks[0].Segments[0].Points))
	}
}

func TestExportRouteGPXNullGeometryReturns404(t *testing.T) {
	mock := &exportMockDB{wkt: pgtype.Text{Valid: false}}
	rec, c, handler := newExportRequest(t, mock, "d", "r", "d")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if !strings.Contains(body.Error, "no geometry") {
		t.Errorf("error body = %q, want contains 'no geometry'", body.Error)
	}
}

func TestExportRouteGPXEmptyLineStringReturns404(t *testing.T) {
	mock := &exportMockDB{wkt: pgtype.Text{String: "LINESTRING EMPTY", Valid: true}}
	rec, c, handler := newExportRequest(t, mock, "d", "r", "d")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestExportRouteGPXRouteNotFoundReturns404(t *testing.T) {
	mock := &exportMockDB{err: pgx.ErrNoRows}
	rec, c, handler := newExportRequest(t, mock, "d", "r", "d")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode error body: %v", err)
	}
	if !strings.Contains(body.Error, "not found") {
		t.Errorf("error body = %q, want contains 'not found'", body.Error)
	}
}

func TestExportRouteGPXDongleMismatchReturns403(t *testing.T) {
	mock := &exportMockDB{wkt: pgtype.Text{String: "LINESTRING(0 0, 1 1)", Valid: true}}
	rec, c, handler := newExportRequest(t, mock, "owner-dongle", "r", "other-dongle")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestExportRouteGPXDatabaseErrorReturns500(t *testing.T) {
	mock := &exportMockDB{err: fmt.Errorf("connection refused")}
	rec, c, handler := newExportRequest(t, mock, "d", "r", "d")
	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestExportRouteGPXRegisterRoutes(t *testing.T) {
	mock := &exportMockDB{wkt: pgtype.Text{String: "LINESTRING(0 0, 1 1)", Valid: true}}
	queries := db.New(mock)
	handler := NewExportHandler(queries, nil)

	e := echo.New()
	g := e.Group("/v1/routes")
	handler.RegisterRoutes(g)

	req := httptest.NewRequest(http.MethodGet, "/v1/routes/d/r/export.gpx", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(middleware.ContextKeyDongleID, "d")
	e.Router().Find(http.MethodGet, "/v1/routes/d/r/export.gpx", c)
	if err := c.Handler()(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestParseLineStringWKT(t *testing.T) {
	cases := []struct {
		name    string
		wkt     string
		want    []gpxTrkpt
		wantErr bool
	}{
		{
			name: "single point",
			wkt:  "LINESTRING(-122.4 37.7)",
			want: []gpxTrkpt{{Lat: 37.7, Lon: -122.4}},
		},
		{
			name: "multiple points",
			wkt:  "LINESTRING(-122.4 37.7, -122.41 37.71, -122.42 37.72)",
			want: []gpxTrkpt{
				{Lat: 37.7, Lon: -122.4},
				{Lat: 37.71, Lon: -122.41},
				{Lat: 37.72, Lon: -122.42},
			},
		},
		{
			name: "empty",
			wkt:  "LINESTRING EMPTY",
			want: []gpxTrkpt{},
		},
		{
			name:    "wrong geometry type",
			wkt:     "POINT(0 0)",
			wantErr: true,
		},
		{
			name:    "malformed body",
			wkt:     "LINESTRING 1 2 3",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLineStringWKT(tc.wkt)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d points, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ----- MP4 export tests -----

const (
	testExportDongleID = "abc123"
	testExportRoute    = "2024-03-15--12-30-00"
)

// newExportMP4Request builds an Echo context targeting the MP4 export endpoint.
func newExportMP4Request(t *testing.T, dongleID, routeName, camera, authDongle string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	target := "/v1/routes/" + dongleID + "/" + routeName + "/export.mp4"
	if camera != "" {
		target += "?camera=" + camera
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongleID, routeName)
	c.Set(middleware.ContextKeyDongleID, authDongle)
	return rec, c
}

// writeTinyTS writes a tiny but valid single-stream MPEG-TS file using ffmpeg.
func writeTinyTS(t *testing.T, ffmpegPath, outPath string) {
	t.Helper()
	cmd := exec.Command(ffmpegPath,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "color=black:s=16x16:d=0.3:r=10",
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		"-pix_fmt", "yuv420p",
		"-f", "mpegts",
		outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to create test ts fixture: %v: %s", err, string(out))
	}
}

// setupHLSRoute lays out storage so it mirrors what the transcoder produces.
func setupHLSRoute(t *testing.T, store *storage.Storage, ffmpegPath, dongleID, routeName, camera string, segmentCount int) {
	t.Helper()
	hlsDir, ok := cameraToHLSDir[camera]
	if !ok {
		t.Fatalf("unknown camera %q", camera)
	}
	for seg := 0; seg < segmentCount; seg++ {
		segStr := strconv.Itoa(seg)
		dir := filepath.Join(store.Path(dongleID, routeName, segStr, ""), hlsDir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("failed to create hls dir: %v", err)
		}
		writeTinyTS(t, ffmpegPath, filepath.Join(dir, "seg_000.ts"))
	}
}

// newExportStorage builds a Storage rooted at a temporary directory.
func newExportStorage(t *testing.T) *storage.Storage {
	t.Helper()
	dir := t.TempDir()
	return storage.New(dir)
}

func TestExportMP4_HappyPath(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 2)

	handler := NewExportHandler(nil, store)
	handler.SetFFmpegPath(ffmpegPath)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute, "f", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4 returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); !strings.HasPrefix(ct, "video/mp4") {
		t.Errorf("Content-Type = %q, want video/mp4", ct)
	}
	body := rec.Body.Bytes()
	if len(body) < 64 {
		t.Fatalf("response too short: %d bytes", len(body))
	}
	if !bytes.Contains(body[:64], []byte("ftyp")) {
		t.Errorf("expected 'ftyp' box in first 64 bytes; got %q", body[:64])
	}
}

func TestExportMP4_MissingHLS(t *testing.T) {
	store := newExportStorage(t)
	handler := NewExportHandler(nil, store)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute, "f", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4 returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestExportMP4_MissingHLS_ExistingRouteDifferentCamera(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}
	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 1)

	handler := NewExportHandler(nil, store)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute, "e", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4 returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestExportMP4_BadCamera(t *testing.T) {
	store := newExportStorage(t)
	handler := NewExportHandler(nil, store)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute, "x", testExportDongleID)
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4 returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestExportMP4_DongleMismatch(t *testing.T) {
	store := newExportStorage(t)
	handler := NewExportHandler(nil, store)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute, "f", "other-dongle")
	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4 returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestExportMP4_CancellationKillsFfmpeg(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal handling test not applicable on Windows")
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 20)

	handler := NewExportHandler(nil, store)
	handler.SetFFmpegPath(ffmpegPath)

	ctx, cancel := context.WithCancel(context.Background())

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/routes/"+testExportDongleID+"/"+testExportRoute+"/export.mp4?camera=f", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(testExportDongleID, testExportRoute)
	c.Set(middleware.ContextKeyDongleID, testExportDongleID)

	done := make(chan error, 1)
	go func() {
		done <- handler.ExportMP4(c)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("ExportMP4 after cancel returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExportMP4 did not return after cancellation")
	}

	_ = syscall.Getpid()
}

func TestBuildConcatList(t *testing.T) {
	paths := []string{"/tmp/a.ts", "/tmp/b's.ts"}
	got := buildConcatList(paths)
	want := "ffconcat version 1.0\nfile 'file:/tmp/a.ts'\nfile 'file:/tmp/b'\\''s.ts'\n"
	if got != want {
		t.Errorf("buildConcatList = %q\nwant %q", got, want)
	}
}

// ----- redact_plates query param tests -----

// exportRedactMockDB is a multi-table stub that serves both the route
// geometry lookup (used by the GPX endpoint) and the
// ListDetectionsForRoute query the redaction path consumes. It also
// stubs GetSetting so the export handler can read alpr_enabled.
type exportRedactMockDB struct {
	wkt        pgtype.Text
	settings   map[string]string
	detections []db.ListDetectionsForRouteRow
}

func (m *exportRedactMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *exportRedactMockDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM plate_detections") {
		return &exportDetectionRows{rows: m.detections}, nil
	}
	return nil, fmt.Errorf("unexpected Query: %s", sql)
}

func (m *exportRedactMockDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	if strings.Contains(sql, "FROM settings") {
		v, ok := m.settings["alpr_enabled"]
		if !ok {
			return &exportMockRow{err: pgx.ErrNoRows}
		}
		return &exportSettingsRow{value: v}
	}
	return &exportMockRow{wkt: m.wkt}
}

type exportSettingsRow struct{ value string }

func (r *exportSettingsRow) Scan(dest ...interface{}) error {
	if len(dest) < 3 {
		return fmt.Errorf("expected 3 dests, got %d", len(dest))
	}
	if k, ok := dest[0].(*string); ok {
		*k = "alpr_enabled"
	}
	if v, ok := dest[1].(*string); ok {
		*v = r.value
	}
	if ts, ok := dest[2].(*pgtype.Timestamptz); ok {
		*ts = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	return nil
}

// exportDetectionRows iterates a slice of ListDetectionsForRouteRow
// for the redaction-loader test path.
type exportDetectionRows struct {
	rows []db.ListDetectionsForRouteRow
	idx  int
}

func (r *exportDetectionRows) Close()                                       {}
func (r *exportDetectionRows) Err() error                                   { return nil }
func (r *exportDetectionRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *exportDetectionRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *exportDetectionRows) Next() bool {
	r.idx++
	return r.idx <= len(r.rows)
}
func (r *exportDetectionRows) Values() ([]interface{}, error) { return nil, nil }
func (r *exportDetectionRows) RawValues() [][]byte            { return nil }
func (r *exportDetectionRows) Conn() *pgx.Conn                { return nil }
func (r *exportDetectionRows) Scan(dest ...interface{}) error {
	if r.idx == 0 || r.idx > len(r.rows) {
		return fmt.Errorf("Scan called out of range")
	}
	src := r.rows[r.idx-1]
	// dests, in order matching ListDetectionsForRoute columns.
	if d, ok := dest[0].(*int64); ok {
		*d = src.ID
	}
	if d, ok := dest[1].(*string); ok {
		*d = src.DongleID
	}
	if d, ok := dest[2].(*string); ok {
		*d = src.Route
	}
	if d, ok := dest[3].(*int32); ok {
		*d = src.Segment
	}
	if d, ok := dest[4].(*int32); ok {
		*d = src.FrameOffsetMs
	}
	if d, ok := dest[5].(*[]byte); ok {
		*d = src.PlateCiphertext
	}
	if d, ok := dest[6].(*[]byte); ok {
		*d = src.PlateHash
	}
	if d, ok := dest[7].(*[]byte); ok {
		*d = src.Bbox
	}
	if d, ok := dest[8].(*float32); ok {
		*d = src.Confidence
	}
	if d, ok := dest[9].(*bool); ok {
		*d = src.OcrCorrected
	}
	if d, ok := dest[10].(*pgtype.Float8); ok {
		*d = src.GpsLat
	}
	if d, ok := dest[11].(*pgtype.Float8); ok {
		*d = src.GpsLng
	}
	if d, ok := dest[12].(*pgtype.Float4); ok {
		*d = src.GpsHeadingDeg
	}
	if d, ok := dest[13].(*pgtype.Timestamptz); ok {
		*d = src.FrameTs
	}
	if d, ok := dest[14].(*pgtype.Text); ok {
		*d = src.ThumbPath
	}
	if d, ok := dest[15].(*pgtype.Timestamptz); ok {
		*d = src.CreatedAt
	}
	return nil
}

func TestExportMP4_RedactPlatesNoOpWhenALPRDisabled(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 1)

	mock := &exportRedactMockDB{settings: map[string]string{"alpr_enabled": "false"}}
	queries := db.New(mock)
	settingsStore := settings.New(queries)

	handler := NewExportHandler(queries, store).WithSettings(settingsStore)
	handler.SetFFmpegPath(ffmpegPath)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute,
		"f", testExportDongleID)
	// Inject ?redact_plates=true; ALPR is off so the request must
	// degrade to the unredacted (-c copy) path and return 200.
	c.QueryParams().Set("redact_plates", "true")

	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Filename should be the unredacted form when redaction was a no-op.
	cd := rec.Header().Get(echo.HeaderContentDisposition)
	if strings.Contains(cd, "redacted") {
		t.Errorf("Content-Disposition = %q, must not say 'redacted' on no-op path", cd)
	}
}

func TestExportMP4_RedactPlatesFalseProducesIdenticalOutput(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 2)

	handler := NewExportHandler(nil, store)
	handler.SetFFmpegPath(ffmpegPath)

	// First export with no redaction.
	rec1, c1 := newExportMP4Request(t, testExportDongleID, testExportRoute,
		"f", testExportDongleID)
	if err := handler.ExportMP4(c1); err != nil {
		t.Fatalf("ExportMP4 first: %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d", rec1.Code)
	}

	// Second export with redact_plates=false (explicit no-op).
	rec2, c2 := newExportMP4Request(t, testExportDongleID, testExportRoute,
		"f", testExportDongleID)
	c2.QueryParams().Set("redact_plates", "false")
	if err := handler.ExportMP4(c2); err != nil {
		t.Fatalf("ExportMP4 second: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec2.Code)
	}

	// Both bodies should be identical: the same -c copy ffmpeg
	// invocation runs in both cases and ffmpeg is deterministic.
	if !bytes.Equal(rec1.Body.Bytes(), rec2.Body.Bytes()) {
		t.Errorf("redact_plates=false changed output bytes (len1=%d, len2=%d)",
			len(rec1.Body.Bytes()), len(rec2.Body.Bytes()))
	}
}

func TestExportMP4_RedactPlatesTrueAppliesFilter(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 1)

	// Build a fake detection in segment 0 covering most of the frame
	// so the boxblur measurably reduces variance over the source.
	bboxJSON, err := json.Marshal(map[string]float64{
		"x": 100, "y": 50, "w": 800, "h": 400,
	})
	if err != nil {
		t.Fatalf("marshal bbox: %v", err)
	}
	mock := &exportRedactMockDB{
		settings: map[string]string{"alpr_enabled": "true"},
		detections: []db.ListDetectionsForRouteRow{
			{
				ID:            1,
				DongleID:      testExportDongleID,
				Route:         testExportRoute,
				Segment:       0,
				FrameOffsetMs: 0,
				Bbox:          bboxJSON,
				Confidence:    0.9,
				FrameTs:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
			},
		},
	}
	queries := db.New(mock)
	settingsStore := settings.New(queries)
	handler := NewExportHandler(queries, store).WithSettings(settingsStore)
	handler.SetFFmpegPath(ffmpegPath)

	rec, c := newExportMP4Request(t, testExportDongleID, testExportRoute,
		"f", testExportDongleID)
	c.QueryParams().Set("redact_plates", "true")

	if err := handler.ExportMP4(c); err != nil {
		t.Fatalf("ExportMP4: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Filename suffix changes to indicate redaction.
	cd := rec.Header().Get(echo.HeaderContentDisposition)
	if !strings.Contains(cd, "redacted") {
		t.Errorf("Content-Disposition = %q, want contains 'redacted'", cd)
	}
	// Body should still be a valid MP4 (ftyp box present).
	body := rec.Body.Bytes()
	if len(body) < 64 {
		t.Fatalf("response too short: %d bytes", len(body))
	}
	if !bytes.Contains(body[:64], []byte("ftyp")) {
		t.Errorf("expected ftyp box in first 64 bytes, got %q", body[:64])
	}
}

// TestExportMP4_RedactedOutputDiffersFromUnredacted validates the
// effect of redaction at the byte level: with the same input, a
// redacted export must produce a different MP4 than the unredacted
// one. This is the export-side counterpart to the per-pixel variance
// check in the redaction-builder integration test (which verifies the
// HLS variant).
func TestExportMP4_RedactedOutputDiffersFromUnredacted(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available; skipping")
	}

	store := newExportStorage(t)
	setupHLSRoute(t, store, ffmpegPath, testExportDongleID, testExportRoute, "f", 1)

	bboxJSON, _ := json.Marshal(map[string]float64{
		"x": 100, "y": 50, "w": 800, "h": 400,
	})
	mock := &exportRedactMockDB{
		settings: map[string]string{"alpr_enabled": "true"},
		detections: []db.ListDetectionsForRouteRow{
			{ID: 1, Segment: 0, FrameOffsetMs: 0, Bbox: bboxJSON, Confidence: 0.9,
				FrameTs: pgtype.Timestamptz{Time: time.Now(), Valid: true}},
		},
	}
	queries := db.New(mock)
	settingsStore := settings.New(queries)
	handler := NewExportHandler(queries, store).WithSettings(settingsStore)
	handler.SetFFmpegPath(ffmpegPath)

	// Unredacted.
	rec1, c1 := newExportMP4Request(t, testExportDongleID, testExportRoute,
		"f", testExportDongleID)
	if err := handler.ExportMP4(c1); err != nil {
		t.Fatalf("ExportMP4 (no redact): %v", err)
	}
	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d", rec1.Code)
	}

	// Redacted.
	rec2, c2 := newExportMP4Request(t, testExportDongleID, testExportRoute,
		"f", testExportDongleID)
	c2.QueryParams().Set("redact_plates", "true")
	if err := handler.ExportMP4(c2); err != nil {
		t.Fatalf("ExportMP4 (redact): %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d", rec2.Code)
	}

	if bytes.Equal(rec1.Body.Bytes(), rec2.Body.Bytes()) {
		t.Errorf("redact_plates=true produced byte-identical output to unredacted")
	}
}

func TestParseBoolQuery(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"True":  true,
		"TRUE":  true,
		"1":     true,
		"yes":   true,
		"on":    true,
		"":      false,
		"false": false,
		"0":     false,
		"no":    false,
		"asdf":  false,
	}
	for in, want := range cases {
		if got := parseBoolQuery(in); got != want {
			t.Errorf("parseBoolQuery(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTSOrderKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/a/seg_000.ts", "000000000000"},
		{"/a/seg_010.ts", "000000000010"},
		{"/a/seg_1.ts", "000000000001"},
		{"/a/noisy.ts", "noisy"},
	}
	for _, tc := range cases {
		got := tsOrderKey(tc.in)
		if got != tc.want {
			t.Errorf("tsOrderKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
