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

// RouteListSortKey selects the ORDER BY clause used by
// ListRoutesByDeviceFiltered. Values are part of the public query contract
// because the API handler forwards them from a query parameter; the
// enumeration here is the trusted whitelist against which user input is
// validated before this struct is constructed.
type RouteListSortKey string

const (
	// RouteListSortDateDesc orders by start_time newest first. It is the
	// default and matches the pre-filter behavior of the route list.
	RouteListSortDateDesc RouteListSortKey = "date_desc"
	// RouteListSortDateAsc orders by start_time oldest first.
	RouteListSortDateAsc RouteListSortKey = "date_asc"
	// RouteListSortDurationDesc orders by the aggregated trip duration,
	// longest first. Routes without an aggregated trip row sort last.
	RouteListSortDurationDesc RouteListSortKey = "duration_desc"
	// RouteListSortDistanceDesc orders by the aggregated trip distance,
	// longest first. Routes without an aggregated trip row sort last.
	RouteListSortDistanceDesc RouteListSortKey = "distance_desc"
)

// orderByClause returns the SQL ORDER BY fragment for the given sort key,
// always appending routes.id DESC as the deterministic tiebreaker so
// pagination stays stable across pages (extends the IH-005 fix).
func (k RouteListSortKey) orderByClause() string {
	switch k {
	case RouteListSortDateAsc:
		return "ORDER BY r.start_time ASC NULLS FIRST, r.id DESC"
	case RouteListSortDurationDesc:
		return "ORDER BY t.duration_seconds DESC NULLS LAST, r.id DESC"
	case RouteListSortDistanceDesc:
		return "ORDER BY t.distance_meters DESC NULLS LAST, r.id DESC"
	case RouteListSortDateDesc:
		fallthrough
	default:
		return "ORDER BY r.start_time DESC NULLS LAST, r.id DESC"
	}
}

// ListRoutesByDeviceFilteredParams is the argument bundle for
// ListRoutesByDeviceFiltered. The filter nargs are reused verbatim by the
// generated CountRoutesByDeviceFiltered query so the filter set is applied
// identically to both the count and the page. Sort is a whitelisted
// enumeration, not raw user input.
//
// Starred narrows to routes.starred = value when set. Tags is an "all of"
// filter: each element adds an EXISTS subquery against route_tags so the
// result set is the intersection (AND), not the union (OR). Callers must
// normalize (lowercase + trim) tag values before populating this slice so
// they match the values stored by SetRouteTags; the db layer compares the
// strings verbatim.
type ListRoutesByDeviceFilteredParams struct {
	DongleID     string
	FromTime     pgtype.Timestamptz
	ToTime       pgtype.Timestamptz
	Preserved    pgtype.Bool
	MinDurationS pgtype.Int4
	MaxDurationS pgtype.Int4
	MinDistanceM pgtype.Float8
	MaxDistanceM pgtype.Float8
	HasEvents    pgtype.Bool
	Starred      pgtype.Bool
	Tags         []string
	Sort         RouteListSortKey
	Limit        int32
	Offset       int32
}

// CountRoutesByDeviceFilteredCustomParams mirrors
// ListRoutesByDeviceFilteredParams minus the pagination/sort fields so the
// custom count wrapper applies the same filter set (including tag AND) as
// the list. A separate struct keeps the contract explicit.
type CountRoutesByDeviceFilteredCustomParams struct {
	DongleID     string
	FromTime     pgtype.Timestamptz
	ToTime       pgtype.Timestamptz
	Preserved    pgtype.Bool
	MinDurationS pgtype.Int4
	MaxDurationS pgtype.Int4
	MinDistanceM pgtype.Float8
	MaxDistanceM pgtype.Float8
	HasEvents    pgtype.Bool
	Starred      pgtype.Bool
	Tags         []string
}

// listRoutesByDeviceFilteredTemplate is the SQL used for the filtered routes
// list. The first %s placeholder is replaced with zero or more tag EXISTS
// subqueries, the second with a whitelisted ORDER BY clause via
// RouteListSortKey.orderByClause; user-supplied input is never interpolated
// as text -- tag values ride as parameters, not as SQL.
//
// The query mirrors CountRoutesByDeviceFiltered in sql/queries/routes.sql so
// the filtered list and the filtered count always agree. Routes without an
// aggregated trip row are included unless min/max_duration_s or
// min/max_distance_m is set, in which case the NULL comparison excludes them.
// has_events is implemented as an EXISTS subquery to keep the planner on
// idx_events_route_id on large tables.
//
// Tag filtering is AND-semantics: for N requested tags we emit N distinct
// EXISTS subqueries. A route must have every requested tag attached to
// appear in the result. This is intentionally more expensive than a single
// ANY($tags) check -- we want "has tag a AND has tag b", not "has any of
// (a, b)".
//
// The select list also surfaces note/starred/tags so the dashboard list
// endpoint can show annotations without N+1 round-trips (mirrors
// ListRoutesByDeviceWithCounts).
const listRoutesByDeviceFilteredTemplate = `
SELECT r.id, r.dongle_id, r.route_name, r.start_time, r.end_time, r.geometry, r.created_at, r.preserved,
       r.note, r.starred,
       COALESCE(tags.tags, ARRAY[]::text[]) AS tags,
       (SELECT count(*) FROM segments s WHERE s.route_id = r.id) AS segment_count
FROM routes r
LEFT JOIN trips t ON t.route_id = r.id
LEFT JOIN LATERAL (
    SELECT ARRAY_AGG(rt.tag ORDER BY rt.tag) AS tags
    FROM route_tags rt
    WHERE rt.route_id = r.id
) tags ON TRUE
WHERE r.dongle_id = $1
  AND ($2::timestamptz IS NULL OR r.start_time >= $2::timestamptz)
  AND ($3::timestamptz IS NULL OR r.start_time <  $3::timestamptz)
  AND ($4::bool        IS NULL OR r.preserved = $4::bool)
  AND ($5::int         IS NULL OR (t.duration_seconds IS NOT NULL AND t.duration_seconds >= $5::int))
  AND ($6::int         IS NULL OR (t.duration_seconds IS NOT NULL AND t.duration_seconds <= $6::int))
  AND ($7::double precision IS NULL OR (t.distance_meters IS NOT NULL AND t.distance_meters >= $7::double precision))
  AND ($8::double precision IS NULL OR (t.distance_meters IS NOT NULL AND t.distance_meters <= $8::double precision))
  AND ($9::bool        IS NULL
       OR ($9::bool = TRUE  AND EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id))
       OR ($9::bool = FALSE AND NOT EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id)))
  AND ($10::bool       IS NULL OR r.starred = $10::bool)
%s
%s
LIMIT $%d OFFSET $%d
`

// countRoutesByDeviceFilteredTemplate mirrors the list template exactly,
// minus the sort/limit/offset. Kept in lockstep so the filtered total and
// the filtered page always reflect the same WHERE clause.
const countRoutesByDeviceFilteredTemplate = `
SELECT COUNT(*)::BIGINT
FROM routes r
LEFT JOIN trips t ON t.route_id = r.id
WHERE r.dongle_id = $1
  AND ($2::timestamptz IS NULL OR r.start_time >= $2::timestamptz)
  AND ($3::timestamptz IS NULL OR r.start_time <  $3::timestamptz)
  AND ($4::bool        IS NULL OR r.preserved = $4::bool)
  AND ($5::int         IS NULL OR (t.duration_seconds IS NOT NULL AND t.duration_seconds >= $5::int))
  AND ($6::int         IS NULL OR (t.duration_seconds IS NOT NULL AND t.duration_seconds <= $6::int))
  AND ($7::double precision IS NULL OR (t.distance_meters IS NOT NULL AND t.distance_meters >= $7::double precision))
  AND ($8::double precision IS NULL OR (t.distance_meters IS NOT NULL AND t.distance_meters <= $8::double precision))
  AND ($9::bool        IS NULL
       OR ($9::bool = TRUE  AND EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id))
       OR ($9::bool = FALSE AND NOT EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id)))
  AND ($10::bool       IS NULL OR r.starred = $10::bool)
%s
`

// buildTagExistsClauses emits an AND-joined sequence of EXISTS subqueries
// for each requested tag. Parameters start at baseIdx (one past the last
// fixed filter arg) and are returned in order for the caller to append to
// the query args slice. An empty tags slice returns an empty string and a
// nil args slice so the caller can unconditionally interpolate them.
func buildTagExistsClauses(tags []string, baseIdx int) (string, []interface{}) {
	if len(tags) == 0 {
		return "", nil
	}
	args := make([]interface{}, 0, len(tags))
	clauses := make([]string, 0, len(tags))
	for i, tag := range tags {
		// One EXISTS per tag so the set intersection is computed by the
		// planner via NestedLoop/IndexScan against (route_id, tag) -- the
		// PRIMARY KEY on route_tags covers this exactly.
		clauses = append(clauses,
			fmt.Sprintf("AND EXISTS (SELECT 1 FROM route_tags rt%d WHERE rt%d.route_id = r.id AND rt%d.tag = $%d)",
				i, i, i, baseIdx+i))
		args = append(args, tag)
	}
	return "  " + joinAnd(clauses) + "\n", args
}

// joinAnd joins predicate clauses with a newline+indent so the emitted SQL
// stays readable if it shows up in a log or an EXPLAIN ANALYZE.
func joinAnd(clauses []string) string {
	out := ""
	for i, c := range clauses {
		if i > 0 {
			out += "\n  "
		}
		out += c
	}
	return out
}

// ListRoutesByDeviceFiltered returns a paginated, filtered slice of routes
// for a device, each annotated with its segment count, note, starred flag,
// and tag list. The ORDER BY is chosen from a whitelist by Sort and always
// has routes.id DESC as the tiebreaker for deterministic pagination.
//
// Starred narrows on routes.starred = value when set. Each entry in Tags
// adds an EXISTS subquery against route_tags so the tag filter is "has all
// of these tags" (AND), not "has any of these tags" (OR). Tag values are
// compared verbatim; callers must normalize before calling.
func (q *Queries) ListRoutesByDeviceFiltered(ctx context.Context, arg ListRoutesByDeviceFilteredParams) ([]RouteWithSegmentCount, error) {
	// Fixed args occupy $1..$10 (dongle_id + 9 filters). Tag args start at
	// $11 and run one per requested tag. Limit/Offset sit immediately after.
	const fixedArgs = 10
	tagClauses, tagArgs := buildTagExistsClauses(arg.Tags, fixedArgs+1)
	limitIdx := fixedArgs + len(arg.Tags) + 1
	offsetIdx := fixedArgs + len(arg.Tags) + 2

	sql := fmt.Sprintf(listRoutesByDeviceFilteredTemplate,
		tagClauses,
		arg.Sort.orderByClause(),
		limitIdx,
		offsetIdx,
	)

	args := make([]interface{}, 0, fixedArgs+len(tagArgs)+2)
	args = append(args,
		arg.DongleID,
		arg.FromTime,
		arg.ToTime,
		arg.Preserved,
		arg.MinDurationS,
		arg.MaxDurationS,
		arg.MinDistanceM,
		arg.MaxDistanceM,
		arg.HasEvents,
		arg.Starred,
	)
	args = append(args, tagArgs...)
	args = append(args, arg.Limit, arg.Offset)

	rows, err := q.db.Query(ctx, sql, args...)
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

// CountRoutesByDeviceFilteredCustom is the count companion to
// ListRoutesByDeviceFiltered. When tag filtering is in play we cannot use
// the sqlc-generated CountRoutesByDeviceFiltered because the tag predicate
// is dynamic; this wrapper appends the same tag EXISTS clauses to a mirror
// of the count SQL so the reported total always matches the filtered list.
//
// Callers that do not need tag AND-semantics can keep using the generated
// CountRoutesByDeviceFiltered; callers that pass tags must use this.
func (q *Queries) CountRoutesByDeviceFilteredCustom(ctx context.Context, arg CountRoutesByDeviceFilteredCustomParams) (int64, error) {
	const fixedArgs = 10
	tagClauses, tagArgs := buildTagExistsClauses(arg.Tags, fixedArgs+1)

	sql := fmt.Sprintf(countRoutesByDeviceFilteredTemplate, tagClauses)

	args := make([]interface{}, 0, fixedArgs+len(tagArgs))
	args = append(args,
		arg.DongleID,
		arg.FromTime,
		arg.ToTime,
		arg.Preserved,
		arg.MinDurationS,
		arg.MaxDurationS,
		arg.MinDistanceM,
		arg.MaxDistanceM,
		arg.HasEvents,
		arg.Starred,
	)
	args = append(args, tagArgs...)

	row := q.db.QueryRow(ctx, sql, args...)
	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

const listRecentRoutes = `
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved
FROM routes
ORDER BY created_at DESC, id DESC
LIMIT $1
`

// ListRecentRoutes returns the most-recently created routes across every
// device, up to limit rows. Used by the thumbnail worker to scan candidate
// routes without a hand-written filesystem walk.
func (q *Queries) ListRecentRoutes(ctx context.Context, limit int32) ([]Route, error) {
	rows, err := q.db.Query(ctx, listRecentRoutes, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Route
	for rows.Next() {
		var i Route
		if err := rows.Scan(
			&i.ID,
			&i.DongleID,
			&i.RouteName,
			&i.StartTime,
			&i.EndTime,
			&i.Geometry,
			&i.CreatedAt,
			&i.Preserved,
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
