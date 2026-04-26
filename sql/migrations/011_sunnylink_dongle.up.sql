-- Sunnylink columns for the devices table.
--
-- A sunnypilot device runs both an athena and a sunnylink client in parallel,
-- each with its own dongle_id and registration. To keep one row per physical
-- device we link the second identity onto the existing devices row rather than
-- creating a separate table. NULL means the device has only registered with
-- comma so far.

ALTER TABLE devices
    ADD COLUMN sunnylink_dongle_id  TEXT UNIQUE,
    ADD COLUMN sunnylink_public_key TEXT;

-- Lookup index for the sunnylink WS auth path: the JWT carries the sunnylink
-- dongle_id in its `identity` claim, and we have to fetch the matching public
-- key to verify the signature. UNIQUE above already creates an index, but
-- making the intent explicit reads better in EXPLAINs.
CREATE INDEX IF NOT EXISTS idx_devices_sunnylink_dongle_id
    ON devices(sunnylink_dongle_id)
    WHERE sunnylink_dongle_id IS NOT NULL;
