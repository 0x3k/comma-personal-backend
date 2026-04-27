-- Per-route turn timeline derived from GPS geometry.
--
-- The turn detector worker scans a route's existing geometry vertices
-- (with timestamps from the parallel geometry_times array) and emits one
-- row per detected turn -- a heading change of >= TURN_DELTA_DEG_MIN
-- across a TURN_WINDOW_SECONDS window, with a TURN_DEDUP_SECONDS
-- suppression window so a rotary or sweeping curve doesn't fire 12 times.
--
-- The table is intentionally narrow: it stores just enough to drive the
-- ALPR stalking heuristic ("turns since first sighting of plate X") and a
-- future turn-marker UI on the route timeline. Geometry remains
-- authoritative on the routes row; turns are a derived, idempotently
-- recomputable side table.
--
-- IF NOT EXISTS guards make this migration coexistence-safe with the
-- alpr-schema feature, which may also declare route_turns. Both paths
-- produce the same table shape; whichever migration runs first wins, and
-- the second is a no-op.
CREATE TABLE IF NOT EXISTS route_turns (
    id                  BIGSERIAL    PRIMARY KEY,
    dongle_id           TEXT         NOT NULL,
    route               TEXT         NOT NULL,
    turn_ts             TIMESTAMPTZ  NOT NULL,
    turn_offset_ms      INTEGER      NOT NULL,
    bearing_before_deg  REAL         NOT NULL,
    bearing_after_deg   REAL         NOT NULL,
    delta_deg           REAL         NOT NULL,
    gps_lat             DOUBLE PRECISION,
    gps_lng             DOUBLE PRECISION
);

-- Idempotency anchor: the worker deletes-then-inserts per (dongle_id,
-- route) inside one transaction. The unique constraint on
-- (dongle_id, route, turn_offset_ms) catches any double-insert that
-- slips past the delete (e.g. two workers racing) so the table never
-- ends up with duplicate turns at the same offset.
CREATE UNIQUE INDEX IF NOT EXISTS route_turns_dongle_route_offset_unq
    ON route_turns (dongle_id, route, turn_offset_ms);

-- Read path: GET /v1/routes/:dongle_id/:route_name/turns lists turns in
-- chronological order. The (dongle_id, route, turn_ts) index covers both
-- that scan and any future "turns in time window" queries.
CREATE INDEX IF NOT EXISTS route_turns_dongle_route_ts_idx
    ON route_turns (dongle_id, route, turn_ts);
