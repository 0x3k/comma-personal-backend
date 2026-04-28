-- Reverse of 017_alpr_backfill_jobs.up.sql. Drops the indexes first then
-- the table (Postgres drops the indexes implicitly with the table, but
-- we name them explicitly so a partial down + manual fix-up does not
-- leave dangling artifacts).
DROP INDEX IF EXISTS idx_alpr_backfill_jobs_started;
DROP INDEX IF EXISTS alpr_backfill_jobs_one_running;
DROP TABLE IF EXISTS alpr_backfill_jobs;
