-- name: GetDeviceParam :one
SELECT id, dongle_id, key, value, updated_at
FROM device_params
WHERE dongle_id = $1 AND key = $2;

-- name: SetDeviceParam :one
INSERT INTO device_params (dongle_id, key, value)
VALUES ($1, $2, $3)
ON CONFLICT (dongle_id, key) DO UPDATE SET value = $3, updated_at = now()
RETURNING id, dongle_id, key, value, updated_at;

-- name: ListDeviceParams :many
SELECT id, dongle_id, key, value, updated_at
FROM device_params
WHERE dongle_id = $1
ORDER BY key ASC;

-- name: DeleteDeviceParam :exec
DELETE FROM device_params
WHERE dongle_id = $1 AND key = $2;
