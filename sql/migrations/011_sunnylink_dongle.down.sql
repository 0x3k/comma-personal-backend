DROP INDEX IF EXISTS idx_devices_sunnylink_dongle_id;

ALTER TABLE devices
    DROP COLUMN IF EXISTS sunnylink_public_key,
    DROP COLUMN IF EXISTS sunnylink_dongle_id;
