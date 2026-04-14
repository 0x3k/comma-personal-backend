-- name: GetDevice :one
SELECT dongle_id, serial, public_key, created_at, updated_at
FROM devices
WHERE dongle_id = $1;

-- name: CreateDevice :one
INSERT INTO devices (dongle_id, serial, public_key)
VALUES ($1, $2, $3)
RETURNING dongle_id, serial, public_key, created_at, updated_at;

-- name: ListDevices :many
SELECT dongle_id, serial, public_key, created_at, updated_at
FROM devices
ORDER BY created_at DESC;
