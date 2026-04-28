-- Vehicle signature schema follow-up to 015_alpr_schema.
--
-- A "signature" is a coarse, low-cardinality fingerprint of a physical
-- vehicle (make, model, color, body type, optional year range). It is
-- stored separately from plate detections so a single physical vehicle
-- (one signature row) can be linked to multiple plates over time. That
-- relationship is what the downstream signature-fusion heuristic uses to
-- detect plate swaps -- "this signature was seen with three different
-- plates in the same week" is suspicious in a way that any one plate by
-- itself is not.
--
-- This migration is purely schema:
--   1. Creates vehicle_signatures.
--   2. Adds denormalized det_* attribute columns to plate_detections so
--      filters like "silver SUVs" do not have to join.
--   3. Wires signature_id FKs from plate_detections, plate_encounters,
--      and plate_watchlist into vehicle_signatures.
--
-- plate_encounters.signature_id was already declared as a plain BIGINT
-- (no FK constraint) by 015_alpr_schema, deliberately, so that this
-- migration could land independently. We now attach the FK constraint
-- via ALTER TABLE ... ADD CONSTRAINT (NOT ADD COLUMN), keeping the column
-- shape but giving it referential integrity.
--
-- ON DELETE SET NULL on every FK: if an operator deletes a signature
-- row, we want the dependent rows to stay (their plate-level evidence
-- is still useful) but with the signature link cleared.
--
-- All statements use IF NOT EXISTS / IF EXISTS where Postgres permits,
-- so the migration is safe to apply against partially-applied schemas.

CREATE TABLE IF NOT EXISTS vehicle_signatures (
    id            BIGSERIAL PRIMARY KEY,
    -- Canonical fingerprint string assembled from the lower-cased,
    -- pipe-joined attributes the heuristic decided are significant.
    -- Missing fields are elided rather than left as empty segments, e.g.
    -- 'toyota|camry|silver|sedan' or just 'silver|sedan' when make/model
    -- are unknown. UNIQUE so the upsert path is a single statement.
    signature_key TEXT    NOT NULL UNIQUE,
    make          TEXT,
    model         TEXT,
    -- Optional year range. Year is rarely OCR-recoverable so we keep
    -- both bounds nullable; when known we store a [min, max] window
    -- rather than a point so multiple model years can collapse into one
    -- signature.
    year_min      INT,
    year_max      INT,
    color         TEXT,
    -- Coarse body-type vocabulary. CHECK accepts NULL so unknown rows
    -- pass; all non-NULL values must be in the fixed set.
    body_type     TEXT
        CHECK (body_type IS NULL OR body_type IN (
            'sedan', 'suv', 'truck', 'hatchback', 'coupe',
            'van', 'wagon', 'motorcycle', 'other'
        )),
    -- Aggregate confidence in [0, 1] across the underlying detections
    -- that fed the signature. NULL until the signature has at least
    -- one observation.
    confidence    REAL,
    -- How many detections have contributed to this signature. The
    -- upsert path increments this; the encounter aggregator and
    -- fusion heuristic use it as a recency-weighted mass.
    sample_count  INT     NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Make+model lookup powers the signature directory UI ("show me all
-- silver Toyota Camrys we have on file") and supports fusion queries
-- that scan within a manufacturer when the body_type column is NULL.
CREATE INDEX IF NOT EXISTS idx_vehicle_signatures_make_model
    ON vehicle_signatures(make, model);


-- plate_detections gains a signature link plus denormalized attribute
-- columns. The denormalization is intentional: review queries like
-- "silver SUVs at intersection X last Tuesday" would otherwise need a
-- join through vehicle_signatures, and per-frame detection rows are
-- the dominant table by row count.
ALTER TABLE plate_detections
    ADD COLUMN IF NOT EXISTS signature_id        BIGINT
        REFERENCES vehicle_signatures(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS det_make            TEXT,
    ADD COLUMN IF NOT EXISTS det_model           TEXT,
    ADD COLUMN IF NOT EXISTS det_color           TEXT,
    ADD COLUMN IF NOT EXISTS det_body_type       TEXT,
    ADD COLUMN IF NOT EXISTS det_attr_confidence REAL;


-- plate_encounters.signature_id already exists (declared by 015 as a
-- plain BIGINT). Attach the FK constraint now that the target table
-- exists. Wrapped in a DO block so re-running the migration does not
-- error on the second pass: ADD CONSTRAINT does not have an
-- IF NOT EXISTS form in this Postgres version.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'plate_encounters_signature_id_fkey'
          AND conrelid = 'plate_encounters'::regclass
    ) THEN
        ALTER TABLE plate_encounters
            ADD CONSTRAINT plate_encounters_signature_id_fkey
            FOREIGN KEY (signature_id)
            REFERENCES vehicle_signatures(id)
            ON DELETE SET NULL;
    END IF;
END
$$;


-- plate_watchlist gains signature_id so a watchlist row can target a
-- signature instead of (or in addition to) a single plate_hash. This
-- powers the plate-swap alert path: when a flagged signature shows up
-- under a new plate, the watchlist match still fires.
ALTER TABLE plate_watchlist
    ADD COLUMN IF NOT EXISTS signature_id BIGINT
        REFERENCES vehicle_signatures(id) ON DELETE SET NULL;
