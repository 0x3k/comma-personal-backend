-- name: GetSetting :one
SELECT key, value, updated_at
FROM settings
WHERE key = $1;

-- name: UpsertSetting :one
INSERT INTO settings (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = $2, updated_at = now()
RETURNING key, value, updated_at;

-- name: InsertSettingIfMissing :exec
INSERT INTO settings (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO NOTHING;
