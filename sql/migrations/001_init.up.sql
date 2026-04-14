-- Enable PostGIS for spatial data (route geometry).
CREATE EXTENSION IF NOT EXISTS postgis;

-- Devices registered with this backend.
CREATE TABLE devices (
    dongle_id  TEXT PRIMARY KEY,
    serial     TEXT,
    public_key TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Routes represent a single drive session (ignition to power-down).
CREATE TABLE routes (
    id         SERIAL PRIMARY KEY,
    dongle_id  TEXT NOT NULL REFERENCES devices(dongle_id),
    route_name TEXT NOT NULL,
    start_time TIMESTAMPTZ,
    end_time   TIMESTAMPTZ,
    geometry   GEOMETRY(LineString, 4326),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dongle_id, route_name)
);

CREATE INDEX idx_routes_dongle_id ON routes(dongle_id);

-- Segments are 1-minute chunks within a route.
CREATE TABLE segments (
    id               SERIAL PRIMARY KEY,
    route_id         INT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    segment_number   INT NOT NULL,
    rlog_uploaded     BOOLEAN NOT NULL DEFAULT false,
    qlog_uploaded     BOOLEAN NOT NULL DEFAULT false,
    fcamera_uploaded  BOOLEAN NOT NULL DEFAULT false,
    ecamera_uploaded  BOOLEAN NOT NULL DEFAULT false,
    dcamera_uploaded  BOOLEAN NOT NULL DEFAULT false,
    qcamera_uploaded  BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (route_id, segment_number)
);

CREATE INDEX idx_segments_route_id ON segments(route_id);
