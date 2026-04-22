package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// RouteWithSegmentCount extends Route with an inline segment count,
// avoiding the need for a separate count query per route.
type RouteWithSegmentCount struct {
	ID           int32
	DongleID     string
	RouteName    string
	StartTime    pgtype.Timestamptz
	EndTime      pgtype.Timestamptz
	Geometry     interface{}
	CreatedAt    pgtype.Timestamptz
	SegmentCount int64
}

const listRoutesByDeviceWithCounts = `
SELECT r.id, r.dongle_id, r.route_name, r.start_time, r.end_time, r.geometry, r.created_at,
       (SELECT count(*) FROM segments s WHERE s.route_id = r.id) AS segment_count
FROM routes r
WHERE r.dongle_id = $1
ORDER BY r.created_at DESC, r.id DESC
LIMIT $2 OFFSET $3
`

// ListRoutesByDeviceWithCounts returns paginated routes for a device, each
// annotated with its segment count in a single query (no N+1).
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
