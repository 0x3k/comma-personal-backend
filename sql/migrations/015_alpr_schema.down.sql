-- Reverse of 015_alpr_schema.up.sql. Tables are dropped in reverse order
-- of creation; CASCADE handles any indexes/constraints attached to them.
-- IF EXISTS so the down migration is safe to apply repeatedly or against
-- a partially-applied schema.
DROP TABLE IF EXISTS alpr_audit_log         CASCADE;
DROP TABLE IF EXISTS alpr_segment_progress  CASCADE;
DROP TABLE IF EXISTS route_turns            CASCADE;
DROP TABLE IF EXISTS plate_alert_events     CASCADE;
DROP TABLE IF EXISTS plate_watchlist        CASCADE;
DROP TABLE IF EXISTS plate_encounters       CASCADE;
DROP TABLE IF EXISTS plate_detections       CASCADE;
