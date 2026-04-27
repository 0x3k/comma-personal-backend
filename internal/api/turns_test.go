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

// turnsMockDB drives the turns handler's two reads: GetRoute (single
// row, may error pgx.ErrNoRows for 404) and ListTurnsForRoute (rows
// iteration). Other handlers in this package use the same shape.
type turnsMockDB struct {
	route    *db.Route
	routeErr error
	turns    []db.RouteTurn
	turnsErr error
}

func (m *turnsMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *turnsMockDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM route_turns") {
		if m.turnsErr != nil {
			return nil, m.turnsErr
		}
		return &turnsMockRows{turns: m.turns}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func (m *turnsMockDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
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

// turnsMockRows implements pgx.Rows for ListTurnsForRoute. Mirrors the
// scan order in route_turns.sql.go.
type turnsMockRows struct {
	turns []db.RouteTurn
	idx   int
}

func (r *turnsMockRows) Close()                                       {}
func (r *turnsMockRows) Err() error                                   { return nil }
func (r *turnsMockRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *turnsMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *turnsMockRows) RawValues() [][]byte                          { return nil }
func (r *turnsMockRows) Conn() *pgx.Conn                              { return nil }
func (r *turnsMockRows) Values() ([]interface{}, error)               { return nil, nil }

func (r *turnsMockRows) Next() bool {
	if r.idx < len(r.turns) {
		r.idx++
		return true
	}
	return false
}

func (r *turnsMockRows) Scan(dest ...interface{}) error {
	t := r.turns[r.idx-1]
	*dest[0].(*int64) = t.ID
	*dest[1].(*string) = t.DongleID
	*dest[2].(*string) = t.Route
	*dest[3].(*pgtype.Timestamptz) = t.TurnTs
	*dest[4].(*int32) = t.TurnOffsetMs
	*dest[5].(*float32) = t.BearingBeforeDeg
	*dest[6].(*float32) = t.BearingAfterDeg
	*dest[7].(*float32) = t.DeltaDeg
	*dest[8].(*pgtype.Float8) = t.GpsLat
	*dest[9].(*pgtype.Float8) = t.GpsLng
	return nil
}

func newTurnsRequest(t *testing.T, dongle, route, authDongle string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	target := fmt.Sprintf("/v1/routes/%s/%s/turns", dongle, route)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongle, route)
	c.Set(middleware.ContextKeyDongleID, authDongle)
	return rec, c
}

func newTurnsMockRoute(id int32, dongleID, routeName string) *db.Route {
	now := time.Now()
	return &db.Route{
		ID:        id,
		DongleID:  dongleID,
		RouteName: routeName,
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func TestTurns_HappyPath(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-05-01--10-00-00"

	now := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	mock := &turnsMockDB{
		route: newTurnsMockRoute(1, dongle, route),
		turns: []db.RouteTurn{
			{
				ID:               1,
				DongleID:         dongle,
				Route:            route,
				TurnTs:           pgtype.Timestamptz{Time: now.Add(2 * time.Second), Valid: true},
				TurnOffsetMs:     2000,
				BearingBeforeDeg: 90,
				BearingAfterDeg:  180,
				DeltaDeg:         90,
				GpsLat:           pgtype.Float8{Float64: 37.5, Valid: true},
				GpsLng:           pgtype.Float8{Float64: -122.4, Valid: true},
			},
			{
				ID:               2,
				DongleID:         dongle,
				Route:            route,
				TurnTs:           pgtype.Timestamptz{Time: now.Add(7 * time.Second), Valid: true},
				TurnOffsetMs:     7000,
				BearingBeforeDeg: 180,
				BearingAfterDeg:  90,
				DeltaDeg:         -90,
				GpsLat:           pgtype.Float8{Float64: 37.6, Valid: true},
				GpsLng:           pgtype.Float8{Float64: -122.5, Valid: true},
			},
		},
	}
	h := NewTurnsHandler(db.New(mock))
	rec, c := newTurnsRequest(t, dongle, route, dongle)
	if err := h.GetRouteTurns(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body turnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	if len(body.Turns) != 2 {
		t.Fatalf("len(turns) = %d, want 2", len(body.Turns))
	}
	first := body.Turns[0]
	if first.OffsetMs != 2000 {
		t.Errorf("first offset_ms = %d, want 2000", first.OffsetMs)
	}
	if first.DeltaDeg != 90 {
		t.Errorf("first delta_deg = %g, want 90", first.DeltaDeg)
	}
	if first.Lat != 37.5 || first.Lng != -122.4 {
		t.Errorf("first lat/lng = %g/%g, want 37.5/-122.4", first.Lat, first.Lng)
	}
}

func TestTurns_RouteNotFound(t *testing.T) {
	mock := &turnsMockDB{routeErr: pgx.ErrNoRows}
	h := NewTurnsHandler(db.New(mock))
	rec, c := newTurnsRequest(t, "dongle-1", "no-such-route", "dongle-1")
	if err := h.GetRouteTurns(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestTurns_EmptyTurns_Returns200(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-05-01--10-00-00"
	mock := &turnsMockDB{
		route: newTurnsMockRoute(1, dongle, route),
		turns: nil,
	}
	h := NewTurnsHandler(db.New(mock))
	rec, c := newTurnsRequest(t, dongle, route, dongle)
	if err := h.GetRouteTurns(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body turnsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Turns) != 0 {
		t.Errorf("len(turns) = %d, want 0", len(body.Turns))
	}
}

func TestTurns_DongleAccessForbidden(t *testing.T) {
	// JWT-auth caller targeting a different dongle's route. The
	// checkDongleAccess helper must short-circuit with 403 before any
	// query runs.
	const ownerDongle = "owner"
	const otherDongle = "attacker"
	const route = "2024-05-01--10-00-00"
	mock := &turnsMockDB{route: newTurnsMockRoute(1, ownerDongle, route)}
	h := NewTurnsHandler(db.New(mock))
	rec, c := newTurnsRequest(t, ownerDongle, route, otherDongle)
	if err := h.GetRouteTurns(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}
