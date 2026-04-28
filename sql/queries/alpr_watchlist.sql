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
ON CONFLICT (plate_hash) WHERE plate_hash IS NOT NULL DO UPDATE
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

-- name: UpsertWatchlistAlertedPreserveAck :one
-- Variant of UpsertWatchlistAlerted used by the stalking heuristic on
-- re-evaluation. The behavioural difference is that acked_at is
-- preserved rather than cleared:
--
--   - On INSERT (no prior row): acked_at is left NULL by the column
--     default. There is nothing to preserve.
--   - On UPDATE: acked_at is set to plate_watchlist.acked_at (i.e.
--     itself), which is a no-op write that preserves whatever the
--     operator's last ack state was. This matters because re-running
--     the heuristic on the same plate after the user has acked the
--     alert should NOT silently re-arm the alert. The worker uses the
--     other variant (UpsertWatchlistAlerted) only when severity
--     strictly increased, in which case the user genuinely needs to
--     re-see the upgraded alert.
--
-- Severity uses GREATEST(existing, computed) so a re-evaluation with
-- a lower computed severity never demotes the row. The heuristic
-- never wants to "unalert" a plate; whitelisting is a separate
-- explicit operator action.
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
ON CONFLICT (plate_hash) WHERE plate_hash IS NOT NULL DO UPDATE
SET kind             = 'alerted',
    severity         = GREATEST(COALESCE(plate_watchlist.severity, 0::SMALLINT),
                                 EXCLUDED.severity),
    label_ciphertext = COALESCE(EXCLUDED.label_ciphertext,
                                plate_watchlist.label_ciphertext),
    first_alert_at   = COALESCE(plate_watchlist.first_alert_at,
                                EXCLUDED.first_alert_at),
    last_alert_at    = EXCLUDED.last_alert_at,
    -- acked_at intentionally preserved -- callers use the
    -- ack-clearing variant only on a strict severity upgrade.
    acked_at         = plate_watchlist.acked_at,
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
ON CONFLICT (plate_hash) WHERE plate_hash IS NOT NULL DO UPDATE
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

-- name: RenameWatchlistHash :execrows
-- Plate-merge path: rewrite plate_hash on a single watchlist row from
-- the source hash to the destination hash. Used by the merge handler
-- when ONLY the source watchlist row exists (no destination) -- the
-- handler can simply rename the existing row instead of doing a
-- full merge. Returns rows-affected so the handler can fall through
-- to the merge path on 0.
UPDATE plate_watchlist
SET plate_hash = sqlc.arg('new_plate_hash'),
    updated_at = now()
WHERE plate_hash = sqlc.arg('old_plate_hash');

-- name: ApplyMergedWatchlistRow :execrows
-- Plate-merge path: when BOTH source and destination watchlist rows
-- exist for the same plate identity, the merge handler computes the
-- merged column values in Go (max severity, earliest first_alert_at,
-- latest last_alert_at, concatenated notes, preserved-or-cleared
-- acked_at) and stamps them onto the destination row. The matching
-- DELETE for the source row is a separate RemoveWatchlist call inside
-- the same transaction so a partial commit is impossible.
--
-- Computing the merge in Go rather than SQL keeps the merge rules
-- testable without a Postgres dependency and avoids ambiguous
-- column-reference traps in CTE-shaped UPDATE+DELETE composites.
UPDATE plate_watchlist
SET severity       = sqlc.narg('severity'),
    first_alert_at = sqlc.narg('first_alert_at'),
    last_alert_at  = sqlc.narg('last_alert_at'),
    acked_at       = sqlc.narg('acked_at'),
    notes          = sqlc.narg('notes'),
    updated_at     = now()
WHERE plate_hash = sqlc.arg('plate_hash');

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

-- name: UpdateWatchlistNotes :execrows
-- Sets the notes column on a watchlist row. Used by the alert-note
-- handler so the operator can jot context ("this is the white truck
-- from Tuesday") next to a plate. NULL clears the existing note.
UPDATE plate_watchlist
SET notes      = $2,
    updated_at = now()
WHERE plate_hash = $1;

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

-- name: ListFlaggedPlateHashes :many
-- Retention "flagged set" feed: alerted plate_hashes that should be
-- preserved under the longer flagged retention window. The semantics
-- intentionally match what the user agreed to in feature notes:
--   - kind = 'alerted'                : whitelisted/note rows are NOT
--                                       in the flagged set; whitelisting
--                                       a plate drops it to the
--                                       unflagged tier.
--   - acked_at IS NULL                : an unacked alert is preserved
--                                       regardless of severity.
--   - acked_at IS NOT NULL AND
--     severity >= 4                   : a high-severity alert stays
--                                       preserved even after ack so
--                                       evidence is retained for review.
--   - acked low-severity (1..3)       : NOT in the set; demotes to the
--                                       unflagged retention tier.
-- The worker uses the returned hashes as the exclusion list for the
-- unflagged-retention DELETE pass.
SELECT plate_hash
FROM plate_watchlist
WHERE kind = 'alerted'
  AND (acked_at IS NULL OR severity >= 4);

-- name: GetWatchlistSignatureSwap :one
-- Look up the signature-keyed plate-swap alert (if any) for a given
-- signature_id. Returns pgx.ErrNoRows when no signature-swap row exists.
-- Used by the signature-fusion heuristic on re-evaluation: when a new
-- encounter lands for any plate that shares this signature, the worker
-- needs to know whether to refresh the existing alert (UPSERT path) or
-- raise a brand-new one. The query is keyed on signature_id with
-- plate_hash IS NULL because that is exactly what distinguishes a
-- signature-swap row from a plate row that happens to have a signature
-- linked.
SELECT id, plate_hash, label_ciphertext, kind, severity,
       first_alert_at, last_alert_at, acked_at, notes,
       created_at, updated_at, signature_id
FROM plate_watchlist
WHERE signature_id = sqlc.arg('signature_id')
  AND plate_hash IS NULL;

-- name: UpsertWatchlistSignatureSwap :one
-- Insert-or-update a signature-keyed plate-swap alert. The row is
-- distinguished from plate-keyed rows by plate_hash IS NULL and a
-- non-NULL signature_id; the partial unique index
-- uq_plate_watchlist_signature_swap (see migration 018) enforces "at
-- most one row per signature" so this UPSERT is well-defined.
--
-- Note that PostgreSQL's ON CONFLICT clause requires referencing the
-- conflict target by either constraint name or by a matching index
-- expression. We use the explicit (signature_id) WHERE clause form so
-- the planner can match the partial unique index regardless of how
-- pgx binds the parameters. The same predicate as the index is
-- repeated verbatim so we get the predicate-aware match.
INSERT INTO plate_watchlist (
    plate_hash,
    signature_id,
    kind,
    severity,
    first_alert_at,
    last_alert_at,
    notes
)
VALUES (
    NULL,
    sqlc.arg('signature_id'),
    'alerted',
    sqlc.arg('severity'),
    sqlc.arg('alert_at'),
    sqlc.arg('alert_at'),
    sqlc.narg('notes')
)
ON CONFLICT (signature_id) WHERE plate_hash IS NULL AND signature_id IS NOT NULL
DO UPDATE
SET kind             = 'alerted',
    severity         = GREATEST(COALESCE(plate_watchlist.severity, 0::SMALLINT),
                                EXCLUDED.severity),
    -- first_alert_at sticks: an existing signature-swap alert keeps
    -- its initial fire time even on subsequent re-fires.
    first_alert_at   = COALESCE(plate_watchlist.first_alert_at,
                                EXCLUDED.first_alert_at),
    last_alert_at    = EXCLUDED.last_alert_at,
    -- acked_at preserved: re-fires of the same signature alert do
    -- NOT clear an operator ack. A strict severity upgrade is the
    -- only path that should re-raise the alert UI; the worker layer
    -- enforces that by deciding whether to call this UPSERT vs. a
    -- separate ack-clearing variant when severity strictly increases.
    -- For the first version of the heuristic we keep the simpler
    -- always-preserve-ack semantics here.
    acked_at         = plate_watchlist.acked_at,
    notes            = COALESCE(EXCLUDED.notes, plate_watchlist.notes),
    updated_at       = now()
RETURNING id, plate_hash, label_ciphertext, kind, severity,
          first_alert_at, last_alert_at, acked_at, notes,
          created_at, updated_at, signature_id;

-- name: ListPlateHashesForSignatureInWindow :many
-- For a given signature_id, return one row per (plate_hash,
-- area_cell_lat, area_cell_lng) pair counted across encounters whose
-- last_seen_ts falls inside the provided window. The geographic cell is
-- computed by floor()-bucketing the encounter's first-detection GPS at
-- the caller-supplied cell size in degrees. The fusion heuristic then
-- groups the rows by area cell and counts distinct plate_hashes; if any
-- cell has >=3 distinct hashes the signature-swap alert fires.
--
-- We compute the cell lat/lng inside SQL (rather than in Go) so the
-- bucketing happens once per row in the database, keeping the result
-- set small (a few thousand rows even on a busy install). Encounters
-- without first-detection GPS (LEFT JOIN miss) are dropped: the
-- signature-swap alert is geographically scoped, so an encounter
-- without coordinates cannot contribute.
--
-- The lat/lng cell sizes differ because longitude degrees shrink with
-- cosine of latitude. Callers compute both from a single km-scale cell
-- size (the fusion heuristic uses 5 km, matching the stalking
-- heuristic's cross_route_geo_spread bucketing) by passing
-- cell_lat_deg = km / 111.32 and cell_lng_deg = cell_lat_deg /
-- max(cos(lat_radians_at_query_time), 1e-6). An exact match with the
-- Go geoCellKey helper is not required because the SQL-side bucketing
-- is purely an internal grouping for the COUNT query, never compared
-- across the SQL/Go boundary.
SELECT pe.plate_hash,
       FLOOR(pd.gps_lat / sqlc.arg('cell_lat_deg'))::BIGINT  AS cell_lat,
       FLOOR(pd.gps_lng / sqlc.arg('cell_lng_deg'))::BIGINT  AS cell_lng,
       pe.id            AS encounter_id,
       pe.last_seen_ts  AS last_seen_ts,
       pd.gps_lat       AS gps_lat,
       pd.gps_lng       AS gps_lng
FROM plate_encounters pe
JOIN plate_detections pd ON pd.dongle_id  = pe.dongle_id
                        AND pd.route      = pe.route
                        AND pd.plate_hash = pe.plate_hash
                        AND pd.frame_ts   = pe.first_seen_ts
WHERE pe.signature_id = sqlc.arg('signature_id')
  AND pe.last_seen_ts >= sqlc.arg('window_start')
  AND pe.first_seen_ts <= sqlc.arg('window_end')
  AND pd.gps_lat IS NOT NULL
  AND pd.gps_lng IS NOT NULL
ORDER BY pe.last_seen_ts DESC, pe.id DESC;
