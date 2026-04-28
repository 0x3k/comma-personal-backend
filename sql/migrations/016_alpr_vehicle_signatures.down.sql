-- Reverse of 016_alpr_vehicle_signatures.up.sql.
--
-- Order matters: every column that points at vehicle_signatures via a
-- foreign key (or that this migration added) must go before the table
-- itself, otherwise the DROP TABLE fails on dependent constraints. Each
-- statement uses IF EXISTS / DROP TABLE ... CASCADE so the down
-- migration is safe to re-run against a partially-applied schema.

-- plate_watchlist.signature_id was added wholesale by this migration,
-- so dropping the column also removes its FK constraint.
ALTER TABLE plate_watchlist
    DROP COLUMN IF EXISTS signature_id;

-- plate_encounters.signature_id is left in place (it was declared by
-- 015_alpr_schema). Drop only the FK constraint this migration added.
ALTER TABLE plate_encounters
    DROP CONSTRAINT IF EXISTS plate_encounters_signature_id_fkey;

-- plate_detections columns added by this migration.
ALTER TABLE plate_detections
    DROP COLUMN IF EXISTS det_attr_confidence,
    DROP COLUMN IF EXISTS det_body_type,
    DROP COLUMN IF EXISTS det_color,
    DROP COLUMN IF EXISTS det_model,
    DROP COLUMN IF EXISTS det_make,
    DROP COLUMN IF EXISTS signature_id;

-- Finally drop the table itself. CASCADE for safety in case a
-- dependent object slipped past the explicit drops above.
DROP TABLE IF EXISTS vehicle_signatures CASCADE;
