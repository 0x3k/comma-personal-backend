-- The pre-backfill values were monotonic-clock garbage (all in 1970) and
-- are not recoverable. Rolling back is therefore a no-op; rerunning event
-- detection (or re-applying the up migration) will restore correct values.
SELECT 1;
