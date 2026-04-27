-- Backfill events.occurred_at from the route's wall-clock start_time plus
-- route_offset_seconds. The event detector previously wrote occurred_at
-- straight from the device's monotonic clock (logMonoTime nanoseconds
-- treated as a Unix timestamp), which made every row land in 1970. The
-- (route_id, route_offset_seconds) pair is authoritative for "when in the
-- route this happened", so adding it to the route's start_time recovers the
-- correct wall-clock instant.
UPDATE events e
SET occurred_at = r.start_time + make_interval(secs => e.route_offset_seconds)
FROM routes r
WHERE e.route_id = r.id
  AND r.start_time IS NOT NULL;
