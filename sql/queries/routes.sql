-- name: GetRoute :one
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times
FROM routes
WHERE dongle_id = $1 AND route_name = $2;

-- name: CreateRoute :one
INSERT INTO routes (dongle_id, route_name, start_time, end_time)
VALUES ($1, $2, $3, $4)
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times;

-- name: ListRoutesByDevice :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times
FROM routes
WHERE dongle_id = $1
ORDER BY created_at DESC;

-- name: ListRoutesByDevicePaginated :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times
FROM routes
WHERE dongle_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2 OFFSET $3;

-- name: CountRoutesByDevice :one
SELECT count(*) FROM routes WHERE dongle_id = $1;

-- name: CountRoutesByDeviceFiltered :one
-- Returns the number of routes for a device matching the same filter set as
-- the dashboard routes list. Pass NULL for any filter arg to disable it.
--
-- Routes without an aggregated trip row are INCLUDED unless a duration or
-- distance filter is set; with those filters they are excluded because there
-- is no trip to compare against.
--
-- has_events:
--   NULL  -> no filter
--   TRUE  -> only routes with at least one row in events
--   FALSE -> only routes with zero rows in events
-- Uses an EXISTS subquery (not LEFT JOIN GROUP BY) so the planner can use
-- the idx_events_route_id index.
--
-- starred:
--   NULL  -> no filter
--   TRUE  -> only routes.starred = true
--   FALSE -> only routes.starred = false
--
-- Tag filtering (per-tag AND) is NOT expressed here: it requires a variable
-- number of EXISTS subqueries driven by user input and is applied in the
-- hand-written wrapper in internal/db/routes_custom.go, appended to this
-- WHERE clause via %s placeholders.
SELECT COUNT(*)::BIGINT
FROM routes r
LEFT JOIN trips t ON t.route_id = r.id
WHERE r.dongle_id = sqlc.arg('dongle_id')
  AND (sqlc.narg('from_time')::timestamptz IS NULL
       OR r.start_time >= sqlc.narg('from_time')::timestamptz)
  AND (sqlc.narg('to_time')::timestamptz IS NULL
       OR r.start_time <  sqlc.narg('to_time')::timestamptz)
  AND (sqlc.narg('preserved')::bool IS NULL
       OR r.preserved = sqlc.narg('preserved')::bool)
  AND (sqlc.narg('min_duration_s')::int IS NULL
       OR (t.duration_seconds IS NOT NULL
           AND t.duration_seconds >= sqlc.narg('min_duration_s')::int))
  AND (sqlc.narg('max_duration_s')::int IS NULL
       OR (t.duration_seconds IS NOT NULL
           AND t.duration_seconds <= sqlc.narg('max_duration_s')::int))
  AND (sqlc.narg('min_distance_m')::double precision IS NULL
       OR (t.distance_meters IS NOT NULL
           AND t.distance_meters >= sqlc.narg('min_distance_m')::double precision))
  AND (sqlc.narg('max_distance_m')::double precision IS NULL
       OR (t.distance_meters IS NOT NULL
           AND t.distance_meters <= sqlc.narg('max_distance_m')::double precision))
  AND (sqlc.narg('has_events')::bool IS NULL
       OR (sqlc.narg('has_events')::bool = TRUE
           AND EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id))
       OR (sqlc.narg('has_events')::bool = FALSE
           AND NOT EXISTS (SELECT 1 FROM events e WHERE e.route_id = r.id)))
  AND (sqlc.narg('starred')::bool IS NULL
       OR r.starred = sqlc.narg('starred')::bool);

-- name: GetRouteByID :one
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times
FROM routes
WHERE id = $1;

-- name: SetRoutePreserved :one
UPDATE routes
SET preserved = $3
WHERE dongle_id = $1 AND route_name = $2
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times;

-- name: SetRouteNote :one
-- Updates the free-form note on a route, keyed by the route's numeric id.
UPDATE routes
SET note = $2
WHERE id = $1
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times;

-- name: SetRouteStarred :one
-- Toggles the starred/favorite flag on a route, keyed by the route's numeric id.
UPDATE routes
SET starred = $2
WHERE id = $1
RETURNING id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times;

-- name: ListPreservedRoutes :many
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times
FROM routes
WHERE preserved = true
ORDER BY created_at DESC;

-- name: ListStaleRoutes :many
-- Returns non-preserved routes whose end_time is older than the given cutoff.
-- Used by the cleanup worker; ordered by end_time asc so the oldest routes are
-- deleted first when MaxDeletionsPerRun caps the batch.
SELECT id, dongle_id, route_name, start_time, end_time, geometry, created_at, preserved, note, starred, geometry_times
FROM routes
WHERE preserved = false
  AND end_time IS NOT NULL
  AND end_time < $1
ORDER BY end_time ASC
LIMIT $2;

-- name: DeleteRoute :exec
-- Deletes a route row. Segments and trips reference it with ON DELETE CASCADE
-- so they are removed automatically.
DELETE FROM routes
WHERE id = $1;

-- name: UpdateRouteMetadata :exec
-- Writes any subset of (start_time, end_time, geometry, geometry_times) onto
-- the routes row, leaving the columns the caller did not supply untouched.
-- NULL inputs mean "do not change this column" -- a partial extraction (e.g.
-- timestamps were recovered but GPS samples were not) does not stomp prior
-- data, and re-runs of the metadata worker are idempotent for the columns
-- whose source data has not changed.
--
-- Geometry is built from a WKT string via ST_GeomFromText with SRID 4326.
-- The caller is responsible for emitting WKT only when there are at least
-- two points; this query trusts the input. The geometry column is set only
-- when the parsed WKT yields a LINESTRING with ST_NumPoints >= 2 -- a
-- single-point or empty WKT is silently dropped so a malformed extraction
-- does not corrupt the routes row downstream consumers (trip aggregator,
-- map view) read from. The same guard applies to geometry_times: we drop
-- the input if it would not match ST_NumPoints of the geometry that lands
-- on the row, so the parallel-array invariant (length(geometry_times) ==
-- ST_NumPoints(geometry)) is enforced server-side regardless of caller bugs.
UPDATE routes
SET start_time = COALESCE(sqlc.narg('start_time')::timestamptz, start_time),
    end_time   = COALESCE(sqlc.narg('end_time')::timestamptz,   end_time),
    geometry   = CASE
        WHEN sqlc.narg('geometry_wkt')::text IS NULL THEN geometry
        WHEN ST_NumPoints(ST_GeomFromText(sqlc.narg('geometry_wkt')::text, 4326)) < 2 THEN geometry
        ELSE ST_GeomFromText(sqlc.narg('geometry_wkt')::text, 4326)
    END,
    geometry_times = CASE
        -- No new times supplied: keep whatever is on the row.
        WHEN sqlc.narg('geometry_times_ms')::bigint[] IS NULL THEN geometry_times
        -- New times supplied but the matching geometry would be invalid /
        -- single-point: drop the times too so the parallel-array invariant
        -- never breaks. (When geometry_wkt is NULL we still know the row's
        -- geometry from the SET above resolves to the existing column.)
        WHEN sqlc.narg('geometry_wkt')::text IS NOT NULL
             AND ST_NumPoints(ST_GeomFromText(sqlc.narg('geometry_wkt')::text, 4326)) < 2
             THEN geometry_times
        -- New times supplied alongside new geometry: enforce parallel-array
        -- length. Mismatch (e.g. a worker bug truncated one but not the
        -- other) drops the times rather than write a corrupt row.
        WHEN sqlc.narg('geometry_wkt')::text IS NOT NULL
             AND ST_NumPoints(ST_GeomFromText(sqlc.narg('geometry_wkt')::text, 4326))
                 <> COALESCE(array_length(sqlc.narg('geometry_times_ms')::bigint[], 1), 0)
             THEN geometry_times
        -- New times supplied without new geometry: must match the existing
        -- geometry's vertex count, else drop.
        WHEN sqlc.narg('geometry_wkt')::text IS NULL
             AND geometry IS NOT NULL
             AND ST_NumPoints(geometry)
                 <> COALESCE(array_length(sqlc.narg('geometry_times_ms')::bigint[], 1), 0)
             THEN geometry_times
        -- Times supplied without any geometry (existing or new) is a no-op
        -- for the column: nothing to align against.
        WHEN sqlc.narg('geometry_wkt')::text IS NULL AND geometry IS NULL
             THEN geometry_times
        ELSE sqlc.narg('geometry_times_ms')::bigint[]
    END
WHERE id = sqlc.arg('id');

-- ListRoutesNeedingMetadata is a hand-written query in
-- internal/db/routes_custom.go because sqlc 1.x cannot resolve the LATERAL
-- subquery alias used to compute the latest segment timestamp (see the
-- existing ListRoutesForTripAggregation precedent for the same pattern).
