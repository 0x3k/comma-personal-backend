package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
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
	SegmentCount int64             `json:"segmentCount"`
	Segments     []segmentResponse `json:"segments"`
}

// routeListItem is a route summary for listing endpoints.
type routeListItem struct {
	DongleID     string     `json:"dongleId"`
	RouteName    string     `json:"routeName"`
	StartTime    *time.Time `json:"startTime"`
	EndTime      *time.Time `json:"endTime"`
	SegmentCount int64      `json:"segmentCount"`
}

// routeListResponse is the paginated list of routes.
type routeListResponse struct {
	Routes []routeListItem `json:"routes"`
	Total  int64           `json:"total"`
	Limit  int32           `json:"limit"`
	Offset int32           `json:"offset"`
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
		SegmentCount: int64(len(segments)),
		Segments:     segResponses,
	})
}

// ListRoutes handles GET /v1/route/:dongle_id and returns a paginated list of
// routes for the authenticated device. It accepts optional query parameters
// "limit" and "offset" for pagination.
func (h *RouteHandler) ListRoutes(c echo.Context) error {
	dongleID := c.Param("dongle_id")

	authDongleID, _ := c.Get(middleware.ContextKeyDongleID).(string)
	if authDongleID != dongleID {
		return c.JSON(http.StatusForbidden, errorResponse{
			Error: "dongle_id does not match authenticated device",
			Code:  http.StatusForbidden,
		})
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

	ctx := c.Request().Context()

	total, err := h.queries.CountRoutesByDevice(ctx, dongleID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count routes",
			Code:  http.StatusInternalServerError,
		})
	}

	routes, err := h.queries.ListRoutesByDeviceWithCounts(ctx, db.ListRoutesByDevicePaginatedParams{
		DongleID: dongleID,
		Limit:    limit,
		Offset:   offset,
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

		items = append(items, routeListItem{
			DongleID:     r.DongleID,
			RouteName:    r.RouteName,
			StartTime:    startTime,
			EndTime:      endTime,
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

// RegisterRoutes wires up the route endpoints on the given Echo group.
// The group should already have JWT auth middleware applied.
func (h *RouteHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name", h.GetRoute)
	g.GET("/:dongle_id", h.ListRoutes)
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
