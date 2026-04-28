-- Allow signature-keyed plate_watchlist rows for the signature-fusion
-- heuristic.
--
-- The original schema (015_alpr_schema) declared plate_watchlist.plate_hash
-- as UNIQUE NOT NULL. The signature-fusion heuristic introduces a NEW
-- alert kind: a row keyed on signature_id with plate_hash IS NULL, used
-- when the heuristic decides "this physical vehicle (by signature) has
-- been seen with multiple plates -- alert on the vehicle, not on any one
-- plate." That row needs:
--
--   1. plate_hash to be nullable (so the signature-keyed row exists
--      without a plate).
--   2. The plate_hash UNIQUE constraint to apply only when plate_hash
--      IS NOT NULL (multiple NULL plate_hash rows are allowed by SQL,
--      but the existing UNIQUE constraint forces a B-tree index that
--      already permits multiple NULLs in PostgreSQL -- we still want
--      to keep "at most one row per real plate_hash" enforced, so the
--      replacement is a partial unique index).
--   3. A NEW partial unique index on signature_id where plate_hash IS
--      NULL, so the heuristic can never write two signature-keyed
--      alerts for the same signature.
--   4. The octet_length CHECK to permit NULL (CHECKs evaluate NULL as
--      "unknown" and accept the row, but we restate explicitly so
--      future readers do not assume the original NOT NULL invariant).
--
-- All operations are idempotent / guarded so this migration is safe
-- against partially-applied schemas.

-- Drop the inline NOT NULL on plate_hash. ALTER COLUMN ... DROP NOT NULL
-- is itself idempotent (running on an already-nullable column is a
-- no-op) so no DO block is needed.
ALTER TABLE plate_watchlist
    ALTER COLUMN plate_hash DROP NOT NULL;

-- Drop the implicit UNIQUE constraint that the inline declaration
-- created. Postgres named it after the table+column; on a
-- freshly-applied 015 the conventional name is
-- plate_watchlist_plate_hash_key. We resolve it dynamically so this
-- migration also handles databases where the constraint was renamed
-- by hand (e.g. by a prior migration's CONSTRAINT clause).
DO $$
DECLARE
    cname TEXT;
BEGIN
    SELECT conname INTO cname
    FROM pg_constraint
    WHERE conrelid = 'plate_watchlist'::regclass
      AND contype  = 'u'
      AND pg_get_constraintdef(oid) ILIKE 'UNIQUE (plate_hash)';
    IF cname IS NOT NULL THEN
        EXECUTE format('ALTER TABLE plate_watchlist DROP CONSTRAINT %I', cname);
    END IF;
END
$$;

-- Replacement: partial unique index that enforces "at most one
-- watchlist row per real plate_hash" while ignoring the
-- signature-keyed (plate_hash IS NULL) rows.
CREATE UNIQUE INDEX IF NOT EXISTS uq_plate_watchlist_plate_hash
    ON plate_watchlist(plate_hash)
    WHERE plate_hash IS NOT NULL;

-- New partial unique index on signature_id for the signature-keyed
-- alert path. "At most one signature-keyed row per signature" -- a
-- second plate-swap detection on the same signature must update the
-- existing row (refresh last_alert_at, append to evidence) rather
-- than insert a duplicate.
CREATE UNIQUE INDEX IF NOT EXISTS uq_plate_watchlist_signature_swap
    ON plate_watchlist(signature_id)
    WHERE plate_hash IS NULL AND signature_id IS NOT NULL;

-- Backfill / sanity index used by the fusion heuristic to find the
-- existing signature-swap alert (if any) for a given signature when
-- re-evaluating. The partial unique index above can serve point
-- lookups, but a non-unique partial index is friendlier for the
-- planner across pgx parameter binding boundaries.
CREATE INDEX IF NOT EXISTS idx_plate_watchlist_signature_swap
    ON plate_watchlist(signature_id)
    WHERE plate_hash IS NULL;
