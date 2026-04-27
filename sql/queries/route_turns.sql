-- name: DeleteTurnsForRoute :execrows
-- Wipe the route_turns rows for one route. The turn detector calls this
-- before re-running over a route so the new pass starts from a clean
-- slate. Returns the number of rows deleted.
DELETE FROM route_turns
WHERE dongle_id = $1 AND route = $2;

-- name: InsertTurns :execrows
-- Batch-insert detected turns for a route. Parallel arrays (one entry
-- per turn) are joined together via WITH ORDINALITY so the i-th element
-- of every array forms one row. ON CONFLICT DO NOTHING keeps
-- reprocessing idempotent: a turn whose (dongle_id, route,
-- turn_offset_ms) already exists is skipped. Callers should pair this
-- with DeleteTurnsForRoute when they want a full overwrite. gps_lat
-- and gps_lng are passed as arrays of DOUBLE PRECISION; SQL NULL
-- elements are preserved through the join.
INSERT INTO route_turns (
    dongle_id,
    route,
    turn_ts,
    turn_offset_ms,
    bearing_before_deg,
    bearing_after_deg,
    delta_deg,
    gps_lat,
    gps_lng
)
SELECT sqlc.arg('dongle_id')::TEXT,
       sqlc.arg('route')::TEXT,
       ts.v,
       off.v,
       bb.v,
       ba.v,
       d.v,
       lat.v,
       lng.v
FROM UNNEST(sqlc.arg('turn_ts')::TIMESTAMPTZ[])           WITH ORDINALITY AS ts(v, idx)
JOIN UNNEST(sqlc.arg('turn_offset_ms')::INT[])            WITH ORDINALITY AS off(v, idx) USING (idx)
JOIN UNNEST(sqlc.arg('bearing_before_deg')::REAL[])       WITH ORDINALITY AS bb(v, idx)  USING (idx)
JOIN UNNEST(sqlc.arg('bearing_after_deg')::REAL[])        WITH ORDINALITY AS ba(v, idx)  USING (idx)
JOIN UNNEST(sqlc.arg('delta_deg')::REAL[])                WITH ORDINALITY AS d(v, idx)   USING (idx)
JOIN UNNEST(sqlc.arg('gps_lat')::DOUBLE PRECISION[])      WITH ORDINALITY AS lat(v, idx) USING (idx)
JOIN UNNEST(sqlc.arg('gps_lng')::DOUBLE PRECISION[])      WITH ORDINALITY AS lng(v, idx) USING (idx)
ON CONFLICT (dongle_id, route, turn_offset_ms) DO NOTHING;

-- name: ListTurnsForRoute :many
-- Time-ordered turn list for a single route. Used by the route overlay
-- UI and by the stalking heuristic, which iterates turns to compare
-- against per-encounter time windows.
SELECT id, dongle_id, route, turn_ts, turn_offset_ms,
       bearing_before_deg, bearing_after_deg, delta_deg,
       gps_lat, gps_lng
FROM route_turns
WHERE dongle_id = $1 AND route = $2
ORDER BY turn_ts ASC, id ASC;

-- name: CountTurnsInWindow :one
-- Number of turns on a route that fall inside an inclusive time window.
-- Backs the stalking heuristic's "did the suspect plate take the same
-- N turns we did" check without forcing the heuristic to load every
-- turn for every encounter.
SELECT COUNT(*)::BIGINT
FROM route_turns
WHERE dongle_id = $1
  AND route     = $2
  AND turn_ts  >= sqlc.arg('window_start')
  AND turn_ts  <= sqlc.arg('window_end');
