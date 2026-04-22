-- Settings is a simple key/value store for operator-configurable runtime
-- settings that may be overridden without a restart. The initial use case is
-- retention_days (consumed by the cleanup worker); value 0 means never delete.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
