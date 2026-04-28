-- Reverse of 018_alpr_watchlist_signature_swap.up.sql.
--
-- Drop the new indexes first, then re-impose the original NOT NULL +
-- UNIQUE on plate_hash. Re-imposing NOT NULL fails if any
-- signature-keyed (plate_hash IS NULL) rows exist; that is by design --
-- a manual operator decision is required to delete those rows before
-- rolling back this migration.

DROP INDEX IF EXISTS idx_plate_watchlist_signature_swap;
DROP INDEX IF EXISTS uq_plate_watchlist_signature_swap;
DROP INDEX IF EXISTS uq_plate_watchlist_plate_hash;

-- Restore NOT NULL on plate_hash. Will fail loudly if any
-- signature-keyed rows remain; the operator must clean them up first.
ALTER TABLE plate_watchlist
    ALTER COLUMN plate_hash SET NOT NULL;

-- Restore the original UNIQUE constraint on plate_hash. Wrapped in a
-- DO block so the down migration is safe to re-run against a
-- partially-rolled-back schema.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conrelid = 'plate_watchlist'::regclass
          AND contype  = 'u'
          AND pg_get_constraintdef(oid) ILIKE 'UNIQUE (plate_hash)'
    ) THEN
        ALTER TABLE plate_watchlist
            ADD CONSTRAINT plate_watchlist_plate_hash_key UNIQUE (plate_hash);
    END IF;
END
$$;
