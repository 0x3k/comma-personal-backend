-- Trips are derived, human-meaningful summaries of a route.
-- A route holds raw ingest state; a trip holds recomputed aggregate stats.
-- One trip per route, identified by route_id.
CREATE TABLE trips (
    id               SERIAL PRIMARY KEY,
    route_id         INT UNIQUE NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    distance_meters  DOUBLE PRECISION,
    duration_seconds INT,
    max_speed_mps    DOUBLE PRECISION,
    avg_speed_mps    DOUBLE PRECISION,
    engaged_seconds  INT,
    start_address    TEXT,
    end_address      TEXT,
    start_lat        DOUBLE PRECISION,
    start_lng        DOUBLE PRECISION,
    end_lat          DOUBLE PRECISION,
    end_lng          DOUBLE PRECISION,
    computed_at      TIMESTAMPTZ
);
