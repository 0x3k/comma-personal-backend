package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// TurnsHandler serves the per-route turn timeline computed by the
// turn-detector worker. The endpoint is useful beyond ALPR: a future UI
// may render turn markers on the route playback timeline, and the ALPR
// stalking heuristic counts "turns since first sighting of plate X" off
// the same data.
type TurnsHandler struct {
	queries *db.Queries
}

// NewTurnsHandler wires a queries handle into a handler ready to mount
// on an Echo group.
func NewTurnsHandler(queries *db.Queries) *TurnsHandler {
	return &TurnsHandler{queries: queries}
}

// turnEntry is the wire-format payload for one turn. Times use the same
// route-relative offset_ms convention the route metadata worker writes
// for geometry vertices, so the frontend can align the two without an
// extra time-zone dance. ts is the absolute timestamp (ISO 8601) for
// downstream consumers that prefer wall-clock.
type turnEntry struct {
	Ts       string  `json:"ts"`
	OffsetMs int32   `json:"offset_ms"`
	DeltaDeg float32 `json:"delta_deg"`
	Lat      float64 `json:"lat,omitempty"`
	Lng      float64 `json:"lng,omitempty"`
}

type turnsResponse struct {
	Turns []turnEntry `json:"turns"`
}

// GetRouteTurns handles GET /v1/routes/:dongle_id/:route_name/turns.
// Returns 404 when the route does not exist; an empty turns array is
// returned (with 200) when the route exists but has no turns yet (the
// detector hasn't run on it, or it is shorter/sparser than the
// minimums).
func (h *TurnsHandler) GetRouteTurns(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	ctx := c.Request().Context()

	if _, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "route not found",
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to load route",
			Code:  http.StatusInternalServerError,
		})
	}

	rows, err := h.queries.ListTurnsForRoute(ctx, db.ListTurnsForRouteParams{
		DongleID: dongleID,
		Route:    routeName,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list turns",
			Code:  http.StatusInternalServerError,
		})
	}

	out := make([]turnEntry, 0, len(rows))
	for _, r := range rows {
		entry := turnEntry{
			Ts:       r.TurnTs.Time.UTC().Format("2006-01-02T15:04:05.000Z"),
			OffsetMs: r.TurnOffsetMs,
			DeltaDeg: r.DeltaDeg,
		}
		if r.GpsLat.Valid {
			entry.Lat = r.GpsLat.Float64
		}
		if r.GpsLng.Valid {
			entry.Lng = r.GpsLng.Float64
		}
		out = append(out, entry)
	}
	return c.JSON(http.StatusOK, turnsResponse{Turns: out})
}

// RegisterRoutes wires the turns endpoint on the given Echo group. The
// group is expected to apply session-or-JWT auth before this handler
// runs; checkDongleAccess inside GetRouteTurns enforces per-device
// authorization on top of that.
func (h *TurnsHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/turns", h.GetRouteTurns)
}
