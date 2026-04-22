-- Events are interesting moments detected from parsed logs (hard brakes,
-- disengagements, FCW, alerts, etc.). Each event is attached to a route,
-- timestamped both relative to the route start and in absolute wall time.
CREATE TABLE events (
    id                    SERIAL PRIMARY KEY,
    route_id              INT NOT NULL REFERENCES routes(id) ON DELETE CASCADE,
    type                  TEXT NOT NULL,
    severity              TEXT NOT NULL DEFAULT 'info',
    route_offset_seconds  DOUBLE PRECISION NOT NULL,
    occurred_at           TIMESTAMPTZ,
    payload               JSONB,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Idempotency: re-running a detector over the same route must not
    -- produce duplicate rows for the same (route, type, offset) triple.
    UNIQUE (route_id, type, route_offset_seconds)
);

CREATE INDEX idx_events_route_id ON events(route_id);
CREATE INDEX idx_events_type ON events(type);
CREATE INDEX idx_events_occurred_at_desc ON events(occurred_at DESC);
