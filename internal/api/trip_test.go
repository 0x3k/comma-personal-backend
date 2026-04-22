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

// tripMockDB implements db.DBTX for trip handler tests. It dispatches on the
// SQL query string because a single call to GetStats or GetTripByRoute chains
// several different queries (totals, list, route lookup, trip lookup).
type tripMockDB struct {
	// Per-device stats
	totals    db.SumTripStatsByDongleIDRow
	totalsErr error
	trips     []db.ListTripsByDongleIDRow
	tripsErr  error
	// captured pagination args so tests can assert the limit/offset made it
	// down to sqlc.
	lastTripsLimit  int32
	lastTripsOffset int32

	// Per-route lookups
	route         *db.Route
	routeErr      error
	trip          *db.Trip
	tripErr       error
	routeIDLookup int32
}

func (m *tripMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *tripMockDB) Query(_ context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM trips t") && strings.Contains(sql, "JOIN routes") && strings.Contains(sql, "LIMIT") {
		if len(args) >= 3 {
			if l, ok := args[1].(int32); ok {
				m.lastTripsLimit = l
			}
			if o, ok := args[2].(int32); ok {
				m.lastTripsOffset = o
			}
		}
		if m.tripsErr != nil {
			return nil, m.tripsErr
		}
		return &mockTripRows{trips: m.trips}, nil
	}
	return nil, fmt.Errorf("unexpected Query: %s", sql)
}

func (m *tripMockDB) QueryRow(_ context.Context, sql string, args ...interface{}) pgx.Row {
	switch {
	case strings.Contains(sql, "SUM(t.distance_meters)"):
		if m.totalsErr != nil {
			return &mockTotalsRow{err: m.totalsErr}
		}
		return &mockTotalsRow{totals: m.totals}
	case strings.Contains(sql, "FROM routes") && strings.Contains(sql, "WHERE dongle_id = $1 AND route_name = $2"):
		if m.routeErr != nil {
			return &mockTripRouteRow{err: m.routeErr}
		}
		if m.route == nil {
			return &mockTripRouteRow{err: pgx.ErrNoRows}
		}
		return &mockTripRouteRow{route: m.route}
	case strings.Contains(sql, "FROM trips") && strings.Contains(sql, "WHERE route_id = $1"):
		if len(args) >= 1 {
			if id, ok := args[0].(int32); ok {
				m.routeIDLookup = id
			}
		}
		if m.tripErr != nil {
			return &mockTripRow{err: m.tripErr}
		}
		if m.trip == nil {
			return &mockTripRow{err: pgx.ErrNoRows}
		}
		return &mockTripRow{trip: m.trip}
	}
	return &mockTripRow{err: fmt.Errorf("unexpected QueryRow: %s", sql)}
}

// mockTotalsRow scans into the four columns produced by SumTripStatsByDongleID.
type mockTotalsRow struct {
	totals db.SumTripStatsByDongleIDRow
	err    error
}

func (r *mockTotalsRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) < 4 {
		return fmt.Errorf("expected 4 destinations, got %d", len(dest))
	}
	*dest[0].(*float64) = r.totals.TotalDistance
	*dest[1].(*int64) = r.totals.TotalDuration
	*dest[2].(*int64) = r.totals.TotalEngaged
	*dest[3].(*int64) = r.totals.TripCount
	return nil
}

// mockTripRouteRow scans into the eight columns produced by GetRoute.
type mockTripRouteRow struct {
	route *db.Route
	err   error
}

func (r *mockTripRouteRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int32) = r.route.ID
	*dest[1].(*string) = r.route.DongleID
	*dest[2].(*string) = r.route.RouteName
	*dest[3].(*pgtype.Timestamptz) = r.route.StartTime
	*dest[4].(*pgtype.Timestamptz) = r.route.EndTime
	*dest[5].(*interface{}) = r.route.Geometry
	*dest[6].(*pgtype.Timestamptz) = r.route.CreatedAt
	*dest[7].(*bool) = r.route.Preserved
	return nil
}

// mockTripRow scans into the 14 columns produced by GetTripByRouteID.
type mockTripRow struct {
	trip *db.Trip
	err  error
}

func (r *mockTripRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int32) = r.trip.ID
	*dest[1].(*int32) = r.trip.RouteID
	*dest[2].(*pgtype.Float8) = r.trip.DistanceMeters
	*dest[3].(*pgtype.Int4) = r.trip.DurationSeconds
	*dest[4].(*pgtype.Float8) = r.trip.MaxSpeedMps
	*dest[5].(*pgtype.Float8) = r.trip.AvgSpeedMps
	*dest[6].(*pgtype.Int4) = r.trip.EngagedSeconds
	*dest[7].(*pgtype.Text) = r.trip.StartAddress
	*dest[8].(*pgtype.Text) = r.trip.EndAddress
	*dest[9].(*pgtype.Float8) = r.trip.StartLat
	*dest[10].(*pgtype.Float8) = r.trip.StartLng
	*dest[11].(*pgtype.Float8) = r.trip.EndLat
	*dest[12].(*pgtype.Float8) = r.trip.EndLng
	*dest[13].(*pgtype.Timestamptz) = r.trip.ComputedAt
	return nil
}

// mockTripRows iterates the joined ListTripsByDongleIDRow result set.
type mockTripRows struct {
	trips []db.ListTripsByDongleIDRow
	idx   int
}

func (r *mockTripRows) Close()                                       {}
func (r *mockTripRows) Err() error                                   { return nil }
func (r *mockTripRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *mockTripRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockTripRows) RawValues() [][]byte                          { return nil }
func (r *mockTripRows) Conn() *pgx.Conn                              { return nil }

func (r *mockTripRows) Next() bool {
	if r.idx < len(r.trips) {
		r.idx++
		return true
	}
	return false
}

func (r *mockTripRows) Scan(dest ...interface{}) error {
	t := r.trips[r.idx-1]
	*dest[0].(*int32) = t.ID
	*dest[1].(*int32) = t.RouteID
	*dest[2].(*pgtype.Float8) = t.DistanceMeters
	*dest[3].(*pgtype.Int4) = t.DurationSeconds
	*dest[4].(*pgtype.Float8) = t.MaxSpeedMps
	*dest[5].(*pgtype.Float8) = t.AvgSpeedMps
	*dest[6].(*pgtype.Int4) = t.EngagedSeconds
	*dest[7].(*pgtype.Text) = t.StartAddress
	*dest[8].(*pgtype.Text) = t.EndAddress
	*dest[9].(*pgtype.Float8) = t.StartLat
	*dest[10].(*pgtype.Float8) = t.StartLng
	*dest[11].(*pgtype.Float8) = t.EndLat
	*dest[12].(*pgtype.Float8) = t.EndLng
	*dest[13].(*pgtype.Timestamptz) = t.ComputedAt
	*dest[14].(*string) = t.DongleID
	*dest[15].(*string) = t.RouteName
	*dest[16].(*pgtype.Timestamptz) = t.StartTime
	return nil
}

func (r *mockTripRows) Values() ([]interface{}, error) { return nil, nil }

// newTestTripRow builds a fully-populated ListTripsByDongleIDRow for list
// response assertions.
func newTestTripRow(id, routeID int32, dongleID, routeName string, startTime time.Time) db.ListTripsByDongleIDRow {
	return db.ListTripsByDongleIDRow{
		ID:              id,
		RouteID:         routeID,
		DistanceMeters:  pgtype.Float8{Float64: 12345.6, Valid: true},
		DurationSeconds: pgtype.Int4{Int32: 600, Valid: true},
		MaxSpeedMps:     pgtype.Float8{Float64: 30.0, Valid: true},
		AvgSpeedMps:     pgtype.Float8{Float64: 20.0, Valid: true},
		EngagedSeconds:  pgtype.Int4{Int32: 300, Valid: true},
		StartAddress:    pgtype.Text{String: "A", Valid: true},
		EndAddress:      pgtype.Text{String: "B", Valid: true},
		StartLat:        pgtype.Float8{Float64: 1.0, Valid: true},
		StartLng:        pgtype.Float8{Float64: 2.0, Valid: true},
		EndLat:          pgtype.Float8{Float64: 3.0, Valid: true},
		EndLng:          pgtype.Float8{Float64: 4.0, Valid: true},
		ComputedAt:      pgtype.Timestamptz{Time: startTime, Valid: true},
		DongleID:        dongleID,
		RouteName:       routeName,
		StartTime:       pgtype.Timestamptz{Time: startTime, Valid: true},
	}
}

// newTestTripModel builds a fully-populated Trip for single-trip response
// assertions.
func newTestTripModel(id, routeID int32, computedAt time.Time) *db.Trip {
	return &db.Trip{
		ID:              id,
		RouteID:         routeID,
		DistanceMeters:  pgtype.Float8{Float64: 5000.0, Valid: true},
		DurationSeconds: pgtype.Int4{Int32: 900, Valid: true},
		MaxSpeedMps:     pgtype.Float8{Float64: 28.0, Valid: true},
		AvgSpeedMps:     pgtype.Float8{Float64: 18.0, Valid: true},
		EngagedSeconds:  pgtype.Int4{Int32: 450, Valid: true},
		StartAddress:    pgtype.Text{String: "Start", Valid: true},
		EndAddress:      pgtype.Text{String: "End", Valid: true},
		StartLat:        pgtype.Float8{Float64: 37.0, Valid: true},
		StartLng:        pgtype.Float8{Float64: -122.0, Valid: true},
		EndLat:          pgtype.Float8{Float64: 37.1, Valid: true},
		EndLng:          pgtype.Float8{Float64: -122.1, Valid: true},
		ComputedAt:      pgtype.Timestamptz{Time: computedAt, Valid: true},
	}
}

func TestGetStats(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	totals := db.SumTripStatsByDongleIDRow{
		TotalDistance: 123456.789,
		TotalDuration: 7200,
		TotalEngaged:  3600,
		TripCount:     5,
	}

	tests := []struct {
		name         string
		dongleID     string
		authDongleID string
		queryParams  string
		mock         *tripMockDB
		wantStatus   int
		wantError    string
		wantLimit    int32
		wantOffset   int32
		wantRecent   int
	}{
		{
			name:         "happy path with defaults",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &tripMockDB{
				totals: totals,
				trips: []db.ListTripsByDongleIDRow{
					newTestTripRow(1, 10, "abc123", "2024-06-01--11-00-00", now),
					newTestTripRow(2, 11, "abc123", "2024-06-01--10-00-00", now.Add(-time.Hour)),
				},
			},
			wantStatus: http.StatusOK,
			wantLimit:  20,
			wantOffset: 0,
			wantRecent: 2,
		},
		{
			name:         "custom pagination is forwarded to sqlc",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=5&offset=10",
			mock: &tripMockDB{
				totals: totals,
				trips:  []db.ListTripsByDongleIDRow{newTestTripRow(3, 12, "abc123", "2024-06-01--09-00-00", now)},
			},
			wantStatus: http.StatusOK,
			wantLimit:  5,
			wantOffset: 10,
			wantRecent: 1,
		},
		{
			name:         "empty result set still returns totals",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &tripMockDB{
				totals: db.SumTripStatsByDongleIDRow{},
				trips:  []db.ListTripsByDongleIDRow{},
			},
			wantStatus: http.StatusOK,
			wantLimit:  20,
			wantOffset: 0,
			wantRecent: 0,
		},
		{
			name:         "dongle_id mismatch returns 403",
			dongleID:     "abc123",
			authDongleID: "other",
			mock:         &tripMockDB{},
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "invalid limit returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=abc",
			mock:         &tripMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "invalid limit parameter",
		},
		{
			name:         "limit above max returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=101",
			mock:         &tripMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "limit must be between 1 and 100",
		},
		{
			name:         "limit zero returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=0",
			mock:         &tripMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "limit must be between 1 and 100",
		},
		{
			name:         "negative offset returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "offset=-5",
			mock:         &tripMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "offset must be non-negative",
		},
		{
			name:         "invalid offset returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "offset=xyz",
			mock:         &tripMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "invalid offset parameter",
		},
		{
			name:         "totals query error returns 500",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &tripMockDB{
				totalsErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to compute trip totals",
		},
		{
			name:         "trips query error returns 500",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &tripMockDB{
				totals:   totals,
				tripsErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to list trips",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := db.New(tt.mock)
			handler := NewTripHandler(queries)

			e := echo.New()
			target := "/v1/devices/" + tt.dongleID + "/stats"
			if tt.queryParams != "" {
				target += "?" + tt.queryParams
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id")
			c.SetParamValues(tt.dongleID)
			c.Set(middleware.ContextKeyDongleID, tt.authDongleID)

			if err := handler.GetStats(c); err != nil {
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
				if body.Code != tt.wantStatus {
					t.Errorf("error code = %d, want %d", body.Code, tt.wantStatus)
				}
				return
			}

			var body statsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response body: %v", err)
			}

			if body.Totals.TripCount != tt.mock.totals.TripCount {
				t.Errorf("totals.trip_count = %d, want %d", body.Totals.TripCount, tt.mock.totals.TripCount)
			}
			if body.Totals.TotalDistanceMeters != tt.mock.totals.TotalDistance {
				t.Errorf("totals.total_distance_meters = %v, want %v", body.Totals.TotalDistanceMeters, tt.mock.totals.TotalDistance)
			}
			if body.Totals.TotalDurationSeconds != tt.mock.totals.TotalDuration {
				t.Errorf("totals.total_duration_seconds = %d, want %d", body.Totals.TotalDurationSeconds, tt.mock.totals.TotalDuration)
			}
			if body.Totals.TotalEngagedSeconds != tt.mock.totals.TotalEngaged {
				t.Errorf("totals.total_engaged_seconds = %d, want %d", body.Totals.TotalEngagedSeconds, tt.mock.totals.TotalEngaged)
			}
			if body.Limit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", body.Limit, tt.wantLimit)
			}
			if body.Offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", body.Offset, tt.wantOffset)
			}
			if len(body.Recent) != tt.wantRecent {
				t.Errorf("len(recent) = %d, want %d", len(body.Recent), tt.wantRecent)
			}

			if tt.mock.lastTripsLimit != tt.wantLimit {
				t.Errorf("sqlc limit param = %d, want %d", tt.mock.lastTripsLimit, tt.wantLimit)
			}
			if tt.mock.lastTripsOffset != tt.wantOffset {
				t.Errorf("sqlc offset param = %d, want %d", tt.mock.lastTripsOffset, tt.wantOffset)
			}
		})
	}
}

// TestGetStatsRecentFields verifies the list items expose every trip field
// the dashboard needs (distance, duration, addresses, coordinates).
func TestGetStatsRecentFields(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mock := &tripMockDB{
		totals: db.SumTripStatsByDongleIDRow{
			TotalDistance: 100.0, TotalDuration: 60, TotalEngaged: 30, TripCount: 1,
		},
		trips: []db.ListTripsByDongleIDRow{newTestTripRow(42, 7, "abc123", "2024-06-01--11-00-00", now)},
	}

	queries := db.New(mock)
	handler := NewTripHandler(queries)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/stats", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")
	c.Set(middleware.ContextKeyDongleID, "abc123")

	if err := handler.GetStats(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var body statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	if len(body.Recent) != 1 {
		t.Fatalf("expected 1 recent trip, got %d", len(body.Recent))
	}
	got := body.Recent[0]
	if got.ID != 42 || got.RouteID != 7 {
		t.Errorf("ids = %d/%d, want 42/7", got.ID, got.RouteID)
	}
	if got.DongleID != "abc123" {
		t.Errorf("dongle_id = %q, want abc123", got.DongleID)
	}
	if got.RouteName != "2024-06-01--11-00-00" {
		t.Errorf("route_name = %q, want 2024-06-01--11-00-00", got.RouteName)
	}
	if got.DistanceMeters == nil || *got.DistanceMeters == 0 {
		t.Error("expected non-zero distance_meters")
	}
	if got.StartAddress == nil || *got.StartAddress == "" {
		t.Error("expected non-empty start_address")
	}
	if got.StartLat == nil || got.StartLng == nil {
		t.Error("expected start coordinates to be present")
	}
}

func TestGetTripByRoute(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	baseRoute := &db.Route{
		ID:        9,
		DongleID:  "abc123",
		RouteName: "2024-06-01--11-00-00",
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(15 * time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}

	tests := []struct {
		name         string
		dongleID     string
		routeName    string
		authDongleID string
		mock         *tripMockDB
		wantStatus   int
		wantError    string
	}{
		{
			name:         "happy path returns trip",
			dongleID:     "abc123",
			routeName:    "2024-06-01--11-00-00",
			authDongleID: "abc123",
			mock: &tripMockDB{
				route: baseRoute,
				trip:  newTestTripModel(1, 9, now),
			},
			wantStatus: http.StatusOK,
		},
		{
			name:         "route not found returns 404",
			dongleID:     "abc123",
			routeName:    "missing",
			authDongleID: "abc123",
			mock: &tripMockDB{
				routeErr: pgx.ErrNoRows,
			},
			wantStatus: http.StatusNotFound,
			wantError:  "trip for route missing not found",
		},
		{
			name:         "trip row not yet aggregated returns 404",
			dongleID:     "abc123",
			routeName:    "2024-06-01--11-00-00",
			authDongleID: "abc123",
			mock: &tripMockDB{
				route:   baseRoute,
				tripErr: pgx.ErrNoRows,
			},
			wantStatus: http.StatusNotFound,
			wantError:  "trip for route 2024-06-01--11-00-00 not found",
		},
		{
			name:         "dongle_id mismatch returns 403",
			dongleID:     "abc123",
			routeName:    "2024-06-01--11-00-00",
			authDongleID: "other",
			mock:         &tripMockDB{route: baseRoute, trip: newTestTripModel(1, 9, now)},
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "route lookup error returns 500",
			dongleID:     "abc123",
			routeName:    "2024-06-01--11-00-00",
			authDongleID: "abc123",
			mock: &tripMockDB{
				routeErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to retrieve route",
		},
		{
			name:         "trip lookup error returns 500",
			dongleID:     "abc123",
			routeName:    "2024-06-01--11-00-00",
			authDongleID: "abc123",
			mock: &tripMockDB{
				route:   baseRoute,
				tripErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to retrieve trip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := db.New(tt.mock)
			handler := NewTripHandler(queries)

			e := echo.New()
			target := fmt.Sprintf("/v1/routes/%s/%s/trip", tt.dongleID, tt.routeName)
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id", "route_name")
			c.SetParamValues(tt.dongleID, tt.routeName)
			c.Set(middleware.ContextKeyDongleID, tt.authDongleID)

			if err := handler.GetTripByRoute(c); err != nil {
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
				if body.Code != tt.wantStatus {
					t.Errorf("error code = %d, want %d", body.Code, tt.wantStatus)
				}
				return
			}

			var body tripResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response body: %v", err)
			}
			if body.DongleID != tt.dongleID {
				t.Errorf("dongle_id = %q, want %q", body.DongleID, tt.dongleID)
			}
			if body.RouteName != tt.routeName {
				t.Errorf("route_name = %q, want %q", body.RouteName, tt.routeName)
			}
			if body.RouteID != baseRoute.ID {
				t.Errorf("route_id = %d, want %d", body.RouteID, baseRoute.ID)
			}
			if body.DistanceMeters == nil || *body.DistanceMeters == 0 {
				t.Error("expected non-zero distance_meters")
			}
			if body.StartTime == nil {
				t.Error("expected start_time to be populated from the route row")
			}

			// Verify that the trip lookup used the route id from the
			// first query, not the dongle_id.
			if tt.mock.routeIDLookup != baseRoute.ID {
				t.Errorf("trip lookup used route id %d, want %d", tt.mock.routeIDLookup, baseRoute.ID)
			}
		})
	}
}

// TestTripHandlerAuth verifies that both endpoints return 401 without a JWT,
// exercising the whole JWTAuthFromDB middleware path.
func TestTripHandlerAuth(t *testing.T) {
	priv, pubPEM := testDeviceKey(t)
	testDevice := newTestDevice("abc123", "SERIAL001", pubPEM)
	validToken := signDeviceJWT(t, priv, "abc123")

	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	baseRoute := &db.Route{
		ID:        9,
		DongleID:  "abc123",
		RouteName: "2024-06-01--11-00-00",
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(15 * time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}

	// The JWTAuthFromDB middleware needs a DeviceLookup. We use an in-line
	// stub so this test is independent of the unified mockDBTX.
	lookup := stubDeviceLookup{device: testDevice}

	tests := []struct {
		name       string
		path       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "stats: missing auth returns 401",
			path:       "/v1/devices/abc123/stats",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "stats: invalid token returns 401",
			path:       "/v1/devices/abc123/stats",
			authHeader: "JWT invalid.token.here",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "stats: valid token returns 200",
			path:       "/v1/devices/abc123/stats",
			authHeader: "JWT " + validToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "trip: missing auth returns 401",
			path:       "/v1/routes/abc123/2024-06-01--11-00-00/trip",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "trip: invalid token returns 401",
			path:       "/v1/routes/abc123/2024-06-01--11-00-00/trip",
			authHeader: "JWT invalid.token.here",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "trip: valid token returns 200",
			path:       "/v1/routes/abc123/2024-06-01--11-00-00/trip",
			authHeader: "JWT " + validToken,
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &tripMockDB{
				totals: db.SumTripStatsByDongleIDRow{TripCount: 0},
				trips:  []db.ListTripsByDongleIDRow{},
				route:  baseRoute,
				trip:   newTestTripModel(1, baseRoute.ID, now),
			}
			queries := db.New(mock)
			handler := NewTripHandler(queries)

			e := echo.New()
			auth := middleware.JWTAuthFromDB(lookup)
			v1 := e.Group("/v1", auth)
			handler.RegisterStatsRoute(v1)
			v1Routes := e.Group("/v1/routes", auth)
			handler.RegisterTripRoute(v1Routes)

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantStatus == http.StatusUnauthorized {
				var body errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse error body: %v", err)
				}
				if body.Code != http.StatusUnauthorized {
					t.Errorf("error code = %d, want %d", body.Code, http.StatusUnauthorized)
				}
				if body.Error == "" {
					t.Error("expected non-empty error message")
				}
			}
		})
	}
}

// stubDeviceLookup satisfies middleware.DeviceLookup for the auth test. It
// cannot reuse tripMockDB because DeviceLookup takes a dongleID string, not a
// raw SQL query -- and we want the auth test to exercise the real
// JWTAuthFromDB code path.
type stubDeviceLookup struct {
	device *db.Device
	err    error
}

func (s stubDeviceLookup) GetDevice(_ context.Context, _ string) (db.Device, error) {
	if s.err != nil {
		return db.Device{}, s.err
	}
	if s.device == nil {
		return db.Device{}, pgx.ErrNoRows
	}
	return *s.device, nil
}

func TestTripHandlerRegisterRoutes(t *testing.T) {
	mock := &tripMockDB{}
	queries := db.New(mock)
	handler := NewTripHandler(queries)

	e := echo.New()
	v1 := e.Group("/v1")
	handler.RegisterStatsRoute(v1)
	v1Routes := e.Group("/v1/routes")
	handler.RegisterTripRoute(v1Routes)

	wantPaths := map[string]bool{
		"/v1/devices/:dongle_id/stats":           false,
		"/v1/routes/:dongle_id/:route_name/trip": false,
	}
	for _, r := range e.Routes() {
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
