-- ALPR (Automatic License Plate Reader) core schema.
--
-- Defines the persistent storage for plate detections, encounter aggregates,
-- the alert/whitelist watchlist, alert event audit trail, the related
-- route_turns table that the stalking heuristic consumes, plus the worker
-- bookkeeping (alpr_segment_progress) and admin audit trail (alpr_audit_log)
-- shared by the watchlist API and the tuning UI.
--
-- Privacy model: plate text never lands in the database in cleartext. The
-- ciphertext column stores the encrypted plate string, and plate_hash holds
-- a 32-byte HMAC-SHA256 hash of the canonical plate so we can index, dedupe,
-- and join without ever decrypting. Encryption itself is implemented in a
-- downstream feature (alpr-encryption-at-rest); this migration only
-- declares the columns of the right type.
--
-- No FK constraints are placed against the existing devices/routes tables.
-- ALPR data is logically orthogonal: we want it deletable independently from
-- the parent route, the retention worker writes both sides, and FK cascades
-- would force a circular cleanup ordering. (dongle_id, route) indexes are
-- sufficient to make the joins the queries actually need fast.
--
-- All CREATE statements are guarded with IF NOT EXISTS so this migration is
-- safe to run on partially-applied databases. Down migration drops in the
-- reverse order.

-- Per-frame plate observations. One row per detected plate per frame. The
-- detection worker writes these; the encounter aggregator collapses them
-- into plate_encounters; the retention sweeper deletes them based on
-- frame_ts when the plate is not on the watchlist.
CREATE TABLE IF NOT EXISTS plate_detections (
    id                BIGSERIAL PRIMARY KEY,
    dongle_id         TEXT NOT NULL,
    route             TEXT NOT NULL,
    segment           INT  NOT NULL,
    -- Offset within the segment, in milliseconds, of the frame the plate
    -- was detected on. Combined with the segment number this pins the
    -- detection to an exact moment in the source video.
    frame_offset_ms   INT  NOT NULL,
    -- Encrypted cleartext plate. NULL is allowed for backfill or when the
    -- engine fails to OCR but still localizes a plate-shaped region.
    plate_ciphertext  BYTEA,
    -- HMAC-SHA256 of the canonical (uppercased, stripped) plate text.
    -- Always 32 bytes; the CHECK constraint enforces it at the DB layer
    -- so callers can not accidentally insert a truncated digest.
    plate_hash        BYTEA NOT NULL CHECK (octet_length(plate_hash) = 32),
    -- Pixel-space bounding box in the source frame: {x, y, w, h}. JSONB so
    -- we can extend with confidence-per-corner or extra annotations later
    -- without a schema bump.
    bbox              JSONB NOT NULL,
    -- OCR confidence in [0, 1]. REAL is plenty for a percentage.
    confidence        REAL  NOT NULL,
    -- True when an operator hand-corrected the OCR output via the manual
    -- correction API. Used by the encounter aggregator to weight corrected
    -- detections more heavily and by the merge UI to show provenance.
    ocr_corrected     BOOLEAN NOT NULL DEFAULT FALSE,
    -- Best-effort GPS context for the frame. NULL when GPS was unavailable
    -- (tunnel, indoor, cold start). Heading is in degrees from true north.
    gps_lat           DOUBLE PRECISION,
    gps_lng           DOUBLE PRECISION,
    gps_heading_deg   REAL,
    -- Wall-clock timestamp of the frame. Indexed for retention sweeps and
    -- per-route ordering.
    frame_ts          TIMESTAMPTZ NOT NULL,
    -- Optional path (relative to STORAGE_PATH) to a cached crop of the
    -- plate region for the review UI. NULL while the thumbnail is still
    -- being generated, and when an operator decides not to keep it.
    thumb_path        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-route detection list (ordered chronologically) is the most common
-- read pattern for the encounter aggregator and review UI.
CREATE INDEX IF NOT EXISTS idx_plate_detections_route_ts
    ON plate_detections(dongle_id, route, frame_ts);

-- Lookup all detections of a specific plate in reverse chronological order.
-- Powers the plate-detail page and the watchlist alert evidence pull.
CREATE INDEX IF NOT EXISTS idx_plate_detections_hash_ts
    ON plate_detections(plate_hash, frame_ts DESC);

-- Retention sweeps scan by frame_ts to find rows older than the configured
-- window. A plain btree on frame_ts keeps the cleanup query fast.
CREATE INDEX IF NOT EXISTS idx_plate_detections_frame_ts
    ON plate_detections(frame_ts);


-- One row per (route, plate) sighting cluster. The aggregator collapses a
-- run of detections for the same plate on the same route into a single
-- encounter row, so the heuristics and UIs deal with O(routes) rows
-- instead of O(frames). Re-aggregation must be idempotent; the unique
-- constraint enforces that.
CREATE TABLE IF NOT EXISTS plate_encounters (
    id                       BIGSERIAL PRIMARY KEY,
    dongle_id                TEXT  NOT NULL,
    route                    TEXT  NOT NULL,
    plate_hash               BYTEA NOT NULL CHECK (octet_length(plate_hash) = 32),
    first_seen_ts            TIMESTAMPTZ NOT NULL,
    last_seen_ts             TIMESTAMPTZ NOT NULL,
    -- Number of underlying plate_detections rows that contributed.
    detection_count          INT  NOT NULL,
    -- Number of distinct route_turns that fall inside the encounter's
    -- (first_seen_ts, last_seen_ts) window. Populated by the stalking
    -- heuristic; defaults to 0 so a plain encounter aggregation does not
    -- need to know about turns.
    turn_count               INT  NOT NULL DEFAULT 0,
    -- The largest gap between consecutive detections inside the encounter,
    -- in seconds. Used by the heuristic to penalize/credit "the car was
    -- behind us continuously vs. seen briefly twice".
    max_internal_gap_seconds INT  NOT NULL,
    -- Optional pointer to a vehicle signature row (defined by the
    -- alpr-vehicle-signature-schema feature). BIGINT NULL because the
    -- signature feature lives in its own migration and we do not want a
    -- cross-feature FK forcing migration ordering.
    signature_id             BIGINT,
    -- Lifecycle status. 'active' is freshly aggregated, 'reviewed' has
    -- been seen by an operator, 'whitelist' is allowlisted (matches a
    -- whitelist row in plate_watchlist), 'alerted' has fired an alert.
    status                   TEXT  NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'reviewed', 'whitelist', 'alerted')),
    -- Bbox of the first and last detection in the encounter, kept here so
    -- the UI can render a locator thumbnail without joining back to the
    -- detection table.
    bbox_first               JSONB,
    bbox_last                JSONB,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Idempotency: re-aggregating the same route should upsert the same
    -- (dongle_id, route, plate_hash, first_seen_ts) row instead of
    -- producing duplicates.
    UNIQUE (dongle_id, route, plate_hash, first_seen_ts)
);

-- Per-plate timeline view. Powers the plate-detail page and the stalking
-- heuristic's "how many recent encounters does this plate have" query.
CREATE INDEX IF NOT EXISTS idx_plate_encounters_hash_last_seen
    ON plate_encounters(plate_hash, last_seen_ts DESC);

-- Per-route encounter list, used by the route overlay UI and by the
-- aggregator when it deletes-and-reinserts a route's encounters during
-- reprocessing.
CREATE INDEX IF NOT EXISTS idx_plate_encounters_route
    ON plate_encounters(dongle_id, route);


-- Whitelist + alerted plates. One row per plate_hash; kind disambiguates
-- whether the row is suppressing alerts (whitelist), recording an active
-- alert (alerted), or just an operator note. UNIQUE on plate_hash means
-- there is at most one watchlist entry per plate -- transitions
-- (whitelist -> alerted, etc.) update the row in place.
CREATE TABLE IF NOT EXISTS plate_watchlist (
    id                BIGSERIAL PRIMARY KEY,
    plate_hash        BYTEA UNIQUE NOT NULL CHECK (octet_length(plate_hash) = 32),
    -- Encrypted operator-supplied label, e.g. "my car", "neighbor's truck".
    -- NULL when no label was provided.
    label_ciphertext  BYTEA,
    kind              TEXT NOT NULL
        CHECK (kind IN ('whitelist', 'alerted', 'note')),
    -- 1..5 severity for alerted rows, NULL for whitelist/note. CHECK lets
    -- alerts arrive without severity (NULL) until the heuristic computes
    -- one, while still rejecting out-of-range values.
    severity          SMALLINT CHECK (severity BETWEEN 1 AND 5),
    -- When the first/last alert fired for this plate. NULL for
    -- whitelist/note rows.
    first_alert_at    TIMESTAMPTZ,
    last_alert_at     TIMESTAMPTZ,
    -- When an operator acknowledged the most recent alert. NULL means
    -- still unacked. UnackWatchlist sets this back to NULL.
    acked_at          TIMESTAMPTZ,
    notes             TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backs the alert feed: list rows of a given kind ordered by recency of
-- the latest alert. NULL last_alert_at sorts last.
CREATE INDEX IF NOT EXISTS idx_plate_watchlist_kind_last_alert
    ON plate_watchlist(kind, last_alert_at DESC);


-- Audit trail of alert evaluations. Each row records why the heuristic
-- decided a given plate was alert-worthy, with the per-component
-- breakdown (which sub-rule contributed which points and what evidence
-- it had). Kept forever -- retention sweeps must NOT touch this table.
CREATE TABLE IF NOT EXISTS plate_alert_events (
    id                 BIGSERIAL PRIMARY KEY,
    plate_hash         BYTEA NOT NULL CHECK (octet_length(plate_hash) = 32),
    -- The route the heuristic was evaluating when it fired. NULL for
    -- cross-route evaluations (e.g. periodic recomputation that has no
    -- single triggering route).
    route              TEXT,
    dongle_id          TEXT,
    computed_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    severity           SMALLINT NOT NULL,
    -- List of {name, points, evidence} objects, one per heuristic
    -- component. JSONB so the heuristic can evolve without forcing a
    -- column-shape migration.
    components         JSONB NOT NULL,
    -- The heuristic version string ("v1", "v2-rolling-window", ...) so
    -- old events stay interpretable when the rules change.
    heuristic_version  TEXT NOT NULL
);


-- Significant turning maneuvers extracted from each route. Independently
-- useful for analytics (turn-by-turn drive review, intersection density
-- per route) and consumed by the stalking heuristic, which counts how
-- many turns the suspect plate also took with us. Not gated on
-- alpr_enabled.
CREATE TABLE IF NOT EXISTS route_turns (
    id                  BIGSERIAL PRIMARY KEY,
    dongle_id           TEXT NOT NULL,
    route               TEXT NOT NULL,
    turn_ts             TIMESTAMPTZ NOT NULL,
    -- Offset within the route, in milliseconds, of the apex of the turn.
    -- Combined with route this yields a stable identity for idempotent
    -- reprocessing.
    turn_offset_ms      INT NOT NULL,
    -- Heading immediately before/after the turn, in degrees true north.
    bearing_before_deg  REAL NOT NULL,
    bearing_after_deg   REAL NOT NULL,
    -- Signed delta (after - before, normalized into (-180, 180]). Negative
    -- = left turn, positive = right turn. Also stored explicitly so range
    -- queries can find e.g. "all >90 degree turns" without recomputing.
    delta_deg           REAL NOT NULL,
    gps_lat             DOUBLE PRECISION,
    gps_lng             DOUBLE PRECISION,
    -- Reprocessing the same route must not double up turns. Keyed on
    -- offset_ms because the turn detector emits one row per apex moment.
    UNIQUE (dongle_id, route, turn_offset_ms)
);

-- Time-ordered per-route turn list, used by the stalking heuristic and
-- the route overlay UI.
CREATE INDEX IF NOT EXISTS idx_route_turns_route_ts
    ON route_turns(dongle_id, route, turn_ts);


-- Worker bookkeeping: which segments have already been processed by the
-- frame extractor and the detection worker. Both columns are NULLable so
-- a segment can be flagged extractor-done before the detector has run.
-- Lives in this migration because it is purely schema (no business
-- logic), and downstream worker features key off it.
CREATE TABLE IF NOT EXISTS alpr_segment_progress (
    dongle_id              TEXT NOT NULL,
    route                  TEXT NOT NULL,
    segment                INT  NOT NULL,
    processed_at_extractor TIMESTAMPTZ,
    processed_at_detector  TIMESTAMPTZ,
    PRIMARY KEY (dongle_id, route, segment)
);


-- Append-only audit log for ALPR admin actions: watchlist edits, plate
-- text corrections, plate merges, ack/unack toggles, and tuning-UI
-- changes. The watchlist/correction APIs and the tuning UI write here so
-- operators have a clear paper trail of who changed what and when.
CREATE TABLE IF NOT EXISTS alpr_audit_log (
    id          BIGSERIAL PRIMARY KEY,
    -- Discriminator. Free-form to keep new actions cheap to add, but the
    -- expected vocabulary is: whitelist_add, whitelist_remove, plate_edit,
    -- plate_merge, ack, unack, tuning_change.
    action      TEXT NOT NULL,
    -- Identifier of the user who performed the action (UI session
    -- username) or NULL for system-initiated changes.
    actor       TEXT,
    -- Action-specific structured payload. JSONB so the audit log can
    -- record things like "old_label -> new_label" without a column-shape
    -- migration per action type.
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
