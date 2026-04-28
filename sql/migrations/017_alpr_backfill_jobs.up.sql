-- ALPR historical backfill jobs.
--
-- A backfill job records an opt-in pass over the user's accumulated
-- routes that funnels each fcamera segment through the live ALPR
-- pipeline (extractor + detector + aggregator) at a throttled rate so
-- live ingest is never starved. Single concurrent job at a time, DB-row
-- backed so two parallel backfills cannot fight, and crash-resumable so
-- a server restart picks up where it left off (the singleton worker
-- looks for any state='running' row on startup and resumes from
-- last_processed_route).
--
-- The partial unique index alpr_backfill_jobs_one_running enforces the
-- "at most one running" invariant at the DB layer: a second
-- POST /v1/alpr/backfill/start that races past the handler-level
-- already-running check still trips on this index and the start handler
-- maps the unique-constraint violation to 409 Conflict.
--
-- filters_json carries the parsed start-request body (from_date,
-- to_date, dongle_id, max_routes, newest_first) so the worker can
-- re-parse the filter on every pause/resume without reading the
-- original request. JSONB rather than typed columns: the filter shape
-- may grow (per-tag filter, route-status filter, ...) without forcing
-- a schema migration each time.

CREATE TABLE IF NOT EXISTS alpr_backfill_jobs (
    id                    BIGSERIAL PRIMARY KEY,
    started_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at           TIMESTAMPTZ,
    state                 TEXT NOT NULL
        CHECK (state IN ('queued', 'running', 'paused', 'done', 'failed')),
    -- Parsed start-request filters: {from_date, to_date, dongle_id,
    -- max_routes, newest_first}. Empty object {} means "all routes,
    -- oldest first". Stored once at insert and never mutated; pause and
    -- resume read this same value back so the filter is stable across
    -- the lifetime of the job.
    filters_json          JSONB NOT NULL DEFAULT '{}'::JSONB,
    -- Total routes the worker plans to process (snapshotted at start
    -- time so progress and ETA are stable even if new uploads arrive
    -- mid-backfill). NULL is allowed for queued rows that have not yet
    -- been claimed by the worker.
    total_routes          INT,
    -- Monotonic counter of routes the worker has finished. Incremented
    -- per route, never decremented (a re-run from last_processed_route
    -- continues from this value).
    processed_routes      INT NOT NULL DEFAULT 0,
    -- Route name (in the canonical "dongle|YYYY-MM-DD--HH-MM-SS" form)
    -- of the most recently completed route. The worker resumes from
    -- the route immediately after this on restart.
    last_processed_route  TEXT,
    -- When state='failed', the human-readable reason. Null for non-
    -- failed states; "cancelled by user" for the cancel handler;
    -- otherwise the worker's err.Error() at the point of failure.
    error                 TEXT,
    -- The session user (UI user id stamped as "user:<id>") who started
    -- the job. NULL is tolerated for system-initiated jobs (currently
    -- there are none, but the column is permissive so future automation
    -- can drop a job in without inventing a fake user id).
    started_by            TEXT
);

-- "At most one running job" invariant. PostgreSQL allows a partial
-- unique index over a constant expression, which is the canonical way
-- to enforce a single-row-of-this-shape constraint without a separate
-- locking table.
CREATE UNIQUE INDEX IF NOT EXISTS alpr_backfill_jobs_one_running
    ON alpr_backfill_jobs ((TRUE)) WHERE state = 'running';

-- Status endpoint reads "the latest job"; ordering by started_at
-- DESC + id DESC makes that fetch a single index seek.
CREATE INDEX IF NOT EXISTS idx_alpr_backfill_jobs_started
    ON alpr_backfill_jobs (started_at DESC, id DESC);
