-- Web UI users. Separate from devices: these identities authenticate humans
-- logging into the dashboard, not openpilot devices uploading data.
CREATE TABLE ui_users (
    id            SERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
