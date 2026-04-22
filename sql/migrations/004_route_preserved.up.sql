-- Add a preserved flag to routes. When true, the cleanup worker must not
-- auto-delete the route. Modeled after comma connect's "preserve" behavior.
ALTER TABLE routes ADD COLUMN preserved BOOLEAN NOT NULL DEFAULT false;

-- Partial index keeps lookups of preserved routes fast without bloating the
-- index with rows that are never preserved.
CREATE INDEX idx_routes_preserved ON routes(dongle_id) WHERE preserved = true;
