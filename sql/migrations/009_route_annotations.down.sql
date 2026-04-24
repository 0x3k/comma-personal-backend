DROP TABLE IF EXISTS route_tags;

DROP INDEX IF EXISTS idx_routes_starred;

ALTER TABLE routes
    DROP COLUMN IF EXISTS note,
    DROP COLUMN IF EXISTS starred;
