-- name: UpsertWatchlistAlerted :one
-- Insert a new alerted-kind row for a plate, or refresh an existing one
-- so its severity/last_alert_at advance and acked_at clears. The
-- transition from whitelist or note into alerted is intentionally
-- permitted: the heuristic decided the plate is alert-worthy regardless
-- of the previous state. Operators reverse it via UpsertWatchlistWhitelist
-- or RemoveWatchlist.
INSERT INTO plate_watchlist (
    plate_hash,
    label_ciphertext,
    kind,
    severity,
    first_alert_at,
    last_alert_at,
    notes
)
VALUES ($1, $2, 'alerted', $3, sqlc.arg('alert_at'), sqlc.arg('alert_at'), $4)
ON CONFLICT (plate_hash) DO UPDATE
SET kind             = 'alerted',
    severity         = EXCLUDED.severity,
    label_ciphertext = COALESCE(EXCLUDED.label_ciphertext,
                                plate_watchlist.label_ciphertext),
    -- first_alert_at sticks: an existing alerted plate keeps its initial
    -- alert timestamp even on subsequent re-fires.
    first_alert_at   = COALESCE(plate_watchlist.first_alert_at,
                                EXCLUDED.first_alert_at),
    last_alert_at    = EXCLUDED.last_alert_at,
    -- New alert event clears any prior ack so the operator sees it again.
    acked_at         = NULL,
    notes            = COALESCE(EXCLUDED.notes, plate_watchlist.notes),
    updated_at       = now()
RETURNING id, plate_hash, label_ciphertext, kind, severity,
          first_alert_at, last_alert_at, acked_at, notes,
          created_at, updated_at;

-- name: UpsertWatchlistWhitelist :one
-- Insert a new whitelist row for a plate, or update an existing row to
-- the whitelist kind. Whitelisting an alerted plate is the operator's
-- "this is fine, stop alerting on it" action: the alert timestamps and
-- severity are cleared so the alert feed no longer surfaces the row.
INSERT INTO plate_watchlist (
    plate_hash,
    label_ciphertext,
    kind,
    notes
)
VALUES ($1, $2, 'whitelist', $3)
ON CONFLICT (plate_hash) DO UPDATE
SET kind             = 'whitelist',
    label_ciphertext = COALESCE(EXCLUDED.label_ciphertext,
                                plate_watchlist.label_ciphertext),
    -- Clearing severity and the alert timestamps prevents stale alert
    -- entries from lingering once the operator has whitelisted.
    severity         = NULL,
    first_alert_at   = NULL,
    last_alert_at    = NULL,
    acked_at         = NULL,
    notes            = COALESCE(EXCLUDED.notes, plate_watchlist.notes),
    updated_at       = now()
RETURNING id, plate_hash, label_ciphertext, kind, severity,
          first_alert_at, last_alert_at, acked_at, notes,
          created_at, updated_at;

-- name: RemoveWatchlist :execrows
-- Delete the watchlist row for a plate. Returns rows-affected so the
-- caller can distinguish "deleted" from "wasn't there to begin with".
DELETE FROM plate_watchlist
WHERE plate_hash = $1;

-- name: GetWatchlistByHash :one
-- Look up a plate's current watchlist row, or pgx.ErrNoRows when none
-- exists. Used by the alert-evaluation worker to decide whether the
-- plate is whitelisted (suppress) before computing severity.
SELECT id, plate_hash, label_ciphertext, kind, severity,
       first_alert_at, last_alert_at, acked_at, notes,
       created_at, updated_at
FROM plate_watchlist
WHERE plate_hash = $1;

-- name: ListWatchlistByKind :many
-- Paginated list of watchlist rows of a given kind, newest update first.
-- Used by the watchlist UI tabs (alerted / whitelist / note).
SELECT id, plate_hash, label_ciphertext, kind, severity,
       first_alert_at, last_alert_at, acked_at, notes,
       created_at, updated_at
FROM plate_watchlist
WHERE kind = $1
ORDER BY updated_at DESC, id DESC
LIMIT $2 OFFSET $3;

-- name: AckWatchlist :execrows
-- Mark a plate's watchlist row as acknowledged at the given time. The
-- alert feed treats acked rows as "seen", de-emphasizing them in the UI
-- without removing them. Returns rows-affected so the caller can detect
-- "no such plate".
UPDATE plate_watchlist
SET acked_at   = sqlc.arg('acked_at'),
    updated_at = now()
WHERE plate_hash = $1;

-- name: UnackWatchlist :execrows
-- Reverse of AckWatchlist: clear acked_at so the alert resurfaces.
UPDATE plate_watchlist
SET acked_at   = NULL,
    updated_at = now()
WHERE plate_hash = $1;

-- name: ListAlerts :many
-- The alert feed: alerted-kind rows ordered by recency of last_alert_at,
-- with optional filtering on whether the row is currently acknowledged.
-- Pass NULL for @acked_filter to include both states. Pagination via
-- limit/offset to match the events feed style elsewhere in the project.
SELECT id, plate_hash, label_ciphertext, kind, severity,
       first_alert_at, last_alert_at, acked_at, notes,
       created_at, updated_at
FROM plate_watchlist
WHERE kind = 'alerted'
  AND (sqlc.narg('acked_filter')::boolean IS NULL
       OR (sqlc.narg('acked_filter')::boolean = TRUE  AND acked_at IS NOT NULL)
       OR (sqlc.narg('acked_filter')::boolean = FALSE AND acked_at IS NULL))
ORDER BY last_alert_at DESC NULLS LAST, id DESC
LIMIT $1 OFFSET $2;

-- name: CountUnackedAlerts :one
-- Used by the dashboard alert badge ("3 unacked alerts").
SELECT COUNT(*)::BIGINT
FROM plate_watchlist
WHERE kind = 'alerted'
  AND acked_at IS NULL;

-- name: MaxOpenSeverity :one
-- Highest severity among unacknowledged alerts, or 0 if none. Drives the
-- color of the dashboard alert badge. COALESCE so the query always
-- returns a row instead of NULL when there are no open alerts.
SELECT COALESCE(MAX(severity), 0)::SMALLINT
FROM plate_watchlist
WHERE kind = 'alerted'
  AND acked_at IS NULL;
