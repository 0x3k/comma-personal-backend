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
-- Paginated, filterable list of events for a device. Pass NULL for
-- @type_filter or @severity_filter to disable that filter.
SELECT e.id, e.route_id, e.type, e.severity, e.route_offset_seconds,
       e.occurred_at, e.payload, e.created_at
FROM events e
JOIN routes r ON r.id = e.route_id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('type_filter')::text IS NULL OR e.type = sqlc.narg('type_filter'))
  AND (sqlc.narg('severity_filter')::text IS NULL OR e.severity = sqlc.narg('severity_filter'))
ORDER BY e.occurred_at DESC NULLS LAST, e.id DESC
LIMIT sqlc.arg('limit_count') OFFSET sqlc.arg('offset_count');

-- name: CountEventsByType :many
-- Aggregate count of events grouped by type for a single route.
SELECT type, count(*) AS count
FROM events
WHERE route_id = $1
GROUP BY type
ORDER BY count DESC, type ASC;
