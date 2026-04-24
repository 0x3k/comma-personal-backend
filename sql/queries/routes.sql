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
