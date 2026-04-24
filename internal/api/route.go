package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// RouteHandler holds the dependencies for route-related HTTP handlers.
type RouteHandler struct {
	queries *db.Queries
}

// NewRouteHandler creates a RouteHandler with the given database queries.
func NewRouteHandler(queries *db.Queries) *RouteHandler {
	return &RouteHandler{queries: queries}
}

// segmentResponse is the JSON representation of a single segment.
type segmentResponse struct {
	Number          int32 `json:"number"`
	RlogUploaded    bool  `json:"rlogUploaded"`
	QlogUploaded    bool  `json:"qlogUploaded"`
	FcameraUploaded bool  `json:"fcameraUploaded"`
	EcameraUploaded bool  `json:"ecameraUploaded"`
	DcameraUploaded bool  `json:"dcameraUploaded"`
	QcameraUploaded bool  `json:"qcameraUploaded"`
}

// routeDetailResponse is the JSON representation of a route with its segments.
type routeDetailResponse struct {
	DongleID     string            `json:"dongleId"`
	RouteName    string            `json:"routeName"`
	StartTime    *time.Time        `json:"startTime"`
	EndTime      *time.Time        `json:"endTime"`
	Preserved    bool              `json:"preserved"`
	Note         string            `json:"note"`
	Starred      bool              `json:"starred"`
	Tags         []string          `json:"tags"`
	SegmentCount int64             `json:"segmentCount"`
	Segments     []segmentResponse `json:"segments"`
}

// routeListItem is a route summary for listing endpoints.
type routeListItem struct {
	DongleID     string     `json:"dongleId"`
	RouteName    string     `json:"routeName"`
	StartTime    *time.Time `json:"startTime"`
	EndTime      *time.Time `json:"endTime"`
	Preserved    bool       `json:"preserved"`
	Note         string     `json:"note"`
	Starred      bool       `json:"starred"`
	Tags         []string   `json:"tags"`
	SegmentCount int64      `json:"segmentCount"`
}

// routeListResponse is the paginated list of routes.
type routeListResponse struct {
	Routes []routeListItem `json:"routes"`
	Total  int64           `json:"total"`
	Limit  int32           `json:"limit"`
	Offset int32           `json:"offset"`
}

// setPreservedRequest is the expected JSON body for
// PUT /v1/routes/:dongle_id/:route_name/preserved.
type setPreservedRequest struct {
	Preserved bool `json:"preserved"`
}

const (
	defaultLimit = int32(25)
	maxLimit     = int32(100)
)

// GetRoute handles GET /v1/route/:dongle_id/:route_name and returns the route
// details including the full segment list with upload status.
func (h *RouteHandler) GetRoute(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	ctx := c.Request().Context()

	route, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("route %s not found", routeName),
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve route",
			Code:  http.StatusInternalServerError,
		})
	}

	segments, err := h.queries.ListSegmentsByRoute(ctx, route.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve segments",
			Code:  http.StatusInternalServerError,
		})
	}

	tags, err := h.queries.ListTagsForRoute(ctx, route.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to retrieve tags",
			Code:  http.StatusInternalServerError,
		})
	}
	if tags == nil {
		tags = []string{}
	}

	segResponses := make([]segmentResponse, 0, len(segments))
	for _, s := range segments {
		segResponses = append(segResponses, segmentResponse{
			Number:          s.SegmentNumber,
			RlogUploaded:    s.RlogUploaded,
			QlogUploaded:    s.QlogUploaded,
			FcameraUploaded: s.FcameraUploaded,
			EcameraUploaded: s.EcameraUploaded,
			DcameraUploaded: s.DcameraUploaded,
			QcameraUploaded: s.QcameraUploaded,
		})
	}

	var startTime, endTime *time.Time
	if route.StartTime.Valid {
		startTime = &route.StartTime.Time
	}
	if route.EndTime.Valid {
		endTime = &route.EndTime.Time
	}

	return c.JSON(http.StatusOK, routeDetailResponse{
		DongleID:     route.DongleID,
		RouteName:    route.RouteName,
		StartTime:    startTime,
		EndTime:      endTime,
		Preserved:    route.Preserved,
		Note:         route.Note,
		Starred:      route.Starred,
		Tags:         tags,
		SegmentCount: int64(len(segments)),
		Segments:     segResponses,
	})
}

// knownListRoutesParams is the set of query-string keys accepted by
// ListRoutes. Any other key triggers a 400; we want typos (e.g. "sorted")
// to fail loudly rather than silently be ignored.
var knownListRoutesParams = map[string]struct{}{
	"limit":          {},
	"offset":         {},
	"from":           {},
	"to":             {},
	"min_duration_s": {},
	"max_duration_s": {},
	"min_distance_m": {},
	"max_distance_m": {},
	"preserved":      {},
	"has_events":     {},
	"starred":        {},
	"tag":            {},
	"sort":           {},
}

// validSortKeys is the whitelist of route-list sort orders the handler
// accepts. Values map 1:1 to db.RouteListSortKey constants.
var validSortKeys = map[string]db.RouteListSortKey{
	"date_desc":     db.RouteListSortDateDesc,
	"date_asc":      db.RouteListSortDateAsc,
	"duration_desc": db.RouteListSortDurationDesc,
	"distance_desc": db.RouteListSortDistanceDesc,
}

// ListRoutes handles GET /v1/route/:dongle_id and returns a paginated list
// of routes for the authenticated device. Filter and sort query parameters
// are optional; an un-parameterized call returns the same shape as before
// (date_desc, no filters, page 0).
//
// Supported query parameters:
//   - from, to                    -- RFC3339 timestamps; r.start_time >= from, < to
//   - min_duration_s, max_duration_s -- int seconds against trips.duration_seconds
//   - min_distance_m, max_distance_m -- int meters against trips.distance_meters
//   - preserved                   -- bool, filters by routes.preserved
//   - has_events                  -- bool, filters by EXISTS/NOT EXISTS in events
//   - starred                     -- bool, filters by routes.starred
//   - tag                         -- repeatable; ?tag=a&tag=b means the route
//     must have BOTH tags (AND semantics, not
//     OR). Values are lowercased/trimmed on the
//     server before matching so the wire casing
//     does not have to align with stored casing.
//   - sort                        -- one of date_desc, date_asc, duration_desc, distance_desc
//   - limit, offset               -- pagination (unchanged from the pre-filter behavior)
//
// Any other query key is rejected with 400 so unknown or misspelled
// parameters fail loudly. Routes without an aggregated trip row are
// included in the result set unless a duration or distance filter is set,
// in which case they are excluded (the trip columns are NULL and cannot
// satisfy the range check).
//
// An unknown tag (one not attached to any of this device's routes)
// collapses the result set to empty without raising an error -- the EXISTS
// subquery simply matches nothing.
func (h *RouteHandler) ListRoutes(c echo.Context) error {
	dongleID := c.Param("dongle_id")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	// Reject unknown query params early so typos don't silently degrade to
	// the default behavior. The filter set here is an explicit contract.
	for key := range c.QueryParams() {
		if _, ok := knownListRoutesParams[key]; !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: fmt.Sprintf("unknown query parameter %q", key),
				Code:  http.StatusBadRequest,
			})
		}
	}

	limit, err := parseIntParam(c.QueryParam("limit"), defaultLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid limit parameter",
			Code:  http.StatusBadRequest,
		})
	}
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	offset, err := parseIntParam(c.QueryParam("offset"), 0)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid offset parameter",
			Code:  http.StatusBadRequest,
		})
	}
	if offset < 0 {
		offset = 0
	}

	fromTime, err := parseTimestamptzParam(c.QueryParam("from"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid from parameter (expected RFC3339 timestamp)",
			Code:  http.StatusBadRequest,
		})
	}
	toTime, err := parseTimestamptzParam(c.QueryParam("to"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid to parameter (expected RFC3339 timestamp)",
			Code:  http.StatusBadRequest,
		})
	}

	minDurationS, err := parseInt4Param(c.QueryParam("min_duration_s"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid min_duration_s parameter",
			Code:  http.StatusBadRequest,
		})
	}
	maxDurationS, err := parseInt4Param(c.QueryParam("max_duration_s"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid max_duration_s parameter",
			Code:  http.StatusBadRequest,
		})
	}
	minDistanceM, err := parseFloat8Param(c.QueryParam("min_distance_m"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid min_distance_m parameter",
			Code:  http.StatusBadRequest,
		})
	}
	maxDistanceM, err := parseFloat8Param(c.QueryParam("max_distance_m"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid max_distance_m parameter",
			Code:  http.StatusBadRequest,
		})
	}

	preserved, err := parseBoolParam(c.QueryParam("preserved"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid preserved parameter (expected true or false)",
			Code:  http.StatusBadRequest,
		})
	}
	hasEvents, err := parseBoolParam(c.QueryParam("has_events"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid has_events parameter (expected true or false)",
			Code:  http.StatusBadRequest,
		})
	}
	starred, err := parseBoolParam(c.QueryParam("starred"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "invalid starred parameter (expected true or false)",
			Code:  http.StatusBadRequest,
		})
	}

	// Repeatable ?tag=a&tag=b means "has ALL of these tags" (AND). Values
	// are normalized the same way SetRouteTags normalizes writes so users
	// do not have to match the stored casing verbatim. Duplicate tags in
	// the querystring are deduped here so the SQL never grows pointless
	// extra EXISTS clauses for the same tag.
	rawTags := c.QueryParams()["tag"]
	tags := normalizeTagFilter(rawTags)

	sortKey := db.RouteListSortDateDesc
	if raw := c.QueryParam("sort"); raw != "" {
		v, ok := validSortKeys[raw]
		if !ok {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: "invalid sort parameter (expected date_desc, date_asc, duration_desc, or distance_desc)",
				Code:  http.StatusBadRequest,
			})
		}
		sortKey = v
	}

	ctx := c.Request().Context()

	total, err := h.queries.CountRoutesByDeviceFilteredCustom(ctx, db.CountRoutesByDeviceFilteredCustomParams{
		DongleID:     dongleID,
		FromTime:     fromTime,
		ToTime:       toTime,
		Preserved:    preserved,
		MinDurationS: minDurationS,
		MaxDurationS: maxDurationS,
		MinDistanceM: minDistanceM,
		MaxDistanceM: maxDistanceM,
		HasEvents:    hasEvents,
		Starred:      starred,
		Tags:         tags,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count routes",
			Code:  http.StatusInternalServerError,
		})
	}

	routes, err := h.queries.ListRoutesByDeviceFiltered(ctx, db.ListRoutesByDeviceFilteredParams{
		DongleID:     dongleID,
		FromTime:     fromTime,
		ToTime:       toTime,
		Preserved:    preserved,
		MinDurationS: minDurationS,
		MaxDurationS: maxDurationS,
		MinDistanceM: minDistanceM,
		MaxDistanceM: maxDistanceM,
		HasEvents:    hasEvents,
		Starred:      starred,
		Tags:         tags,
		Sort:         sortKey,
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list routes",
			Code:  http.StatusInternalServerError,
		})
	}

	items := make([]routeListItem, 0, len(routes))
	for _, r := range routes {
		var startTime, endTime *time.Time
		if r.StartTime.Valid {
			startTime = &r.StartTime.Time
		}
		if r.EndTime.Valid {
			endTime = &r.EndTime.Time
		}

		tags := r.Tags
		if tags == nil {
			tags = []string{}
		}
		items = append(items, routeListItem{
			DongleID:     r.DongleID,
			RouteName:    r.RouteName,
			StartTime:    startTime,
			EndTime:      endTime,
			Preserved:    r.Preserved,
			Note:         r.Note,
			Starred:      r.Starred,
			Tags:         tags,
			SegmentCount: r.SegmentCount,
		})
	}

	return c.JSON(http.StatusOK, routeListResponse{
		Routes: items,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// SetPreserved handles PUT /v1/routes/:dongle_id/:route_name/preserved and
// toggles the preserved flag on the route. Preserved routes are exempt from
// automatic cleanup.
func (h *RouteHandler) SetPreserved(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	var req setPreservedRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	route, err := h.queries.SetRoutePreserved(ctx, db.SetRoutePreservedParams{
		DongleID:  dongleID,
		RouteName: routeName,
		Preserved: req.Preserved,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: fmt.Sprintf("route %s not found", routeName),
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to update preserved flag",
			Code:  http.StatusInternalServerError,
		})
	}

	var startTime, endTime *time.Time
	if route.StartTime.Valid {
		startTime = &route.StartTime.Time
	}
	if route.EndTime.Valid {
		endTime = &route.EndTime.Time
	}

	return c.JSON(http.StatusOK, routeListItem{
		DongleID:  route.DongleID,
		RouteName: route.RouteName,
		StartTime: startTime,
		EndTime:   endTime,
		Preserved: route.Preserved,
		Note:      route.Note,
		Starred:   route.Starred,
		Tags:      []string{},
	})
}

// RegisterRoutes wires up the route endpoints on the given Echo group.
// The group should already have JWT auth middleware applied.
func (h *RouteHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name", h.GetRoute)
	g.GET("/:dongle_id", h.ListRoutes)
}

// RegisterPreservedRoute wires up the preserve-toggle endpoint on an Echo
// group mounted at /v1/routes. The group should already have JWT auth
// middleware applied.
func (h *RouteHandler) RegisterPreservedRoute(g *echo.Group) {
	g.PUT("/:dongle_id/:route_name/preserved", h.SetPreserved)
}

// parseIntParam parses a string as an int32, returning the default value if the
// string is empty.
func parseIntParam(s string, defaultVal int32) (int32, error) {
	if s == "" {
		return defaultVal, nil
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("failed to parse integer %q: %w", s, err)
	}
	return int32(v), nil
}

// parseTimestamptzParam parses an RFC3339 timestamp into a pgtype.Timestamptz.
// An empty string yields an invalid (NULL) value so the db layer can skip the
// filter entirely. strict: only RFC3339 is accepted; other common date-only
// formats are rejected to keep the wire contract explicit.
func parseTimestamptzParam(s string) (pgtype.Timestamptz, error) {
	if s == "" {
		return pgtype.Timestamptz{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return pgtype.Timestamptz{}, fmt.Errorf("failed to parse RFC3339 timestamp %q: %w", s, err)
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, nil
}

// parseInt4Param parses a string as a pgtype.Int4. An empty string yields an
// invalid (NULL) value so the db layer can skip the filter entirely.
func parseInt4Param(s string) (pgtype.Int4, error) {
	if s == "" {
		return pgtype.Int4{}, nil
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return pgtype.Int4{}, fmt.Errorf("failed to parse int32 %q: %w", s, err)
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}, nil
}

// parseFloat8Param parses a string as a pgtype.Float8. An empty string yields
// an invalid (NULL) value so the db layer can skip the filter entirely. The
// handler exposes meters as an integer on the wire but stores them as
// DOUBLE PRECISION, so we accept both integer and float forms here.
func parseFloat8Param(s string) (pgtype.Float8, error) {
	if s == "" {
		return pgtype.Float8{}, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return pgtype.Float8{}, fmt.Errorf("failed to parse float64 %q: %w", s, err)
	}
	return pgtype.Float8{Float64: v, Valid: true}, nil
}

// parseBoolParam parses a string as a pgtype.Bool. An empty string yields an
// invalid (NULL) value so the db layer can skip the filter entirely. We
// accept only the strict Go strconv forms ("true"/"false"/"1"/"0") rather
// than the broader echo-style "yes"/"no" spellings to keep the contract
// obvious.
func parseBoolParam(s string) (pgtype.Bool, error) {
	if s == "" {
		return pgtype.Bool{}, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return pgtype.Bool{}, fmt.Errorf("failed to parse boolean %q: %w", s, err)
	}
	return pgtype.Bool{Bool: v, Valid: true}, nil
}

// normalizeTagFilter lowercases, trims, and deduplicates tag values from the
// repeated ?tag=a&tag=b query parameter. Empty entries (after trimming) are
// dropped so "?tag=&tag=road-trip" collapses to just "road-trip" rather
// than raising a validation error -- the list-list filter is forgiving by
// design (the mutation endpoint is where strict validation lives). The
// resulting order is stable: each tag appears in the position of its first
// occurrence.
func normalizeTagFilter(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		tag := strings.ToLower(strings.TrimSpace(r))
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
