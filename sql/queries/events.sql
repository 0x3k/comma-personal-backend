-- name: InsertEvent :one
-- Insert a new event. Reruns of a detector over the same (route, type, offset)
-- are idempotent: the UNIQUE constraint causes the conflict to be skipped and
-- nothing is returned. Callers should treat pgx.ErrNoRows as "already exists".
INSERT INTO events (
    route_id,
    type,
    severity,
    route_offset_seconds,
    occurred_at,
    payload
)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (route_id, type, route_offset_seconds) DO NOTHING
RETURNING id, route_id, type, severity, route_offset_seconds,
          occurred_at, payload, created_at;

-- name: ListEventsByRoute :many
SELECT id, route_id, type, severity, route_offset_seconds,
       occurred_at, payload, created_at
FROM events
WHERE route_id = $1
ORDER BY route_offset_seconds ASC, id ASC;

-- name: ListEventsByDongleID :many
-- Paginated, filterable list of events for a device, joined with the owning
-- route so the UI can render the route_name alongside the event without a
-- second round trip. Pass NULL for @type_filter, @severity_filter, or
-- @route_name_filter to disable that filter. The route_name filter is
-- exact-match and is intended for the moments-page "expand a route" UI,
-- which lazy-fetches a single route's events on demand.
SELECT e.id, e.route_id, e.type, e.severity, e.route_offset_seconds,
       e.occurred_at, e.payload, e.created_at,
       r.route_name, r.dongle_id
FROM events e
JOIN routes r ON r.id = e.route_id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('type_filter')::text IS NULL OR e.type = sqlc.narg('type_filter'))
  AND (sqlc.narg('severity_filter')::text IS NULL OR e.severity = sqlc.narg('severity_filter'))
  AND (sqlc.narg('route_name_filter')::text IS NULL OR r.route_name = sqlc.narg('route_name_filter'))
ORDER BY e.occurred_at DESC NULLS LAST, e.id DESC
LIMIT sqlc.arg('limit_count') OFFSET sqlc.arg('offset_count');

-- name: CountEventsByDongleID :one
-- Total count matching the same filters as ListEventsByDongleID, so the UI
-- can paginate without repeatedly scanning the full set. Pass NULL for
-- any filter argument to disable it.
SELECT COUNT(*)::BIGINT
FROM events e
JOIN routes r ON r.id = e.route_id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('type_filter')::text IS NULL OR e.type = sqlc.narg('type_filter'))
  AND (sqlc.narg('severity_filter')::text IS NULL OR e.severity = sqlc.narg('severity_filter'))
  AND (sqlc.narg('route_name_filter')::text IS NULL OR r.route_name = sqlc.narg('route_name_filter'));

-- name: ListRoutesWithEventsByDongleID :many
-- Backs the moments-page "collapsed list of routes" view. Returns one row
-- per route that has at least one event matching the type/severity filter,
-- with the total matching count and the most recent matching event time so
-- the UI can sort routes by recency without a follow-up call. Pass NULL
-- for @type_filter or @severity_filter to disable the corresponding filter.
SELECT r.id           AS route_id,
       r.route_name,
       r.start_time,
       r.end_time,
       COUNT(e.id)::BIGINT                       AS event_count,
       MAX(e.occurred_at)::TIMESTAMPTZ            AS last_event_at
FROM routes r
JOIN events e ON e.route_id = r.id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('type_filter')::text IS NULL OR e.type = sqlc.narg('type_filter'))
  AND (sqlc.narg('severity_filter')::text IS NULL OR e.severity = sqlc.narg('severity_filter'))
GROUP BY r.id, r.route_name, r.start_time, r.end_time
ORDER BY MAX(e.occurred_at) DESC NULLS LAST, r.id DESC
LIMIT sqlc.arg('limit_count') OFFSET sqlc.arg('offset_count');

-- name: CountRoutesWithEventsByDongleID :one
-- Total route count matching the same filters as
-- ListRoutesWithEventsByDongleID. Used by the UI for pagination.
SELECT COUNT(DISTINCT r.id)::BIGINT
FROM routes r
JOIN events e ON e.route_id = r.id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('type_filter')::text IS NULL OR e.type = sqlc.narg('type_filter'))
  AND (sqlc.narg('severity_filter')::text IS NULL OR e.severity = sqlc.narg('severity_filter'));

-- name: ListEventTypeBreakdownByDongleID :many
-- Per-route, per-type event counts for the routes returned by
-- ListRoutesWithEventsByDongleID. The UI calls this once per page-of-routes
-- and joins client-side, so a single round trip yields the breakdown for
-- every visible route. Filters mirror the routes query so the breakdown
-- only counts events the user is actually filtering on.
SELECT e.route_id, e.type, COUNT(*)::BIGINT AS count
FROM events e
JOIN routes r ON r.id = e.route_id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('type_filter')::text IS NULL OR e.type = sqlc.narg('type_filter'))
  AND (sqlc.narg('severity_filter')::text IS NULL OR e.severity = sqlc.narg('severity_filter'))
  AND e.route_id = ANY (sqlc.arg('route_ids')::int[])
GROUP BY e.route_id, e.type
ORDER BY e.route_id, COUNT(*) DESC, e.type;

-- name: CountEventsByType :many
-- Aggregate count of events grouped by type for a single route.
SELECT type, count(*) AS count
FROM events
WHERE route_id = $1
GROUP BY type
ORDER BY count DESC, type ASC;
