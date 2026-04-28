-- Queries for the ALPR historical backfill jobs table. The schema lives
-- in 017_alpr_backfill_jobs.up.sql. There is at most one row in
-- state='running' at a time (enforced by the partial unique index
-- alpr_backfill_jobs_one_running); the unique-violation that an
-- attempted second running INSERT trips on is what the API handler
-- maps to 409 Conflict.

-- name: InsertBackfillJob :one
-- Inserts a fresh job row in the requested state. Callers pass
-- state='running' to start the worker immediately (the partial unique
-- index then guards against a parallel start). filters_json is the
-- parsed start-request body. total_routes is computed by the API
-- handler (CountBackfillRoutes) before the insert so the worker has a
-- stable denominator for progress / ETA.
INSERT INTO alpr_backfill_jobs (
    state, filters_json, total_routes, started_by
)
VALUES ($1, $2, $3, $4)
RETURNING id, started_at, finished_at, state, filters_json,
          total_routes, processed_routes, last_processed_route,
          error, started_by;

-- name: GetBackfillJob :one
-- Fetches a single job row by id. Used by the worker on restart to
-- re-read state mid-loop (pause / resume / cancel cooperative checks).
SELECT id, started_at, finished_at, state, filters_json,
       total_routes, processed_routes, last_processed_route,
       error, started_by
FROM alpr_backfill_jobs
WHERE id = $1;

-- name: GetLatestBackfillJob :one
-- Returns the most-recently started job row. The status endpoint
-- (GET /v1/alpr/backfill/status) returns this, regardless of state.
-- Returns sql.ErrNoRows when no job has ever been recorded; the API
-- handler maps that to a 200 with an empty job envelope.
SELECT id, started_at, finished_at, state, filters_json,
       total_routes, processed_routes, last_processed_route,
       error, started_by
FROM alpr_backfill_jobs
ORDER BY started_at DESC, id DESC
LIMIT 1;

-- name: GetRunningBackfillJob :one
-- Returns the job currently in state='running', if any. The worker
-- calls this on startup to find the row to resume; the start handler
-- can use it as a pre-check before INSERT (the partial unique index is
-- still authoritative -- this is a fast-path).
SELECT id, started_at, finished_at, state, filters_json,
       total_routes, processed_routes, last_processed_route,
       error, started_by
FROM alpr_backfill_jobs
WHERE state = 'running'
LIMIT 1;

-- name: UpdateBackfillJobState :exec
-- Updates the state column on a job row. The API handlers (pause /
-- resume / cancel) and the worker (transition to 'paused' on
-- alpr_enabled=false, 'done' on completion, 'failed' on hard error)
-- all funnel through here. finished_at is set by the caller iff state
-- moves to a terminal value (done/failed); pass a NULL otherwise so
-- the column stays NULL until termination.
UPDATE alpr_backfill_jobs
SET state       = $2,
    finished_at = COALESCE(sqlc.narg('finished_at')::TIMESTAMPTZ, finished_at),
    error       = COALESCE(sqlc.narg('error_text')::TEXT, error)
WHERE id = $1;

-- name: IncrementBackfillJobProgress :exec
-- Bumps processed_routes by 1 and stamps last_processed_route after a
-- route's segments have all been enqueued (or skipped for being
-- already complete). Atomic so the worker's progress and resume token
-- can never disagree across a crash.
UPDATE alpr_backfill_jobs
SET processed_routes     = processed_routes + 1,
    last_processed_route = $2
WHERE id = $1;

-- name: CountBackfillRoutes :one
-- Returns the number of routes that match the start-request filter
-- set, used to seed total_routes on insert. Pass NULL for from_date /
-- to_date / dongle_id to disable that filter; pass 0 for max_routes
-- to disable the cap (matches the "unlimited" semantics callers
-- expect from omitted JSON fields). NULL filter args are evaluated
-- against the routes table without referencing trips so a fresh
-- install with no aggregated trips still counts correctly.
SELECT COUNT(*)::BIGINT AS total
FROM routes r
WHERE (sqlc.narg('from_date')::TIMESTAMPTZ IS NULL
       OR r.start_time >= sqlc.narg('from_date')::TIMESTAMPTZ)
  AND (sqlc.narg('to_date')::TIMESTAMPTZ IS NULL
       OR r.start_time <  sqlc.narg('to_date')::TIMESTAMPTZ)
  AND (sqlc.narg('dongle_id')::TEXT IS NULL
       OR r.dongle_id = sqlc.narg('dongle_id')::TEXT)
  AND (sqlc.narg('after_route')::TEXT IS NULL
       OR r.route_name > sqlc.narg('after_route')::TEXT);

-- name: ListBackfillRoutesAsc :many
-- Lists routes for the worker to process in chronological order
-- (oldest first). Filters mirror CountBackfillRoutes. after_route is
-- the resume token: pass last_processed_route to skip routes already
-- handled by an earlier pass of the same job. Pass NULL for any
-- filter to disable it. Limit caps the slice the worker walks per
-- batch -- the worker re-queries with the new last_processed_route
-- after each batch so a server crash mid-batch resumes correctly.
SELECT id, dongle_id, route_name, start_time, end_time, geometry,
       created_at, preserved, note, starred, geometry_times
FROM routes r
WHERE (sqlc.narg('from_date')::TIMESTAMPTZ IS NULL
       OR r.start_time >= sqlc.narg('from_date')::TIMESTAMPTZ)
  AND (sqlc.narg('to_date')::TIMESTAMPTZ IS NULL
       OR r.start_time <  sqlc.narg('to_date')::TIMESTAMPTZ)
  AND (sqlc.narg('dongle_id')::TEXT IS NULL
       OR r.dongle_id = sqlc.narg('dongle_id')::TEXT)
  AND (sqlc.narg('after_route')::TEXT IS NULL
       OR r.route_name > sqlc.narg('after_route')::TEXT)
ORDER BY r.route_name ASC, r.id ASC
LIMIT $1;

-- name: ListBackfillRoutesDesc :many
-- Same as ListBackfillRoutesAsc but newest first. The newest_first
-- knob in the start-request filter selects between the two.
-- after_route here means "route_name strictly less than this token"
-- so the resume sequence still moves monotonically toward older
-- routes.
SELECT id, dongle_id, route_name, start_time, end_time, geometry,
       created_at, preserved, note, starred, geometry_times
FROM routes r
WHERE (sqlc.narg('from_date')::TIMESTAMPTZ IS NULL
       OR r.start_time >= sqlc.narg('from_date')::TIMESTAMPTZ)
  AND (sqlc.narg('to_date')::TIMESTAMPTZ IS NULL
       OR r.start_time <  sqlc.narg('to_date')::TIMESTAMPTZ)
  AND (sqlc.narg('dongle_id')::TEXT IS NULL
       OR r.dongle_id = sqlc.narg('dongle_id')::TEXT)
  AND (sqlc.narg('after_route')::TEXT IS NULL
       OR r.route_name < sqlc.narg('after_route')::TEXT)
ORDER BY r.route_name DESC, r.id DESC
LIMIT $1;
