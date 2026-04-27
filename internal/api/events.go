package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// EventsHandler serves the "Moments" page reads: a paginated, filterable
// list of detected events across all routes for a device. The heavy lifting
// is done by the event-detector worker; this handler is just a thin SELECT
// wrapper with query-parameter plumbing.
type EventsHandler struct {
	queries *db.Queries
}

// NewEventsHandler constructs an EventsHandler with the given database
// queries.
func NewEventsHandler(queries *db.Queries) *EventsHandler {
	return &EventsHandler{queries: queries}
}

// Pagination bounds for GET /v1/devices/:dongle_id/events. Kept local so
// the moments page can tune its page size independently of the route and
// stats listings.
const (
	defaultEventsLimit = int32(50)
	maxEventsLimit     = int32(500)
)

// eventResponse is the JSON shape for one event row in the list response.
// Field names stay snake_case to match the rest of the device-facing API
// surface (see tripResponse in trip.go).
type eventResponse struct {
	ID                 int32           `json:"id"`
	RouteName          string          `json:"route_name"`
	Type               string          `json:"type"`
	Severity           string          `json:"severity"`
	RouteOffsetSeconds float64         `json:"route_offset_seconds"`
	OccurredAt         *time.Time      `json:"occurred_at"`
	Payload            json.RawMessage `json:"payload"`
}

// eventsListResponse is the paginated envelope returned by the GET handler.
// Total is the full filtered count so the UI can render "N results" /
// pagination controls without another request.
type eventsListResponse struct {
	Events []eventResponse `json:"events"`
	Total  int64           `json:"total"`
	Limit  int32           `json:"limit"`
	Offset int32           `json:"offset"`
}

// ListEvents handles GET /v1/devices/:dongle_id/events and returns a
// paginated list of detected events for the device, newest-first, with
// optional filters on type and severity passed through to sqlc.
func (h *EventsHandler) ListEvents(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	limit, err := parseIntParam(c.QueryParam("limit"), defaultEventsLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid limit parameter",
			Code:  http.StatusBadRequest,
		})
	}
	if limit < 1 || limit > maxEventsLimit {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("limit must be between 1 and %d", maxEventsLimit),
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

	// Optional filters: empty string means "don't filter". pgtype.Text with
	// Valid=false becomes SQL NULL, which the query treats as the no-filter
	// sentinel via the `(... IS NULL OR e.col = ...)` pattern.
	typeFilter := optionalText(c.QueryParam("type"))
	severityFilter := optionalText(c.QueryParam("severity"))
	// route_name lets the moments page lazy-fetch a single route's events
	// when the user expands its row. Exact-match; pass empty to disable.
	routeNameFilter := optionalText(c.QueryParam("route_name"))

	ctx := c.Request().Context()

	total, err := h.queries.CountEventsByDongleID(ctx, db.CountEventsByDongleIDParams{
		DongleID:        dongleID,
		TypeFilter:      typeFilter,
		SeverityFilter:  severityFilter,
		RouteNameFilter: routeNameFilter,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count events",
			Code:  http.StatusInternalServerError,
		})
	}

	rows, err := h.queries.ListEventsByDongleID(ctx, db.ListEventsByDongleIDParams{
		DongleID:        dongleID,
		TypeFilter:      typeFilter,
		SeverityFilter:  severityFilter,
		RouteNameFilter: routeNameFilter,
		LimitCount:      limit,
		OffsetCount:     offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list events",
			Code:  http.StatusInternalServerError,
		})
	}

	events := make([]eventResponse, 0, len(rows))
	for _, r := range rows {
		events = append(events, eventRowToResponse(r))
	}

	return c.JSON(http.StatusOK, eventsListResponse{
		Events: events,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// RegisterRoutes wires the events listing onto the given Echo group. The
// group is expected to be mounted at /v1 and to carry the session-or-JWT
// auth middleware so both dashboard operators and devices can reach it.
func (h *EventsHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/devices/:dongle_id/events", h.ListEvents)
	g.GET("/devices/:dongle_id/event-routes", h.ListEventRoutes)
}

// eventRouteResponse is one row in the moments-page collapsed-by-route
// view: a single route, with its time bounds, the count of events that
// matched the active filter, and the most recent matching event time so
// the UI can sort and label without a follow-up call.
type eventRouteResponse struct {
	RouteName   string           `json:"route_name"`
	StartTime   *time.Time       `json:"start_time"`
	EndTime     *time.Time       `json:"end_time"`
	LastEventAt *time.Time       `json:"last_event_at"`
	EventCount  int64            `json:"event_count"`
	TypeCounts  map[string]int64 `json:"type_counts"`
}

type eventRoutesListResponse struct {
	Routes []eventRouteResponse `json:"routes"`
	Total  int64                `json:"total"`
	Limit  int32                `json:"limit"`
	Offset int32                `json:"offset"`
}

// ListEventRoutes handles GET /v1/devices/:dongle_id/event-routes and
// returns a paginated list of routes that have at least one event matching
// the type/severity filter. Each row includes the per-type breakdown so the
// UI can render summary chips on the collapsed row without expanding it.
func (h *EventsHandler) ListEventRoutes(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	if dongleID == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "dongle_id is required",
			Code:  http.StatusBadRequest,
		})
	}
	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	limit, err := parseIntParam(c.QueryParam("limit"), defaultEventsLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid limit parameter",
			Code:  http.StatusBadRequest,
		})
	}
	if limit < 1 || limit > maxEventsLimit {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("limit must be between 1 and %d", maxEventsLimit),
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

	typeFilter := optionalText(c.QueryParam("type"))
	severityFilter := optionalText(c.QueryParam("severity"))
	ctx := c.Request().Context()

	total, err := h.queries.CountRoutesWithEventsByDongleID(ctx, db.CountRoutesWithEventsByDongleIDParams{
		DongleID:       dongleID,
		TypeFilter:     typeFilter,
		SeverityFilter: severityFilter,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count event routes",
			Code:  http.StatusInternalServerError,
		})
	}

	rows, err := h.queries.ListRoutesWithEventsByDongleID(ctx, db.ListRoutesWithEventsByDongleIDParams{
		DongleID:       dongleID,
		TypeFilter:     typeFilter,
		SeverityFilter: severityFilter,
		LimitCount:     limit,
		OffsetCount:    offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list event routes",
			Code:  http.StatusInternalServerError,
		})
	}

	// Collect ids so we can fetch the per-route type breakdown in one shot.
	routeIDs := make([]int32, 0, len(rows))
	for _, r := range rows {
		routeIDs = append(routeIDs, r.RouteID)
	}
	breakdownByRoute := make(map[int32]map[string]int64, len(rows))
	if len(routeIDs) > 0 {
		breakdownRows, err := h.queries.ListEventTypeBreakdownByDongleID(ctx, db.ListEventTypeBreakdownByDongleIDParams{
			DongleID:       dongleID,
			TypeFilter:     typeFilter,
			SeverityFilter: severityFilter,
			RouteIds:       routeIDs,
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "failed to load type breakdown",
				Code:  http.StatusInternalServerError,
			})
		}
		for _, b := range breakdownRows {
			m, ok := breakdownByRoute[b.RouteID]
			if !ok {
				m = make(map[string]int64)
				breakdownByRoute[b.RouteID] = m
			}
			m[b.Type] = b.Count
		}
	}

	out := make([]eventRouteResponse, 0, len(rows))
	for _, r := range rows {
		row := eventRouteResponse{
			RouteName:  r.RouteName,
			EventCount: r.EventCount,
			TypeCounts: breakdownByRoute[r.RouteID],
		}
		if row.TypeCounts == nil {
			row.TypeCounts = map[string]int64{}
		}
		if r.StartTime.Valid {
			t := r.StartTime.Time
			row.StartTime = &t
		}
		if r.EndTime.Valid {
			t := r.EndTime.Time
			row.EndTime = &t
		}
		if r.LastEventAt.Valid {
			t := r.LastEventAt.Time
			row.LastEventAt = &t
		}
		out = append(out, row)
	}

	return c.JSON(http.StatusOK, eventRoutesListResponse{
		Routes: out,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// eventRowToResponse maps the sqlc join row into the wire shape. Nullable
// columns (occurred_at, payload) flow through as JSON null; non-null but
// empty payloads still encode as null so the frontend has a single shape to
// handle.
func eventRowToResponse(r db.ListEventsByDongleIDRow) eventResponse {
	resp := eventResponse{
		ID:                 r.ID,
		RouteName:          r.RouteName,
		Type:               r.Type,
		Severity:           r.Severity,
		RouteOffsetSeconds: r.RouteOffsetSeconds,
	}
	if r.OccurredAt.Valid {
		t := r.OccurredAt.Time
		resp.OccurredAt = &t
	}
	if len(r.Payload) > 0 {
		// Stored as JSONB; pass through unparsed so clients see the exact
		// detector output without an intermediate encode/decode round trip.
		resp.Payload = json.RawMessage(r.Payload)
	}
	return resp
}

// optionalText maps an empty filter query param to a SQL NULL (sqlc
// pgtype.Text with Valid=false), which the underlying query treats as "no
// filter" thanks to the `($n IS NULL OR col = $n)` idiom.
func optionalText(v string) pgtype.Text {
	if v == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: v, Valid: true}
}
