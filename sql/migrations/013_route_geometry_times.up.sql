-- Per-vertex timestamps for routes.geometry.
--
-- routes.geometry is a PostGIS LINESTRING that the route-metadata worker
-- builds from cereal.GpsPoint samples in the qlogs. The LINESTRING format
-- discards the per-vertex timestamps that those samples carried, but the
-- map UI needs them to render an accurate "current car position" dot that
-- tracks the playback head along the polyline. Without timestamps, the
-- only option is to assume points are uniform in time, which jumps over
-- intersections where the dedupe step collapses a long stop into a single
-- vertex.
--
-- We store the timestamps as route-relative milliseconds (point.Time -
-- start_time, in ms) in a parallel BIGINT[] whose length is intended to
-- match ST_NumPoints(geometry). The metadata worker writes both columns
-- in lockstep on every extraction. Keeping them parallel (rather than
-- migrating to LINESTRING M, which would change the WKT format and break
-- existing readers) means the column is fully additive: NULL on old rows
-- and the frontend falls back to time-fraction indexing while the worker
-- backfills.
ALTER TABLE routes
    ADD COLUMN geometry_times BIGINT[];

COMMENT ON COLUMN routes.geometry_times IS
    'Route-relative milliseconds for each vertex of routes.geometry, '
    'parallel array (length matches ST_NumPoints(geometry)). NULL for '
    'routes whose metadata predates the column; backfilled by the route '
    'metadata worker on its next pass.';
