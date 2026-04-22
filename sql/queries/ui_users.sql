-- name: GetUIUserByUsername :one
SELECT id, username, password_hash, created_at
FROM ui_users
WHERE username = $1;

-- name: GetUIUserByID :one
SELECT id, username, password_hash, created_at
FROM ui_users
WHERE id = $1;

-- name: UpsertUIUser :one
INSERT INTO ui_users (username, password_hash)
VALUES ($1, $2)
ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash
RETURNING id, username, password_hash, created_at;
