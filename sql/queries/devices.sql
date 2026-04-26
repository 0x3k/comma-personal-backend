-- name: GetDevice :one
SELECT dongle_id, serial, public_key, created_at, updated_at,
       sunnylink_dongle_id, sunnylink_public_key
FROM devices
WHERE dongle_id = $1;

-- name: GetDeviceByPublicKey :one
SELECT dongle_id, serial, public_key, created_at, updated_at,
       sunnylink_dongle_id, sunnylink_public_key
FROM devices
WHERE public_key = $1
LIMIT 1;

-- name: GetDeviceBySunnylinkDongleID :one
SELECT dongle_id, serial, public_key, created_at, updated_at,
       sunnylink_dongle_id, sunnylink_public_key
FROM devices
WHERE sunnylink_dongle_id = $1
LIMIT 1;

-- name: CreateDevice :one
INSERT INTO devices (dongle_id, serial, public_key)
VALUES ($1, $2, $3)
RETURNING dongle_id, serial, public_key, created_at, updated_at,
          sunnylink_dongle_id, sunnylink_public_key;

-- name: ListDevices :many
SELECT dongle_id, serial, public_key, created_at, updated_at,
       sunnylink_dongle_id, sunnylink_public_key
FROM devices
ORDER BY created_at DESC;

-- name: UpsertDevice :one
INSERT INTO devices (dongle_id, serial, public_key)
VALUES ($1, $2, $3)
ON CONFLICT (dongle_id) DO UPDATE SET serial = $2, public_key = $3, updated_at = now()
RETURNING dongle_id, serial, public_key, created_at, updated_at,
          sunnylink_dongle_id, sunnylink_public_key;

-- name: SetSunnylinkRegistration :one
-- Links a sunnylink identity onto an existing devices row keyed by the comma
-- dongle_id. Used by the pilotauth handler when a request carries the extra
-- comma_dongle_id form field (sunnylink registration variant).
UPDATE devices
SET sunnylink_dongle_id  = $2,
    sunnylink_public_key = $3,
    updated_at           = now()
WHERE dongle_id = $1
RETURNING dongle_id, serial, public_key, created_at, updated_at,
          sunnylink_dongle_id, sunnylink_public_key;
