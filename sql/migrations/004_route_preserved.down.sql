DROP INDEX IF EXISTS idx_routes_preserved;
ALTER TABLE routes DROP COLUMN IF EXISTS preserved;
