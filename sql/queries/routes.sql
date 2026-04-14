-- name: GetRoute :one
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at
FROM routes
WHERE dongle_id = $1 AND route_name = $2;

-- name: CreateRoute :one
INSERT INTO routes (dongle_id, route_name, start_time, end_time)
VALUES ($1, $2, $3, $4)
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at;

-- name: ListRoutesByDevice :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at
FROM routes
WHERE dongle_id = $1
ORDER BY created_at DESC;
