package api

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// ExportHandler serves downloadable representations of a route's GPS track.
type ExportHandler struct {
	queries *db.Queries
}

// NewExportHandler creates an ExportHandler with the given database queries.
func NewExportHandler(queries *db.Queries) *ExportHandler {
	return &ExportHandler{queries: queries}
}

// gpxFile is the top-level <gpx> element of a GPX 1.1 document.
//
// The XMLName field forces encoding/xml to emit a <gpx> tag. Version and
// Creator are required by the GPX 1.1 schema; Xmlns declares the default
// namespace so parsers (including Strava, Garmin, and encoding/xml round-trip
// tests) recognise the document.
type gpxFile struct {
	XMLName xml.Name `xml:"gpx"`
	Version string   `xml:"version,attr"`
	Creator string   `xml:"creator,attr"`
	Xmlns   string   `xml:"xmlns,attr"`
	Tracks  []gpxTrk `xml:"trk"`
}

// gpxTrk is a single <trk> (track), representing one route.
type gpxTrk struct {
	Name     string      `xml:"name,omitempty"`
	Segments []gpxTrkseg `xml:"trkseg"`
}

// gpxTrkseg is a <trkseg> (track segment) containing an ordered list of points.
type gpxTrkseg struct {
	Points []gpxTrkpt `xml:"trkpt"`
}

// gpxTrkpt is a <trkpt> (track point). Latitude and longitude are attributes
// per the GPX 1.1 schema.
type gpxTrkpt struct {
	Lat float64 `xml:"lat,attr"`
	Lon float64 `xml:"lon,attr"`
}

// ExportRouteGPX handles GET /v1/routes/:dongle_id/:route_name/export.gpx. It
// loads the route's LineString geometry from PostGIS (as WKT), converts it to
// GPX 1.1, and returns it as an attachment.
//
// Returns 404 when either the route does not exist or the route has no
// geometry attached. Returns 403 when the authenticated device does not match
// the path's dongle_id.
func (h *ExportHandler) ExportRouteGPX(c echo.Context) error {
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

	wkt, err := h.queries.GetRouteGeometryWKT(ctx, db.GetRouteGeometryWKTParams{
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
			Error: "failed to retrieve route geometry",
			Code:  http.StatusInternalServerError,
		})
	}

	if !wkt.Valid || strings.TrimSpace(wkt.String) == "" {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("route %s has no geometry", routeName),
			Code:  http.StatusNotFound,
		})
	}

	points, err := parseLineStringWKT(wkt.String)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: fmt.Sprintf("failed to parse route geometry: %s", err.Error()),
			Code:  http.StatusInternalServerError,
		})
	}
	if len(points) == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: fmt.Sprintf("route %s has no geometry", routeName),
			Code:  http.StatusNotFound,
		})
	}

	doc := gpxFile{
		Version: "1.1",
		Creator: "comma-personal-backend",
		Xmlns:   "http://www.topografix.com/GPX/1/1",
		Tracks: []gpxTrk{{
			Name:     routeName,
			Segments: []gpxTrkseg{{Points: points}},
		}},
	}

	body, err := xml.Marshal(doc)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to encode GPX document",
			Code:  http.StatusInternalServerError,
		})
	}

	payload := []byte(xml.Header)
	payload = append(payload, body...)

	c.Response().Header().Set(echo.HeaderContentType, "application/gpx+xml")
	c.Response().Header().Set(echo.HeaderContentDisposition,
		fmt.Sprintf(`attachment; filename="%s.gpx"`, routeName))
	return c.Blob(http.StatusOK, "application/gpx+xml", payload)
}

// RegisterRoutes wires up the export endpoints on the given Echo group. The
// group should already have the shared session-or-JWT auth middleware applied.
func (h *ExportHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/export.gpx", h.ExportRouteGPX)
}

// parseLineStringWKT extracts an ordered list of (lat, lon) points from a
// PostGIS-rendered WKT string such as "LINESTRING(-122.4 37.7, -122.41 37.71)".
//
// PostGIS emits coordinates in (longitude, latitude) order, matching the
// underlying SRID 4326 convention, while GPX expects lat/lon as separate
// attributes -- the conversion happens here.
//
// Inputs that do not start with "LINESTRING" are rejected. An empty
// "LINESTRING EMPTY" (which PostGIS emits for zero-point geometries) yields
// a zero-length slice with no error, so the caller can translate that into
// a 404.
func parseLineStringWKT(wkt string) ([]gpxTrkpt, error) {
	s := strings.TrimSpace(wkt)
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "LINESTRING") {
		return nil, fmt.Errorf("expected LINESTRING geometry, got %q", truncate(s, 32))
	}
	// Strip the "LINESTRING" prefix (case preserved by using the original
	// length rather than the upper-cased copy).
	rest := strings.TrimSpace(s[len("LINESTRING"):])

	// PostGIS renders an empty geometry as "LINESTRING EMPTY".
	if strings.EqualFold(rest, "EMPTY") {
		return []gpxTrkpt{}, nil
	}

	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return nil, fmt.Errorf("malformed LINESTRING body: %q", truncate(rest, 32))
	}
	body := strings.TrimSpace(rest[1 : len(rest)-1])
	if body == "" {
		return []gpxTrkpt{}, nil
	}

	parts := strings.Split(body, ",")
	points := make([]gpxTrkpt, 0, len(parts))
	for _, raw := range parts {
		coord := strings.Fields(strings.TrimSpace(raw))
		if len(coord) < 2 {
			return nil, fmt.Errorf("malformed coordinate pair: %q", raw)
		}
		lon, err := strconv.ParseFloat(coord[0], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid longitude %q: %w", coord[0], err)
		}
		lat, err := strconv.ParseFloat(coord[1], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid latitude %q: %w", coord[1], err)
		}
		points = append(points, gpxTrkpt{Lat: lat, Lon: lon})
	}
	return points, nil
}

// truncate trims s to at most n runes, appending an ellipsis when truncated.
// Used only to keep error messages bounded when the input is malformed.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
