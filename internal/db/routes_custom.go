package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// RouteWithSegmentCount extends Route with an inline segment count, the
// annotation fields (note, starred), and the list of tags attached to the
// route. All of that is gathered in a single query so the list endpoint
// can surface annotations without N+1 round-trips.
type RouteWithSegmentCount struct {
	ID           int32
	DongleID     string
	RouteName    string
	StartTime    pgtype.Timestamptz
	EndTime      pgtype.Timestamptz
	Geometry     interface{}
	CreatedAt    pgtype.Timestamptz
	Preserved    bool
	Note         string
	Starred      bool
	Tags         []string
	SegmentCount int64
}

// listRoutesByDeviceWithCounts pulls the annotation fields straight off the
// routes row and builds the tags array via a LATERAL subquery. COALESCE
// turns an empty aggregation into an empty text[] (rather than NULL) so
// pgx scans cleanly into []string.
const listRoutesByDeviceWithCounts = `
SELECT r.id, r.dongle_id, r.route_name, r.start_time, r.end_time, r.geometry, r.created_at, r.preserved,
       r.note, r.starred,
       COALESCE(tags.tags, ARRAY[]::text[]) AS tags,
       (SELECT count(*) FROM segments s WHERE s.route_id = r.id) AS segment_count
FROM routes r
LEFT JOIN LATERAL (
    SELECT ARRAY_AGG(rt.tag ORDER BY rt.tag) AS tags
    FROM route_tags rt
    WHERE rt.route_id = r.id
) tags ON TRUE
WHERE r.dongle_id = $1
ORDER BY r.created_at DESC, r.id DESC
LIMIT $2 OFFSET $3
`

// ListRoutesByDeviceWithCounts returns paginated routes for a device, each
// annotated with its segment count, note, starred flag, and tag list in a
// single query (no N+1).
func (q *Queries) ListRoutesByDeviceWithCounts(ctx context.Context, arg ListRoutesByDevicePaginatedParams) ([]RouteWithSegmentCount, error) {
	rows, err := q.db.Query(ctx, listRoutesByDeviceWithCounts, arg.DongleID, arg.Limit, arg.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []RouteWithSegmentCount
	for rows.Next() {
		var i RouteWithSegmentCount
		if err := rows.Scan(
			&i.ID,
			&i.DongleID,
			&i.RouteName,
			&i.StartTime,
			&i.EndTime,
			&i.Geometry,
			&i.CreatedAt,
			&i.Preserved,
			&i.Note,
			&i.Starred,
			&i.Tags,
			&i.SegmentCount,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ReplaceRouteTags atomically swaps the tag set on a route: it clears every
// existing tag and inserts the given replacements inside a single
// transaction. Duplicate or already-normalized tags in `tags` are fine --
// the INSERT uses ON CONFLICT DO NOTHING so callers do not need to de-dup
// beforehand. An empty `tags` slice simply clears the route's tag set.
//
// This wrapper is written by hand because sqlc 1.x has no native support
// for looping over a slice inside a single generated function; splitting
// the delete and insert into two generated queries and composing them
// here keeps the SQL in sql/queries/ and the transaction boundary in Go.
func (q *Queries) ReplaceRouteTags(ctx context.Context, routeID int32, tags []string) error {
	pool, ok := q.db.(interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("db handle does not support transactions")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := q.WithTx(tx)

	if err := qtx.ReplaceRouteTagsDelete(ctx, routeID); err != nil {
		return fmt.Errorf("clear tags: %w", err)
	}
	for _, tag := range tags {
		if err := qtx.AddRouteTag(ctx, AddRouteTagParams{RouteID: routeID, Tag: tag}); err != nil {
			return fmt.Errorf("insert tag %q: %w", tag, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

const getRouteGeometryWKT = `
SELECT ST_AsText(geometry)
FROM routes
WHERE dongle_id = $1 AND route_name = $2
`

const listRoutesForTripAggregation = `
SELECT r.id, r.dongle_id, r.route_name, r.start_time, r.end_time, r.geometry,
       r.created_at, r.preserved,
       seg.latest_segment_at,
       seg.segment_count
FROM routes r
LEFT JOIN LATERAL (
    SELECT MAX(s.created_at) AS latest_segment_at,
           COUNT(*)          AS segment_count
    FROM segments s
    WHERE s.route_id = r.id
) seg ON TRUE
LEFT JOIN trips t ON t.route_id = r.id
WHERE seg.segment_count > 0
  AND (seg.latest_segment_at IS NULL OR seg.latest_segment_at < $1)
  AND (
        t.id IS NULL
        OR t.computed_at IS NULL
        OR t.computed_at < seg.latest_segment_at
      )
ORDER BY r.created_at ASC, r.id ASC
LIMIT $2
`

// RouteForTripAggregation is the subset of a route row needed to decide
// whether a trip should be (re)computed, plus the latest segment timestamp
// used for the "finalized" check and the stale computed_at comparison.
type RouteForTripAggregation struct {
	ID              int32
	DongleID        string
	RouteName       string
	StartTime       pgtype.Timestamptz
	EndTime         pgtype.Timestamptz
	Geometry        interface{}
	CreatedAt       pgtype.Timestamptz
	Preserved       bool
	LatestSegmentAt pgtype.Timestamptz
	SegmentCount    int64
}

// ListRoutesForTripAggregationParams selects routes whose most recent
// segment arrived before FinalizedBefore, and that either have no trip
// yet or have a trip whose computed_at predates that segment.
type ListRoutesForTripAggregationParams struct {
	FinalizedBefore pgtype.Timestamptz
	Limit           int32
}

// ListRoutesForTripAggregation returns routes that are ready to have a
// trip summary computed or refreshed. A route is considered "finalized"
// when its latest segment upload is older than FinalizedBefore (i.e. no
// new segments have arrived for a while).
func (q *Queries) ListRoutesForTripAggregation(ctx context.Context, arg ListRoutesForTripAggregationParams) ([]RouteForTripAggregation, error) {
	rows, err := q.db.Query(ctx, listRoutesForTripAggregation, arg.FinalizedBefore, arg.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []RouteForTripAggregation
	for rows.Next() {
		var i RouteForTripAggregation
		if err := rows.Scan(
			&i.ID,
			&i.DongleID,
			&i.RouteName,
			&i.StartTime,
			&i.EndTime,
			&i.Geometry,
			&i.CreatedAt,
			&i.Preserved,
			&i.LatestSegmentAt,
			&i.SegmentCount,
		); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const getRouteGeometryStats = `
SELECT
    ST_NumPoints(geometry)                                       AS num_points,
    ST_Length(geometry::geography)                               AS distance_meters,
    ST_X(ST_PointN(geometry, 1))                                 AS start_lng,
    ST_Y(ST_PointN(geometry, 1))                                 AS start_lat,
    ST_X(ST_PointN(geometry, ST_NumPoints(geometry)))            AS end_lng,
    ST_Y(ST_PointN(geometry, ST_NumPoints(geometry)))            AS end_lat
FROM routes
WHERE id = $1
  AND geometry IS NOT NULL
  AND NOT ST_IsEmpty(geometry)
  AND ST_NumPoints(geometry) > 1
`

// RouteGeometryStats holds PostGIS-derived aggregate stats about a route's
// GPS track. Populated only when the route has a non-empty LineString with
// at least two points.
type RouteGeometryStats struct {
	NumPoints      int32
	DistanceMeters float64
	StartLng       float64
	StartLat       float64
	EndLng         float64
	EndLat         float64
}

// GetRouteGeometryStats returns distance and endpoint coordinates for the
// given route. Returns pgx.ErrNoRows when the route has no usable geometry
// (missing, empty, or a single-point track).
func (q *Queries) GetRouteGeometryStats(ctx context.Context, routeID int32) (RouteGeometryStats, error) {
	row := q.db.QueryRow(ctx, getRouteGeometryStats, routeID)
	var s RouteGeometryStats
	if err := row.Scan(
		&s.NumPoints,
		&s.DistanceMeters,
		&s.StartLng,
		&s.StartLat,
		&s.EndLng,
		&s.EndLat,
	); err != nil {
		return RouteGeometryStats{}, err
	}
	return s, nil
}

const getRouteGeometrySegmentMaxLength = `
SELECT COALESCE(MAX(
    ST_Distance(
        ST_PointN(geometry, g.n)::geography,
        ST_PointN(geometry, g.n + 1)::geography
    )
), 0)
FROM routes r,
     generate_series(1, ST_NumPoints(r.geometry) - 1) AS g(n)
WHERE r.id = $1
  AND r.geometry IS NOT NULL
  AND NOT ST_IsEmpty(r.geometry)
  AND ST_NumPoints(r.geometry) > 1
`

// GetRouteGeometrySegmentMaxLength returns the longest distance between
// consecutive points in the route's LineString, in meters. Paired with a
// uniform per-segment time budget, this yields an approximate max speed.
//
// Returns pgx.ErrNoRows when the route has no usable geometry.
func (q *Queries) GetRouteGeometrySegmentMaxLength(ctx context.Context, routeID int32) (float64, error) {
	row := q.db.QueryRow(ctx, getRouteGeometrySegmentMaxLength, routeID)
	var maxLen float64
	if err := row.Scan(&maxLen); err != nil {
		return 0, err
	}
	return maxLen, nil
}

// GetRouteGeometryWKTParams identifies the route whose geometry should be
// serialized to GPX.
type GetRouteGeometryWKTParams struct {
	DongleID  string
	RouteName string
}

// GetRouteGeometryWKT returns the route's LineString rendered as WKT
// (e.g. "LINESTRING(lon lat, lon lat, ...)").
//
// The returned pgtype.Text is invalid when either:
//   - the route does not exist (pgx.ErrNoRows); or
//   - the route exists but its geometry column is NULL (Valid = false).
//
// Callers should distinguish these two cases explicitly so the GPX handler
// can return a 404 for "no track to export" without surfacing an empty file.
func (q *Queries) GetRouteGeometryWKT(ctx context.Context, arg GetRouteGeometryWKTParams) (pgtype.Text, error) {
	row := q.db.QueryRow(ctx, getRouteGeometryWKT, arg.DongleID, arg.RouteName)
	var wkt pgtype.Text
	if err := row.Scan(&wkt); err != nil {
		return pgtype.Text{}, err
	}
	return wkt, nil
}
