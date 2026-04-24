package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// Route-annotation limits. These match the CHECK constraints in the
// route-annotations-schema migration so the API rejects bad input before
// the database does -- cheaper, and lets us return a useful error envelope
// rather than a generic 500.
const (
	// maxNoteLen is the maximum byte length of the free-form note on a
	// route. Mirrors the CHECK (char_length(note) <= 4000) constraint
	// added by the annotations migration.
	maxNoteLen = 4000
	// minTagLen / maxTagLen are the bounds on a single tag. Mirror the
	// CHECK constraint on route_tags.tag (1..32 after normalization).
	minTagLen = 1
	maxTagLen = 32
)

// setNoteRequest is the expected JSON body for
// PUT /v1/routes/:dongle_id/:route_name/note.
type setNoteRequest struct {
	Note string `json:"note"`
}

// setStarredRequest is the expected JSON body for
// PUT /v1/routes/:dongle_id/:route_name/starred.
type setStarredRequest struct {
	Starred bool `json:"starred"`
}

// setTagsRequest is the expected JSON body for
// PUT /v1/routes/:dongle_id/:route_name/tags. The wire field stays the
// same as the response shape (a flat array) so the UI can round-trip a
// GET body straight back in.
type setTagsRequest struct {
	Tags []string `json:"tags"`
}

// tagsResponse is the JSON body returned for the two tag GET endpoints.
// Separate from setTagsRequest only to keep request/response types from
// drifting silently.
type tagsResponse struct {
	Tags []string `json:"tags"`
}

// SetNote handles PUT /v1/routes/:dongle_id/:route_name/note.
//
// Authorization funnels through checkDongleAccess so a device JWT cannot
// target another device's route (the route group itself should additionally
// be mounted on the session-only middleware so device JWTs cannot edit user
// annotations at all -- see cmd/server/routes.go).
//
// On over-length input (>maxNoteLen bytes) the handler returns 400 without
// touching the database.
func (h *RouteHandler) SetNote(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	var req setNoteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	if len(req.Note) > maxNoteLen {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("note exceeds maximum length of %d characters", maxNoteLen),
			Code:  http.StatusBadRequest,
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

	if _, err := h.queries.SetRouteNote(ctx, db.SetRouteNoteParams{
		ID:   route.ID,
		Note: req.Note,
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to update note",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// SetStarred handles PUT /v1/routes/:dongle_id/:route_name/starred.
// Session-only at the middleware layer, dongle-scoped via checkDongleAccess.
func (h *RouteHandler) SetStarred(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	var req setStarredRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
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

	if _, err := h.queries.SetRouteStarred(ctx, db.SetRouteStarredParams{
		ID:      route.ID,
		Starred: req.Starred,
	}); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to update starred flag",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// GetRouteTags handles GET /v1/routes/:dongle_id/:route_name/tags and
// returns the tag set for a single route, alphabetically sorted. Accepts
// either a session cookie or a device JWT (shared with the UI and
// share-link flows).
func (h *RouteHandler) GetRouteTags(c echo.Context) error {
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
	return c.JSON(http.StatusOK, tagsResponse{Tags: tags})
}

// SetRouteTags handles PUT /v1/routes/:dongle_id/:route_name/tags. The
// handler:
//
//  1. Lowercases and trims whitespace on every incoming tag.
//  2. Rejects any tag whose normalized length is outside [minTagLen,
//     maxTagLen] with 400 (the "[]" slice and the single-empty-string
//     entry both fall out of this check; an empty slice is a valid
//     "clear all tags" request).
//  3. Silently deduplicates normalized tags.
//  4. Atomically replaces the route's tag set via ReplaceRouteTags.
//
// Session-only at the middleware layer, dongle-scoped via checkDongleAccess.
func (h *RouteHandler) SetRouteTags(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	var req setTagsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	// Normalize and validate before touching the database. Tags are stored
	// lowercased so the UI's autocomplete sees a single canonical form.
	seen := make(map[string]struct{}, len(req.Tags))
	normalized := make([]string, 0, len(req.Tags))
	for _, raw := range req.Tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if len(tag) < minTagLen || len(tag) > maxTagLen {
			return c.JSON(http.StatusBadRequest, errorResponse{
				Error: fmt.Sprintf("tag %q must be between %d and %d characters", raw, minTagLen, maxTagLen),
				Code:  http.StatusBadRequest,
			})
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		normalized = append(normalized, tag)
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

	if err := h.queries.ReplaceRouteTags(ctx, route.ID, normalized); err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to replace tags",
			Code:  http.StatusInternalServerError,
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// ListDeviceTags handles GET /v1/devices/:dongle_id/tags and returns the
// distinct tag set across every route for that device. Backs the
// tag-picker autocomplete in the dashboard. Accepts session-or-JWT.
func (h *RouteHandler) ListDeviceTags(c echo.Context) error {
	dongleID := c.Param("dongle_id")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	ctx := c.Request().Context()

	tags, err := h.queries.ListTagsForDevice(ctx, dongleID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list tags",
			Code:  http.StatusInternalServerError,
		})
	}
	if tags == nil {
		tags = []string{}
	}
	return c.JSON(http.StatusOK, tagsResponse{Tags: tags})
}

// RegisterAnnotationReadRoutes wires the read-only annotation endpoints on
// an Echo group mounted at /v1/routes. The group is expected to carry the
// SessionOrJWT middleware so both dashboard reads and share-link flows
// keep working.
func (h *RouteHandler) RegisterAnnotationReadRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/tags", h.GetRouteTags)
}

// RegisterAnnotationMutationRoutes wires the mutation endpoints on an Echo
// group mounted at /v1/routes. The group is expected to require an operator
// session cookie (NOT a device JWT) so a compromised device cannot edit
// another device's user annotations.
func (h *RouteHandler) RegisterAnnotationMutationRoutes(g *echo.Group) {
	g.PUT("/:dongle_id/:route_name/note", h.SetNote)
	g.PUT("/:dongle_id/:route_name/starred", h.SetStarred)
	g.PUT("/:dongle_id/:route_name/tags", h.SetRouteTags)
}

// RegisterDeviceTagsRoute wires the device-level tag autocomplete endpoint
// on an Echo group mounted at /v1/devices. Accepts session-or-JWT so the
// dashboard and device-JWT callers both work.
func (h *RouteHandler) RegisterDeviceTagsRoute(g *echo.Group) {
	g.GET("/:dongle_id/tags", h.ListDeviceTags)
}
