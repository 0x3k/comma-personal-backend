-- name: GetDevice :one
SELECT dongle_id, serial, public_key, created_at, updated_at
FROM devices
WHERE dongle_id = $1;

-- name: GetDeviceByPublicKey :one
SELECT dongle_id, serial, public_key, created_at, updated_at
FROM devices
WHERE public_key = $1
LIMIT 1;

-- name: CreateDevice :one
INSERT INTO devices (dongle_id, serial, public_key)
VALUES ($1, $2, $3)
RETURNING dongle_id, serial, public_key, created_at, updated_at;

-- name: ListDevices :many
SELECT dongle_id, serial, public_key, created_at, updated_at
FROM devices
ORDER BY created_at DESC;

-- name: UpsertDevice :one
INSERT INTO devices (dongle_id, serial, public_key)
VALUES ($1, $2, $3)
ON CONFLICT (dongle_id) DO UPDATE SET serial = $2, public_key = $3, updated_at = now()
RETURNING dongle_id, serial, public_key, created_at, updated_at;
