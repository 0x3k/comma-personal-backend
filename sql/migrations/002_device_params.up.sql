CREATE TABLE device_params (
    id         SERIAL PRIMARY KEY,
    dongle_id  TEXT NOT NULL REFERENCES devices(dongle_id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dongle_id, key)
);

CREATE INDEX idx_device_params_dongle_id ON device_params(dongle_id);
