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
)

// routeMockDB implements db.DBTX for route handler tests. It dispatches
// based on the SQL query string to return the appropriate mock data.
type routeMockDB struct {
	route       *db.Route
	routeErr    error
	routes      []db.Route
	routesErr   error
	routeCount  int64
	countErr    error
	segments    []db.Segment
	segmentsErr error
	segCount    int64
}

func (m *routeMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *routeMockDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "segment_count") {
		if m.routesErr != nil {
			return nil, m.routesErr
		}
		return &mockRouteWithCountRows{routes: m.routes, segCount: m.segCount}, nil
	}
	if strings.Contains(sql, "FROM routes") {
		if m.routesErr != nil {
			return nil, m.routesErr
		}
		return &mockRouteRows{routes: m.routes}, nil
	}
	if strings.Contains(sql, "FROM segments") {
		if m.segmentsErr != nil {
			return nil, m.segmentsErr
		}
		return &mockSegmentRows{segments: m.segments}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func (m *routeMockDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	if strings.Contains(sql, "count(*)") && strings.Contains(sql, "routes") {
		if m.countErr != nil {
			return &mockCountRow{err: m.countErr}
		}
		return &mockCountRow{count: m.routeCount}
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
	return &mockRouteRow{err: fmt.Errorf("unexpected query: %s", sql)}
}

// mockRouteRow implements pgx.Row for a single Route scan.
type mockRouteRow struct {
	route *db.Route
	err   error
}

func (r *mockRouteRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) < 7 {
		return fmt.Errorf("expected 7 scan destinations, got %d", len(dest))
	}
	*dest[0].(*int32) = r.route.ID
	*dest[1].(*string) = r.route.DongleID
	*dest[2].(*string) = r.route.RouteName
	*dest[3].(*pgtype.Timestamptz) = r.route.StartTime
	*dest[4].(*pgtype.Timestamptz) = r.route.EndTime
	*dest[5].(*interface{}) = r.route.Geometry
	*dest[6].(*pgtype.Timestamptz) = r.route.CreatedAt
	return nil
}

// mockCountRow implements pgx.Row for count queries.
type mockCountRow struct {
	count int64
	err   error
}

func (r *mockCountRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64) = r.count
	return nil
}

// mockRouteRows implements pgx.Rows for listing routes.
type mockRouteRows struct {
	routes []db.Route
	idx    int
	closed bool
}

func (r *mockRouteRows) Close()                                       { r.closed = true }
func (r *mockRouteRows) Err() error                                   { return nil }
func (r *mockRouteRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *mockRouteRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockRouteRows) RawValues() [][]byte                          { return nil }
func (r *mockRouteRows) Conn() *pgx.Conn                              { return nil }

func (r *mockRouteRows) Next() bool {
	if r.idx < len(r.routes) {
		r.idx++
		return true
	}
	return false
}

func (r *mockRouteRows) Scan(dest ...interface{}) error {
	route := r.routes[r.idx-1]
	*dest[0].(*int32) = route.ID
	*dest[1].(*string) = route.DongleID
	*dest[2].(*string) = route.RouteName
	*dest[3].(*pgtype.Timestamptz) = route.StartTime
	*dest[4].(*pgtype.Timestamptz) = route.EndTime
	*dest[5].(*interface{}) = route.Geometry
	*dest[6].(*pgtype.Timestamptz) = route.CreatedAt
	return nil
}

func (r *mockRouteRows) Values() ([]interface{}, error) { return nil, nil }

// mockRouteWithCountRows implements pgx.Rows for ListRoutesByDeviceWithCounts.
type mockRouteWithCountRows struct {
	routes   []db.Route
	segCount int64
	idx      int
	closed   bool
}

func (r *mockRouteWithCountRows) Close()                                       { r.closed = true }
func (r *mockRouteWithCountRows) Err() error                                   { return nil }
func (r *mockRouteWithCountRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *mockRouteWithCountRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockRouteWithCountRows) RawValues() [][]byte                          { return nil }
func (r *mockRouteWithCountRows) Conn() *pgx.Conn                              { return nil }

func (r *mockRouteWithCountRows) Next() bool {
	if r.idx < len(r.routes) {
		r.idx++
		return true
	}
	return false
}

func (r *mockRouteWithCountRows) Scan(dest ...interface{}) error {
	route := r.routes[r.idx-1]
	*dest[0].(*int32) = route.ID
	*dest[1].(*string) = route.DongleID
	*dest[2].(*string) = route.RouteName
	*dest[3].(*pgtype.Timestamptz) = route.StartTime
	*dest[4].(*pgtype.Timestamptz) = route.EndTime
	*dest[5].(*interface{}) = route.Geometry
	*dest[6].(*pgtype.Timestamptz) = route.CreatedAt
	*dest[7].(*int64) = r.segCount
	return nil
}

func (r *mockRouteWithCountRows) Values() ([]interface{}, error) { return nil, nil }

// mockSegmentRows implements pgx.Rows for listing segments.
type mockSegmentRows struct {
	segments []db.Segment
	idx      int
	closed   bool
}

func (r *mockSegmentRows) Close()                                       { r.closed = true }
func (r *mockSegmentRows) Err() error                                   { return nil }
func (r *mockSegmentRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *mockSegmentRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockSegmentRows) RawValues() [][]byte                          { return nil }
func (r *mockSegmentRows) Conn() *pgx.Conn                              { return nil }

func (r *mockSegmentRows) Next() bool {
	if r.idx < len(r.segments) {
		r.idx++
		return true
	}
	return false
}

func (r *mockSegmentRows) Scan(dest ...interface{}) error {
	seg := r.segments[r.idx-1]
	*dest[0].(*int32) = seg.ID
	*dest[1].(*int32) = seg.RouteID
	*dest[2].(*int32) = seg.SegmentNumber
	*dest[3].(*bool) = seg.RlogUploaded
	*dest[4].(*bool) = seg.QlogUploaded
	*dest[5].(*bool) = seg.FcameraUploaded
	*dest[6].(*bool) = seg.EcameraUploaded
	*dest[7].(*bool) = seg.DcameraUploaded
	*dest[8].(*bool) = seg.QcameraUploaded
	*dest[9].(*pgtype.Timestamptz) = seg.CreatedAt
	return nil
}

func (r *mockSegmentRows) Values() ([]interface{}, error) { return nil, nil }

func newTestRoute(id int32, dongleID, routeName string) *db.Route {
	now := time.Now()
	return &db.Route{
		ID:        id,
		DongleID:  dongleID,
		RouteName: routeName,
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(10 * time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func newTestSegment(id, routeID, number int32) db.Segment {
	return db.Segment{
		ID:              id,
		RouteID:         routeID,
		SegmentNumber:   number,
		RlogUploaded:    true,
		QlogUploaded:    false,
		FcameraUploaded: true,
		EcameraUploaded: false,
		DcameraUploaded: false,
		QcameraUploaded: true,
		CreatedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
}

func TestGetRoute(t *testing.T) {
	tests := []struct {
		name         string
		dongleID     string
		routeName    string
		authDongleID string
		mock         *routeMockDB
		wantStatus   int
		wantError    string
	}{
		{
			name:         "successful route retrieval",
			dongleID:     "abc123",
			routeName:    "2024-03-15--12-30-00",
			authDongleID: "abc123",
			mock: &routeMockDB{
				route: newTestRoute(1, "abc123", "2024-03-15--12-30-00"),
				segments: []db.Segment{
					newTestSegment(1, 1, 0),
					newTestSegment(2, 1, 1),
				},
				segCount: 2,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:         "route not found",
			dongleID:     "abc123",
			routeName:    "2024-01-01--00-00-00",
			authDongleID: "abc123",
			mock: &routeMockDB{
				routeErr: pgx.ErrNoRows,
			},
			wantStatus: http.StatusNotFound,
			wantError:  "route 2024-01-01--00-00-00 not found",
		},
		{
			name:         "dongle_id mismatch",
			dongleID:     "abc123",
			routeName:    "2024-03-15--12-30-00",
			authDongleID: "other999",
			mock:         &routeMockDB{},
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "database error on route query",
			dongleID:     "abc123",
			routeName:    "2024-03-15--12-30-00",
			authDongleID: "abc123",
			mock: &routeMockDB{
				routeErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to retrieve route",
		},
		{
			name:         "database error on segment list",
			dongleID:     "abc123",
			routeName:    "2024-03-15--12-30-00",
			authDongleID: "abc123",
			mock: &routeMockDB{
				route:       newTestRoute(1, "abc123", "2024-03-15--12-30-00"),
				segmentsErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to retrieve segments",
		},
		{
			name:         "route with no segments",
			dongleID:     "abc123",
			routeName:    "2024-03-15--12-30-00",
			authDongleID: "abc123",
			mock: &routeMockDB{
				route:    newTestRoute(1, "abc123", "2024-03-15--12-30-00"),
				segments: []db.Segment{},
				segCount: 0,
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := db.New(tt.mock)
			handler := NewRouteHandler(queries)

			e := echo.New()
			target := fmt.Sprintf("/v1/route/%s/%s", tt.dongleID, tt.routeName)
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id", "route_name")
			c.SetParamValues(tt.dongleID, tt.routeName)
			c.Set(middleware.ContextKeyDongleID, tt.authDongleID)

			err := handler.GetRoute(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantError != "" {
				var body errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse error body: %v", err)
				}
				if !strings.Contains(body.Error, tt.wantError) {
					t.Errorf("error = %q, want substring %q", body.Error, tt.wantError)
				}
				return
			}

			var body routeDetailResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response body: %v", err)
			}

			if body.DongleID != tt.dongleID {
				t.Errorf("dongleId = %q, want %q", body.DongleID, tt.dongleID)
			}
			if body.RouteName != tt.routeName {
				t.Errorf("routeName = %q, want %q", body.RouteName, tt.routeName)
			}
			if body.SegmentCount != tt.mock.segCount {
				t.Errorf("segmentCount = %d, want %d", body.SegmentCount, tt.mock.segCount)
			}
			if len(body.Segments) != len(tt.mock.segments) {
				t.Errorf("len(segments) = %d, want %d", len(body.Segments), len(tt.mock.segments))
			}
		})
	}
}

func TestGetRouteSegmentUploadStatus(t *testing.T) {
	seg := db.Segment{
		ID:              1,
		RouteID:         1,
		SegmentNumber:   0,
		RlogUploaded:    true,
		QlogUploaded:    false,
		FcameraUploaded: true,
		EcameraUploaded: false,
		DcameraUploaded: true,
		QcameraUploaded: false,
		CreatedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	mock := &routeMockDB{
		route:    newTestRoute(1, "abc123", "2024-03-15--12-30-00"),
		segments: []db.Segment{seg},
		segCount: 1,
	}

	queries := db.New(mock)
	handler := NewRouteHandler(queries)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/route/abc123/2024-03-15--12-30-00", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues("abc123", "2024-03-15--12-30-00")
	c.Set(middleware.ContextKeyDongleID, "abc123")

	if err := handler.GetRoute(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body routeDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	if len(body.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(body.Segments))
	}

	s := body.Segments[0]
	if s.Number != 0 {
		t.Errorf("segment number = %d, want 0", s.Number)
	}
	if !s.RlogUploaded {
		t.Error("expected rlogUploaded = true")
	}
	if s.QlogUploaded {
		t.Error("expected qlogUploaded = false")
	}
	if !s.FcameraUploaded {
		t.Error("expected fcameraUploaded = true")
	}
	if s.EcameraUploaded {
		t.Error("expected ecameraUploaded = false")
	}
	if !s.DcameraUploaded {
		t.Error("expected dcameraUploaded = true")
	}
	if s.QcameraUploaded {
		t.Error("expected qcameraUploaded = false")
	}
}

func TestListRoutes(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name         string
		dongleID     string
		authDongleID string
		queryParams  string
		mock         *routeMockDB
		wantStatus   int
		wantError    string
		wantTotal    int64
		wantCount    int
		wantLimit    int32
		wantOffset   int32
	}{
		{
			name:         "successful listing with defaults",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "",
			mock: &routeMockDB{
				routeCount: 2,
				routes: []db.Route{
					{ID: 1, DongleID: "abc123", RouteName: "2024-03-15--12-30-00",
						StartTime: pgtype.Timestamptz{Time: now, Valid: true},
						EndTime:   pgtype.Timestamptz{Time: now.Add(10 * time.Minute), Valid: true},
						CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}},
					{ID: 2, DongleID: "abc123", RouteName: "2024-03-16--08-00-00",
						StartTime: pgtype.Timestamptz{Time: now, Valid: true},
						EndTime:   pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true},
						CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}},
				},
				segCount: 3,
			},
			wantStatus: http.StatusOK,
			wantTotal:  2,
			wantCount:  2,
			wantLimit:  25,
			wantOffset: 0,
		},
		{
			name:         "custom limit and offset",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=10&offset=5",
			mock: &routeMockDB{
				routeCount: 20,
				routes: []db.Route{
					{ID: 6, DongleID: "abc123", RouteName: "2024-03-20--10-00-00",
						StartTime: pgtype.Timestamptz{Time: now, Valid: true},
						EndTime:   pgtype.Timestamptz{Time: now.Add(10 * time.Minute), Valid: true},
						CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}},
				},
				segCount: 1,
			},
			wantStatus: http.StatusOK,
			wantTotal:  20,
			wantCount:  1,
			wantLimit:  10,
			wantOffset: 5,
		},
		{
			name:         "limit exceeding max resets to default",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=999",
			mock: &routeMockDB{
				routeCount: 0,
				routes:     []db.Route{},
			},
			wantStatus: http.StatusOK,
			wantTotal:  0,
			wantCount:  0,
			wantLimit:  25,
			wantOffset: 0,
		},
		{
			name:         "empty result set",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "",
			mock: &routeMockDB{
				routeCount: 0,
				routes:     []db.Route{},
			},
			wantStatus: http.StatusOK,
			wantTotal:  0,
			wantCount:  0,
			wantLimit:  25,
			wantOffset: 0,
		},
		{
			name:         "dongle_id mismatch",
			dongleID:     "abc123",
			authDongleID: "other999",
			queryParams:  "",
			mock:         &routeMockDB{},
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "invalid limit",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=abc",
			mock:         &routeMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "invalid limit parameter",
		},
		{
			name:         "invalid offset",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "offset=xyz",
			mock:         &routeMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "invalid offset parameter",
		},
		{
			name:         "database error on count",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "",
			mock: &routeMockDB{
				countErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to count routes",
		},
		{
			name:         "database error on list",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "",
			mock: &routeMockDB{
				routeCount: 5,
				routesErr:  fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to list routes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := db.New(tt.mock)
			handler := NewRouteHandler(queries)

			e := echo.New()
			target := "/v1/route/" + tt.dongleID
			if tt.queryParams != "" {
				target += "?" + tt.queryParams
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id")
			c.SetParamValues(tt.dongleID)
			c.Set(middleware.ContextKeyDongleID, tt.authDongleID)

			err := handler.ListRoutes(c)
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantError != "" {
				var body errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse error body: %v", err)
				}
				if !strings.Contains(body.Error, tt.wantError) {
					t.Errorf("error = %q, want substring %q", body.Error, tt.wantError)
				}
				return
			}

			var body routeListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response body: %v", err)
			}

			if body.Total != tt.wantTotal {
				t.Errorf("total = %d, want %d", body.Total, tt.wantTotal)
			}
			if len(body.Routes) != tt.wantCount {
				t.Errorf("len(routes) = %d, want %d", len(body.Routes), tt.wantCount)
			}
			if body.Limit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", body.Limit, tt.wantLimit)
			}
			if body.Offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", body.Offset, tt.wantOffset)
			}
		})
	}
}

func TestListRoutesIncludesMetadata(t *testing.T) {
	now := time.Now()
	endTime := now.Add(10 * time.Minute)

	mock := &routeMockDB{
		routeCount: 1,
		routes: []db.Route{
			{
				ID:        1,
				DongleID:  "abc123",
				RouteName: "2024-03-15--12-30-00",
				StartTime: pgtype.Timestamptz{Time: now, Valid: true},
				EndTime:   pgtype.Timestamptz{Time: endTime, Valid: true},
				CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
			},
		},
		segCount: 5,
	}

	queries := db.New(mock)
	handler := NewRouteHandler(queries)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/route/abc123", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")
	c.Set(middleware.ContextKeyDongleID, "abc123")

	if err := handler.ListRoutes(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body routeListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}

	if len(body.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(body.Routes))
	}

	r := body.Routes[0]
	if r.DongleID != "abc123" {
		t.Errorf("dongleId = %q, want %q", r.DongleID, "abc123")
	}
	if r.RouteName != "2024-03-15--12-30-00" {
		t.Errorf("routeName = %q, want %q", r.RouteName, "2024-03-15--12-30-00")
	}
	if r.StartTime == nil {
		t.Error("expected startTime to be non-nil")
	}
	if r.EndTime == nil {
		t.Error("expected endTime to be non-nil")
	}
	if r.SegmentCount != 5 {
		t.Errorf("segmentCount = %d, want 5", r.SegmentCount)
	}
}

func TestRouteHandlerRegisterRoutes(t *testing.T) {
	mock := &routeMockDB{}
	queries := db.New(mock)
	handler := NewRouteHandler(queries)

	e := echo.New()
	g := e.Group("/v1/route")
	handler.RegisterRoutes(g)

	routes := e.Routes()
	wantPaths := map[string]bool{
		"/v1/route/:dongle_id/:route_name": false,
		"/v1/route/:dongle_id":             false,
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

func TestParseIntParam(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		defaultVal int32
		want       int32
		wantErr    bool
	}{
		{name: "empty returns default", input: "", defaultVal: 25, want: 25},
		{name: "valid number", input: "10", defaultVal: 25, want: 10},
		{name: "zero", input: "0", defaultVal: 25, want: 0},
		{name: "invalid number", input: "abc", defaultVal: 25, wantErr: true},
		{name: "float", input: "1.5", defaultVal: 25, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIntParam(tt.input, tt.defaultVal)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}
