-- name: GetRoute :one
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred
FROM routes
WHERE dongle_id = $1 AND route_name = $2;

-- name: CreateRoute :one
INSERT INTO routes (dongle_id, route_name, start_time, end_time)
VALUES ($1, $2, $3, $4)
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred;

-- name: ListRoutesByDevice :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred
FROM routes
WHERE dongle_id = $1
ORDER BY created_at DESC;

-- name: ListRoutesByDevicePaginated :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred
FROM routes
WHERE dongle_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2 OFFSET $3;

-- name: CountRoutesByDevice :one
SELECT count(*) FROM routes WHERE dongle_id = $1;

-- name: CountRoutesByDeviceFiltered :one
-- Returns the number of routes for a device matching the same filter set as
-- the dashboard routes list. Pass NULL for any filter arg to disable it.
--
-- Routes without an aggregated trip row are INCLUDED unless a duration or
-- distance filter is set; with those filters they are excluded because there
-- is no trip to compare against.
--
-- has_events:
--   NULL  -> no filter
--   TRUE  -> only routes with at least one row in events
--   FALSE -> only routes with zero rows in events
-- Uses an EXISTS subquery (not LEFT JOIN GROUP BY) so the planner can use
-- the idx_events_route_id index.
SELECT COUNT(*)::BIGINT
FROM routes r
LEFT JOIN trips t ON t.route_id = r.id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('from_time')::timestamptz IS NULL
       OR r.start_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz IS NULL
       OR r.start_time <  sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('preserved')::bool IS NULL
       OR r.preserved = sqlc.narg('preserved')::bool)
  AND (sqlc.narg('min_duration_s')::int IS NULL
       OR (t.duration_seconds IS NOT NULL
           AND t.duration_seconds >= sqlc.narg('min_duration_s')::int))
  AND (sqlc.narg('max_duration_s')::int IS NULL
       OR (t.duration_seconds IS NOT NULL
           AND t.duration_seconds <= sqlc.narg('max_duration_s')::int))
  AND (sqlc.narg('min_distance_m')::double precision IS NULL
       OR (t.distance_meters IS NOT NULL
           AND t.distance_meters >= sqlc.narg('min_distance_m')::double precision))
  AND (sqlc.narg('max_distance_m')::double precision IS NULL
       OR (t.distance_meters IS NOT NULL
           AND t.distance_meters <= sqlc.narg('max_distance_m')::double precision))
  AND (sqlc.narg('has_events')::bool IS NULL
       OR (sqlc.narg('has_events')::bool = TRUE
           AND EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id))
       OR (sqlc.narg('has_events')::bool = FALSE
           AND NOT EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id)));

-- name: GetRouteByID :one
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred
FROM routes
WHERE id = $1;

-- name: SetRoutePreserved :one
UPDATE routes
SET preserved = $3
WHERE dongle_id = $1 AND route_name = $2
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred;

-- name: SetRouteNote :one
-- Updates the free-form note on a route, keyed by the route's numeric id.
UPDATE routes
SET note = $2
WHERE id = $1
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred;

-- name: SetRouteStarred :one
-- Toggles the starred/favorite flag on a route, keyed by the route's numeric id.
UPDATE routes
SET starred = $2
WHERE id = $1
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred;

-- name: ListPreservedRoutes :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred
FROM routes
WHERE preserved = true
ORDER BY created_at DESC;

-- name: ListStaleRoutes :many
-- Returns non-preserved routes whose end_time is older than the given cutoff.
-- Used by the cleanup worker; ordered by end_time asc so the oldest routes are
-- deleted first when MaxDeletionsPerRun caps the batch.
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred
FROM routes
WHERE preserved = false
  AND end_time IS NOT NULL
  AND end_time < $1
ORDER BY end_time ASC
LIMIT $2;

-- name: DeleteRoute :exec
-- Deletes a route row. Segments and trips reference it with ON DELETE CASCADE
-- so they are removed automatically.
DELETE FROM routes
WHERE id = $1;
