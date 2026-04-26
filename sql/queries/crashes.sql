-- name: InsertCrash :one
INSERT INTO crashes (
    event_id, dongle_id, level, message, fingerprint,
    tags, exception, breadcrumbs, raw_event, occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, event_id, dongle_id, level, message, fingerprint,
          tags, exception, breadcrumbs, raw_event, occurred_at, received_at;

-- name: ListCrashes :many
-- Paginated crash list with optional dongle_id filter. Pass an empty
-- string for dongleID to skip the filter; a non-empty value restricts
-- to a single device.
SELECT id, event_id, dongle_id, level, message, fingerprint,
       tags, exception, breadcrumbs, occurred_at, received_at
FROM crashes
WHERE (sqlc.arg(dongle_id_filter)::TEXT = ''
       OR dongle_id = sqlc.arg(dongle_id_filter)::TEXT)
ORDER BY received_at DESC
LIMIT $1 OFFSET $2;

-- name: GetCrash :one
SELECT id, event_id, dongle_id, level, message, fingerprint,
       tags, exception, breadcrumbs, raw_event, occurred_at, received_at
FROM crashes
WHERE id = $1;

-- name: CountCrashes :one
SELECT COUNT(*)
FROM crashes
WHERE (sqlc.arg(dongle_id_filter)::TEXT = ''
       OR dongle_id = sqlc.arg(dongle_id_filter)::TEXT);
