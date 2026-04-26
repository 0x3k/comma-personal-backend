-- Sentry-relay ingest table.
--
-- The personal backend exposes a Sentry-compatible envelope endpoint at
-- POST /api/:project_id/envelope/ so devices can repoint their existing
-- sentry_sdk.init(...) DSN at us instead of sentry.io. Each event is
-- persisted here for the operator dashboard.
--
-- The raw_event column holds the original envelope JSON so the dashboard
-- can show fields we did not surface as columns yet (release, environment,
-- contexts, ...) without a schema migration.

CREATE TABLE crashes (
    id           SERIAL       PRIMARY KEY,
    -- Sentry's event_id (32-char hex). We do not enforce uniqueness
    -- because a misbehaving SDK could re-send the same event after a
    -- network blip, and we would rather record the duplicates than
    -- reject them.
    event_id     TEXT         NOT NULL,
    -- dongle_id pulled from envelope.user.id when present, else NULL.
    -- Indexed so the dashboard's "filter by device" query is fast.
    dongle_id    TEXT,
    -- Sentry level: error/warning/info/debug/fatal. Defaults to error
    -- because that is the level capture_exception uses.
    level        TEXT         NOT NULL DEFAULT 'error',
    message      TEXT         NOT NULL DEFAULT '',
    -- fingerprint identifies a single bug across many crashes. Sentry
    -- sends it as a list[str]; we serialize as JSONB so we keep order.
    fingerprint  JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- The envelope's tags, exception, and breadcrumbs as-is. These are
    -- frequently nested, so JSONB lets the dashboard pull individual
    -- fields without re-parsing the raw body each time.
    tags         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    exception    JSONB        NOT NULL DEFAULT '{}'::jsonb,
    breadcrumbs  JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- The full envelope payload, kept verbatim so we can extract more
    -- fields later without losing data.
    raw_event    JSONB        NOT NULL,
    -- occurred_at comes from envelope.timestamp. received_at is set by
    -- us at insert time so we can sort the dashboard list by ingest.
    occurred_at  TIMESTAMPTZ,
    received_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_crashes_received_at ON crashes(received_at DESC);
CREATE INDEX idx_crashes_dongle_id   ON crashes(dongle_id) WHERE dongle_id IS NOT NULL;
