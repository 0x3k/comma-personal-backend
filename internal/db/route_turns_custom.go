package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// listRoutesForTurnDetection picks routes that are eligible for the
// turn-detector worker on its steady-state poll. The criteria mirror the
// trip-aggregator's "finalized" check (newest segment is older than the
// cutoff) plus two extra requirements specific to turn detection:
//
//  1. The route must already have its start_time and geometry populated
//     by the route-metadata worker -- the turn detector reads them.
//  2. The route either has no turns yet OR its newest segment arrived
//     after we last computed turns. The "after we last computed" check
//     is approximate (we don't store a per-route computed_at for turns)
//     and uses the existence of a row in route_turns whose dongle/route
//     matches as the proxy for "already done". A future segment that
//     bumps the route's geometry will leave the turn rows in place and
//     this query will not pick it back up; that is acceptable because
//     re-running over a segment-extended route is what the
//     ALPR historical backfill is for.
//
// Hand-written rather than sqlc-generated because sqlc 1.x does not
// resolve the LATERAL alias used to compute latest_segment_at -- same
// reason ListRoutesForTripAggregation lives in this file.
const listRoutesForTurnDetection = `
SELECT r.id, r.dongle_id, r.route_name, r.start_time, r.created_at,
       seg.latest_segment_at, seg.segment_count
FROM routes r
LEFT JOIN LATERAL (
    SELECT MAX(s.created_at) AS latest_segment_at,
           COUNT(*)          AS segment_count
    FROM segments s
    WHERE s.route_id = r.id
) seg ON TRUE
WHERE seg.segment_count > 0
  AND seg.latest_segment_at IS NOT NULL
  AND seg.latest_segment_at < $1
  AND r.start_time IS NOT NULL
  AND r.geometry   IS NOT NULL
  AND r.geometry_times IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM route_turns rt
      WHERE rt.dongle_id = r.dongle_id
        AND rt.route     = r.route_name
  )
ORDER BY r.created_at ASC, r.id ASC
LIMIT $2
`

// RouteForTurnDetection is the subset of a route row needed by the
// turn-detector worker. The shape mirrors RouteForTripAggregation
// deliberately so the workers look alike; the detector only needs
// dongle_id+route_name (the primary key for its turn table) plus the
// finalized-check fields.
type RouteForTurnDetection struct {
	ID              int32
	DongleID        string
	RouteName       string
	StartTime       pgtype.Timestamptz
	CreatedAt       pgtype.Timestamptz
	LatestSegmentAt pgtype.Timestamptz
	SegmentCount    int64
}

// ListRoutesForTurnDetectionParams selects routes whose newest segment
// is older than FinalizedBefore and that have route_metadata populated
// but no turns yet. Limit caps batch size.
type ListRoutesForTurnDetectionParams struct {
	FinalizedBefore pgtype.Timestamptz
	Limit           int32
}

// ListRoutesForTurnDetection returns routes that are ready for turn
// detection. See the const docs for selection criteria.
func (q *Queries) ListRoutesForTurnDetection(ctx context.Context, arg ListRoutesForTurnDetectionParams) ([]RouteForTurnDetection, error) {
	rows, err := q.db.Query(ctx, listRoutesForTurnDetection, arg.FinalizedBefore, arg.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []RouteForTurnDetection
	for rows.Next() {
		var i RouteForTurnDetection
		if err := rows.Scan(
			&i.ID,
			&i.DongleID,
			&i.RouteName,
			&i.StartTime,
			&i.CreatedAt,
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

// listRecentRoutesForTurnBackfill returns the most-recent N routes that
// already have geometry+start_time populated and do NOT have any turn
// rows yet. Used by the one-shot first-deploy backfill so freshly
// installed servers don't have to wait for new uploads to populate
// turn data for routes already in the database.
//
// Order is created_at DESC -- newest first -- so a backfill that runs
// out of time still covers the most operationally interesting routes.
const listRecentRoutesForTurnBackfill = `
SELECT r.dongle_id, r.route_name
FROM routes r
WHERE r.start_time     IS NOT NULL
  AND r.geometry       IS NOT NULL
  AND r.geometry_times IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM route_turns rt
      WHERE rt.dongle_id = r.dongle_id
        AND rt.route     = r.route_name
  )
ORDER BY r.created_at DESC, r.id DESC
LIMIT $1
`

// RouteForTurnBackfill is the minimal pair the backfill loop needs: it
// looks up the route by (dongle_id, route_name), so neither the routes
// PK nor segment timestamps are projected.
type RouteForTurnBackfill struct {
	DongleID  string
	RouteName string
}

// ListRecentRoutesForTurnBackfill returns up to limit routes whose
// turns have not been computed yet, newest first. Used once per process
// start by the turn-detector backfill.
func (q *Queries) ListRecentRoutesForTurnBackfill(ctx context.Context, limit int32) ([]RouteForTurnBackfill, error) {
	rows, err := q.db.Query(ctx, listRecentRoutesForTurnBackfill, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []RouteForTurnBackfill
	for rows.Next() {
		var i RouteForTurnBackfill
		if err := rows.Scan(&i.DongleID, &i.RouteName); err != nil {
			return nil, err
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}
