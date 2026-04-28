-- ALPR notification de-duplication ledger.
--
-- The notify dispatcher (internal/alpr/notify) writes one row here every
-- time it sends an out-of-band alert (email or webhook) for a plate so a
-- subsequent AlertCreated within ALPR_NOTIFY_DEDUP_HOURS for the same plate
-- is suppressed. The "ongoing situation" failure mode is the one this
-- ledger guards against: a plate trips the heuristic on every fresh
-- encounter while the user is still being followed, and we do not want to
-- spam the user's inbox each time.
--
-- plate_hash is the same 32-byte HMAC key used everywhere else in the ALPR
-- schema. The CHECK constraint mirrors plate_detections / plate_watchlist
-- so a truncated hash cannot land here. PRIMARY KEY on plate_hash plus an
-- UPSERT in the dispatcher gives us "track most recent send per plate"
-- without a separate index or housekeeping pass.
--
-- For signature-fusion alerts (Wave 7) that emit on a vehicle signature
-- rather than a plate hash, the dispatcher synthesises a stable 32-byte
-- key from the signature id ("sig:<id>" SHA-256'd) so the same dedup
-- table covers both alert sources without a second migration.
--
-- IF NOT EXISTS keeps the migration idempotent on partially-applied
-- databases, matching the rest of the ALPR schema.
CREATE TABLE IF NOT EXISTS alpr_notifications_sent (
    plate_hash    BYTEA       PRIMARY KEY CHECK (octet_length(plate_hash) = 32),
    last_sent_at  TIMESTAMPTZ NOT NULL    DEFAULT now()
);
