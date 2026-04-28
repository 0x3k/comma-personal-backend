-- Reverse of 018_alpr_notifications_sent.up.sql. Dropping the table also
-- drops its primary key index implicitly. IF EXISTS keeps a partial
-- rollback safe to re-run.
DROP TABLE IF EXISTS alpr_notifications_sent;
