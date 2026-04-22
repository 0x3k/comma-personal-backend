package api

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// TripHandler serves read endpoints for the aggregated trip data written by
// the trip-aggregator-worker. It is intentionally read-only: trip rows are
// produced by the background worker, not by API clients.
type TripHandler struct {
	queries *db.Queries
}

// NewTripHandler creates a TripHandler with the given database queries.
func NewTripHandler(queries *db.Queries) *TripHandler {
	return &TripHandler{queries: queries}
}

// Pagination bounds for GET /v1/devices/:dongle_id/stats. These are local to
// the trip handler so the dashboard-home limits can evolve independently from
// the route-list defaults in route.go.
const (
	defaultStatsLimit = int32(20)
	maxStatsLimit     = int32(100)
)

// tripResponse is the JSON representation of a single trip row with its
// owning route's identifiers. snake_case matches the rest of the device-facing
// API surface (see deviceResponse).
type tripResponse struct {
	ID              int32      `json:"id"`
	DongleID        string     `json:"dongle_id"`
	RouteID         int32      `json:"route_id"`
	RouteName       string     `json:"route_name"`
	StartTime       *time.Time `json:"start_time"`
	DistanceMeters  *float64   `json:"distance_meters"`
	DurationSeconds *int32     `json:"duration_seconds"`
	MaxSpeedMps     *float64   `json:"max_speed_mps"`
	AvgSpeedMps     *float64   `json:"avg_speed_mps"`
	EngagedSeconds  *int32     `json:"engaged_seconds"`
	StartAddress    *string    `json:"start_address"`
	EndAddress      *string    `json:"end_address"`
	StartLat        *float64   `json:"start_lat"`
	StartLng        *float64   `json:"start_lng"`
	EndLat          *float64   `json:"end_lat"`
	EndLng          *float64   `json:"end_lng"`
	ComputedAt      *time.Time `json:"computed_at"`
}

// statsTotals is the lifetime aggregate portion of the stats response.
type statsTotals struct {
	TripCount            int64   `json:"trip_count"`
	TotalDistanceMeters  float64 `json:"total_distance_meters"`
	TotalDurationSeconds int64   `json:"total_duration_seconds"`
	TotalEngagedSeconds  int64   `json:"total_engaged_seconds"`
}

// statsResponse is the JSON body returned by GET /v1/devices/:dongle_id/stats.
type statsResponse struct {
	Totals statsTotals    `json:"totals"`
	Recent []tripResponse `json:"recent"`
	Limit  int32          `json:"limit"`
	Offset int32          `json:"offset"`
}

// GetStats handles GET /v1/devices/:dongle_id/stats. It returns the lifetime
// totals for the device alongside a paginated list of recent trips
// (newest-first). The dashboard-home page consumes this.
func (h *TripHandler) GetStats(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
	}

	limit, err := parseIntParam(c.QueryParam("limit"), defaultStatsLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid limit parameter",
			Code:  http.StatusBadRequest,
		})
	}
	if limit < 1 || limit > maxStatsLimit {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("limit must be between 1 and %d", maxStatsLimit),
			Code:  http.StatusBadRequest,
		})
	}

	offset, err := parseIntParam(c.QueryParam("offset"), 0)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid offset parameter",
			Code:  http.StatusBadRequest,
		})
	}
	if offset < 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "offset must be non-negative",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	totals, err := h.queries.SumTripStatsByDongleID(ctx, dongleID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to compute trip totals",
			Code:  http.StatusInternalServerError,
		})
	}

	recent, err := h.queries.ListTripsByDongleID(ctx, db.ListTripsByDongleIDParams{
		DongleID: dongleID,
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list trips",
			Code:  http.StatusInternalServerError,
		})
	}

	items := make([]tripResponse, 0, len(recent))
	for _, r := range recent {
		items = append(items, tripRowToResponse(r))
	}

	return c.JSON(http.StatusOK, statsResponse{
		Totals: statsTotals{
			TripCount:            totals.TripCount,
			TotalDistanceMeters:  totals.TotalDistance,
			TotalDurationSeconds: totals.TotalDuration,
			TotalEngagedSeconds:  totals.TotalEngaged,
		},
		Recent: items,
		Limit:  limit,
		Offset: offset,
	})
}

// GetTripByRoute handles GET /v1/routes/:dongle_id/:route_name/trip. It
// returns the aggregated Trip row for the named route, or 404 if the
// aggregator has not yet processed the route.
func (h *TripHandler) GetTripByRoute(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
	}

	ctx := c.Request().Context()

	route, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("trip for route %s not found", routeName),
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve route",
			Code:  http.StatusInternalServerError,
		})
	}

	trip, err := h.queries.GetTripByRouteID(ctx, route.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("trip for route %s not found", routeName),
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve trip",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.JSON(http.StatusOK, tripToResponse(trip, route))
}

// RegisterStatsRoute wires GET /devices/:dongle_id/stats on the given group.
// The group is expected to be mounted at /v1 with JWT auth middleware.
func (h *TripHandler) RegisterStatsRoute(g *echo.Group) {
	g.GET("/devices/:dongle_id/stats", h.GetStats)
}

// RegisterTripRoute wires GET /:dongle_id/:route_name/trip on the given
// group. The group is expected to be mounted at /v1/routes with JWT auth
// middleware.
func (h *TripHandler) RegisterTripRoute(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/trip", h.GetTripByRoute)
}

// tripRowToResponse converts a joined ListTripsByDongleIDRow into the wire
// representation, unwrapping nullable pg types into pointers so the JSON
// encodes them as null rather than as pgtype objects.
func tripRowToResponse(r db.ListTripsByDongleIDRow) tripResponse {
	resp := tripResponse{
		ID:        r.ID,
		DongleID:  r.DongleID,
		RouteID:   r.RouteID,
		RouteName: r.RouteName,
	}
	if r.StartTime.Valid {
		t := r.StartTime.Time
		resp.StartTime = &t
	}
	if r.DistanceMeters.Valid {
		v := r.DistanceMeters.Float64
		resp.DistanceMeters = &v
	}
	if r.DurationSeconds.Valid {
		v := r.DurationSeconds.Int32
		resp.DurationSeconds = &v
	}
	if r.MaxSpeedMps.Valid {
		v := r.MaxSpeedMps.Float64
		resp.MaxSpeedMps = &v
	}
	if r.AvgSpeedMps.Valid {
		v := r.AvgSpeedMps.Float64
		resp.AvgSpeedMps = &v
	}
	if r.EngagedSeconds.Valid {
		v := r.EngagedSeconds.Int32
		resp.EngagedSeconds = &v
	}
	if r.StartAddress.Valid {
		v := r.StartAddress.String
		resp.StartAddress = &v
	}
	if r.EndAddress.Valid {
		v := r.EndAddress.String
		resp.EndAddress = &v
	}
	if r.StartLat.Valid {
		v := r.StartLat.Float64
		resp.StartLat = &v
	}
	if r.StartLng.Valid {
		v := r.StartLng.Float64
		resp.StartLng = &v
	}
	if r.EndLat.Valid {
		v := r.EndLat.Float64
		resp.EndLat = &v
	}
	if r.EndLng.Valid {
		v := r.EndLng.Float64
		resp.EndLng = &v
	}
	if r.ComputedAt.Valid {
		t := r.ComputedAt.Time
		resp.ComputedAt = &t
	}
	return resp
}

// tripToResponse converts a bare Trip row into the wire representation,
// decorating it with the dongle_id / route_name / start_time from the owning
// route (so the single-trip endpoint returns the same shape as the list
// endpoint).
func tripToResponse(t db.Trip, r db.Route) tripResponse {
	resp := tripResponse{
		ID:        t.ID,
		DongleID:  r.DongleID,
		RouteID:   t.RouteID,
		RouteName: r.RouteName,
	}
	if r.StartTime.Valid {
		tm := r.StartTime.Time
		resp.StartTime = &tm
	}
	if t.DistanceMeters.Valid {
		v := t.DistanceMeters.Float64
		resp.DistanceMeters = &v
	}
	if t.DurationSeconds.Valid {
		v := t.DurationSeconds.Int32
		resp.DurationSeconds = &v
	}
	if t.MaxSpeedMps.Valid {
		v := t.MaxSpeedMps.Float64
		resp.MaxSpeedMps = &v
	}
	if t.AvgSpeedMps.Valid {
		v := t.AvgSpeedMps.Float64
		resp.AvgSpeedMps = &v
	}
	if t.EngagedSeconds.Valid {
		v := t.EngagedSeconds.Int32
		resp.EngagedSeconds = &v
	}
	if t.StartAddress.Valid {
		v := t.StartAddress.String
		resp.StartAddress = &v
	}
	if t.EndAddress.Valid {
		v := t.EndAddress.String
		resp.EndAddress = &v
	}
	if t.StartLat.Valid {
		v := t.StartLat.Float64
		resp.StartLat = &v
	}
	if t.StartLng.Valid {
		v := t.StartLng.Float64
		resp.StartLng = &v
	}
	if t.EndLat.Valid {
		v := t.EndLat.Float64
		resp.EndLat = &v
	}
	if t.EndLng.Valid {
		v := t.EndLng.Float64
		resp.EndLng = &v
	}
	if t.ComputedAt.Valid {
		tm := t.ComputedAt.Time
		resp.ComputedAt = &tm
	}
	return resp
}
