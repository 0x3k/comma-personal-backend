package api

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// exportMockDB is a tiny DBTX stub that returns a canned pgtype.Text for
// GetRouteGeometryWKT. It covers all three cases the handler cares about:
// row not found, row with NULL geometry, and row with a WKT string.
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

// newExportRequest builds an Echo context pre-populated with the path
// parameters and the authenticated dongle_id the handler expects. Tests
// pass the same dongle_id for both unless they are exercising the auth
// mismatch path.
func newExportRequest(t *testing.T, mock *exportMockDB, dongleID, routeName, authDongleID string) (*httptest.ResponseRecorder, echo.Context, *ExportHandler) {
	t.Helper()

	queries := db.New(mock)
	handler := NewExportHandler(queries)

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
	// Three ordered points, each with distinct lon/lat so we can assert the
	// attribute ordering in the marshalled output.
	wkt := "LINESTRING(-122.4194 37.7749,-122.42 37.78,-122.43 37.79)"

	mock := &exportMockDB{wkt: pgtype.Text{String: wkt, Valid: true}}
	rec, c, handler := newExportRequest(t, mock, "dongle1", "2024-03-15--12-30-00", "dongle1")

	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	if ct := rec.Header().Get(echo.HeaderContentType); ct != "application/gpx+xml" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/gpx+xml")
	}
	if cd := rec.Header().Get(echo.HeaderContentDisposition); cd != `attachment; filename="2024-03-15--12-30-00.gpx"` {
		t.Errorf("Content-Disposition = %q, want attachment with route filename", cd)
	}

	// Acceptance criterion: "200 with the right byte count when geometry has
	// N points". We assert this by round-tripping the body through
	// encoding/xml and comparing the re-emitted bytes exactly.
	body := rec.Body.Bytes()
	if len(body) == 0 {
		t.Fatal("response body is empty")
	}

	var parsed gpxFile
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response body is not valid XML: %v\nbody: %s", err, body)
	}

	if parsed.Version != "1.1" {
		t.Errorf("gpx version = %q, want 1.1", parsed.Version)
	}
	if parsed.Xmlns != "http://www.topografix.com/GPX/1/1" {
		t.Errorf("gpx xmlns = %q, want GPX 1.1 namespace", parsed.Xmlns)
	}

	if len(parsed.Tracks) != 1 {
		t.Fatalf("len(tracks) = %d, want 1", len(parsed.Tracks))
	}
	trk := parsed.Tracks[0]
	if trk.Name != "2024-03-15--12-30-00" {
		t.Errorf("trk name = %q, want route name", trk.Name)
	}
	if len(trk.Segments) != 1 {
		t.Fatalf("len(segments) = %d, want 1", len(trk.Segments))
	}

	pts := trk.Segments[0].Points
	if len(pts) != 3 {
		t.Fatalf("len(trkpt) = %d, want 3", len(pts))
	}
	wantLatLon := []struct{ lat, lon float64 }{
		{37.7749, -122.4194},
		{37.78, -122.42},
		{37.79, -122.43},
	}
	for i, want := range wantLatLon {
		if pts[i].Lat != want.lat || pts[i].Lon != want.lon {
			t.Errorf("trkpt[%d] = (%v,%v), want (%v,%v)",
				i, pts[i].Lat, pts[i].Lon, want.lat, want.lon)
		}
	}

	// Re-marshal the parsed document and verify the re-emitted byte count
	// matches the structure we expect (header + marshalled body). Any drift
	// between marshal and unmarshal would surface here.
	reBody, err := xml.Marshal(parsed)
	if err != nil {
		t.Fatalf("failed to re-marshal GPX: %v", err)
	}
	expected := append([]byte(xml.Header), reBody...)
	if len(body) != len(expected) {
		t.Errorf("response byte count = %d, want %d (round-trip mismatch)", len(body), len(expected))
	}
	if string(body) != string(expected) {
		t.Errorf("response bytes differ from round-tripped bytes\n got: %s\nwant: %s", body, expected)
	}
}

func TestExportRouteGPXByteCountScalesWithPoints(t *testing.T) {
	// Build WKT strings of increasing size and confirm the emitted GPX has
	// exactly one <trkpt> per input coordinate.
	cases := []int{1, 5, 25}

	for _, n := range cases {
		t.Run(fmt.Sprintf("%d_points", n), func(t *testing.T) {
			coords := make([]string, 0, n)
			for i := 0; i < n; i++ {
				// Distinct coordinates keep every trkpt element the same
				// width, so byte counts are predictable.
				coords = append(coords, fmt.Sprintf("%d.1 %d.2", -120-i, 30+i))
			}
			wkt := "LINESTRING(" + strings.Join(coords, ",") + ")"

			mock := &exportMockDB{wkt: pgtype.Text{String: wkt, Valid: true}}
			rec, c, handler := newExportRequest(t, mock, "dongle1", "r", "dongle1")

			if err := handler.ExportRouteGPX(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}

			var parsed gpxFile
			if err := xml.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
				t.Fatalf("response body is not valid XML: %v", err)
			}
			if len(parsed.Tracks) != 1 || len(parsed.Tracks[0].Segments) != 1 {
				t.Fatalf("expected one trk/trkseg, got tracks=%d segs=%v",
					len(parsed.Tracks), parsed.Tracks)
			}
			got := len(parsed.Tracks[0].Segments[0].Points)
			if got != n {
				t.Errorf("trkpt count = %d, want %d", got, n)
			}

			// Acceptance criterion: byte count matches the xml header plus
			// whatever xml.Marshal emits for the parsed structure.
			reBody, err := xml.Marshal(parsed)
			if err != nil {
				t.Fatalf("failed to re-marshal GPX: %v", err)
			}
			expected := len([]byte(xml.Header)) + len(reBody)
			if rec.Body.Len() != expected {
				t.Errorf("body length = %d, want %d", rec.Body.Len(), expected)
			}
		})
	}
}

func TestExportRouteGPXNullGeometryReturns404(t *testing.T) {
	mock := &exportMockDB{wkt: pgtype.Text{Valid: false}}
	rec, c, handler := newExportRequest(t, mock, "dongle1", "2024-03-15--12-30-00", "dongle1")

	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error body: %v", err)
	}
	if !strings.Contains(body.Error, "no geometry") {
		t.Errorf("error = %q, want message mentioning missing geometry", body.Error)
	}

	// A 404 must NOT include a gpx attachment response -- confirm we never
	// leaked an empty file to the client.
	if cd := rec.Header().Get(echo.HeaderContentDisposition); cd != "" {
		t.Errorf("Content-Disposition = %q, want empty on 404", cd)
	}
}

func TestExportRouteGPXEmptyLineStringReturns404(t *testing.T) {
	// PostGIS renders a zero-point LineString as "LINESTRING EMPTY". The
	// handler should treat that the same as NULL.
	mock := &exportMockDB{wkt: pgtype.Text{String: "LINESTRING EMPTY", Valid: true}}
	rec, c, handler := newExportRequest(t, mock, "dongle1", "r", "dongle1")

	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestExportRouteGPXRouteNotFoundReturns404(t *testing.T) {
	mock := &exportMockDB{err: pgx.ErrNoRows}
	rec, c, handler := newExportRequest(t, mock, "dongle1", "missing-route", "dongle1")

	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error body: %v", err)
	}
	if !strings.Contains(body.Error, "not found") {
		t.Errorf("error = %q, want message mentioning 'not found'", body.Error)
	}
}

func TestExportRouteGPXDongleMismatchReturns403(t *testing.T) {
	mock := &exportMockDB{wkt: pgtype.Text{String: "LINESTRING(1 2)", Valid: true}}
	rec, c, handler := newExportRequest(t, mock, "dongle1", "r", "other-device")

	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestExportRouteGPXDatabaseErrorReturns500(t *testing.T) {
	mock := &exportMockDB{err: fmt.Errorf("connection refused")}
	rec, c, handler := newExportRequest(t, mock, "dongle1", "r", "dongle1")

	if err := handler.ExportRouteGPX(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
}

func TestExportRouteGPXRegisterRoutes(t *testing.T) {
	mock := &exportMockDB{}
	queries := db.New(mock)
	handler := NewExportHandler(queries)

	e := echo.New()
	g := e.Group("/v1/routes")
	handler.RegisterRoutes(g)

	found := false
	for _, r := range e.Routes() {
		if r.Method == http.MethodGet && r.Path == "/v1/routes/:dongle_id/:route_name/export.gpx" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected GET /v1/routes/:dongle_id/:route_name/export.gpx to be registered")
	}
}

func TestParseLineStringWKT(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []gpxTrkpt
		wantErr bool
	}{
		{
			name:  "single point",
			input: "LINESTRING(-122.4 37.7)",
			want:  []gpxTrkpt{{Lat: 37.7, Lon: -122.4}},
		},
		{
			name:  "multiple points",
			input: "LINESTRING(-122.4 37.7, -122.5 37.8, -122.6 37.9)",
			want: []gpxTrkpt{
				{Lat: 37.7, Lon: -122.4},
				{Lat: 37.8, Lon: -122.5},
				{Lat: 37.9, Lon: -122.6},
			},
		},
		{
			name:  "lowercase linestring",
			input: "linestring(1 2,3 4)",
			want: []gpxTrkpt{
				{Lat: 2, Lon: 1},
				{Lat: 4, Lon: 3},
			},
		},
		{
			name:  "empty linestring",
			input: "LINESTRING EMPTY",
			want:  []gpxTrkpt{},
		},
		{
			name:    "not a linestring",
			input:   "POINT(1 2)",
			wantErr: true,
		},
		{
			name:    "missing parens",
			input:   "LINESTRING 1 2",
			wantErr: true,
		},
		{
			name:    "bad coordinate",
			input:   "LINESTRING(abc def)",
			wantErr: true,
		},
		{
			name:    "single coordinate (missing lat)",
			input:   "LINESTRING(1)",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLineStringWKT(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d points, want %d", len(got), len(tt.want))
			}
			for i, pt := range got {
				if pt.Lat != tt.want[i].Lat || pt.Lon != tt.want[i].Lon {
					t.Errorf("points[%d] = %+v, want %+v", i, pt, tt.want[i])
				}
			}
		})
	}
}
