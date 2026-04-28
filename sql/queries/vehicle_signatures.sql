-- name: UpsertSignatureByKey :one
-- Insert or update a signature row keyed on signature_key. The
-- signature_key is a canonical, low-cardinality fingerprint built by
-- the caller (e.g. 'toyota|camry|silver|sedan'); when two detections
-- produce the same key they collapse into the same row.
--
-- Behavior on conflict:
--   * sample_count is incremented by 1 to record the additional
--     observation. This is the recency-weighted mass the fusion
--     heuristic uses.
--   * make / model / color / body_type are filled in only when the
--     existing row has them as NULL. Already-set attributes are
--     preserved -- the first observation wins so a noisy follow-up
--     detection cannot stomp the canonical attribute. Callers that
--     want to override should use a dedicated update.
--   * confidence: if the new value is non-NULL and either the
--     existing value is NULL or the new value is higher, take it.
--     This keeps the row's confidence the best evidence we have so
--     far rather than bleeding it down with a worse later sample.
--   * updated_at is bumped to now() on every observation.
--
-- Returns the full row so the caller can use the (possibly fresh) id
-- as the signature_id FK on plate_detections / plate_encounters.
INSERT INTO vehicle_signatures (
    signature_key,
    make,
    model,
    color,
    body_type,
    confidence,
    sample_count
)
VALUES (
    sqlc.arg('signature_key'),
    sqlc.narg('make'),
    sqlc.narg('model'),
    sqlc.narg('color'),
    sqlc.narg('body_type'),
    sqlc.narg('confidence'),
    1
)
ON CONFLICT (signature_key) DO UPDATE
SET sample_count = vehicle_signatures.sample_count + 1,
    make         = COALESCE(vehicle_signatures.make,      EXCLUDED.make),
    model        = COALESCE(vehicle_signatures.model,     EXCLUDED.model),
    color        = COALESCE(vehicle_signatures.color,     EXCLUDED.color),
    body_type    = COALESCE(vehicle_signatures.body_type, EXCLUDED.body_type),
    confidence   = CASE
        WHEN EXCLUDED.confidence IS NULL THEN vehicle_signatures.confidence
        WHEN vehicle_signatures.confidence IS NULL THEN EXCLUDED.confidence
        WHEN EXCLUDED.confidence > vehicle_signatures.confidence THEN EXCLUDED.confidence
        ELSE vehicle_signatures.confidence
    END,
    updated_at   = now()
RETURNING id, signature_key, make, model, year_min, year_max, color,
          body_type, confidence, sample_count, created_at, updated_at;

-- name: GetSignature :one
-- Look up a single signature row by id. Returns pgx.ErrNoRows when the
-- id is not present.
SELECT id, signature_key, make, model, year_min, year_max, color,
       body_type, confidence, sample_count, created_at, updated_at
FROM vehicle_signatures
WHERE id = $1;

-- name: ListSignaturesForPlate :many
-- All signatures that have ever been associated with a given plate,
-- joined via plate_detections. DISTINCT collapses the per-detection
-- rows into one row per signature. Ordered by sample_count DESC so the
-- dominant signature for the plate is first; this is what the fusion
-- heuristic surfaces as "the canonical vehicle behind this plate".
SELECT DISTINCT vs.id, vs.signature_key, vs.make, vs.model, vs.year_min,
       vs.year_max, vs.color, vs.body_type, vs.confidence,
       vs.sample_count, vs.created_at, vs.updated_at
FROM vehicle_signatures vs
JOIN plate_detections pd ON pd.signature_id = vs.id
WHERE pd.plate_hash = $1
ORDER BY vs.sample_count DESC, vs.id ASC;

-- name: ListPlatesForSignature :many
-- Distinct plate_hashes that have ever been observed under a given
-- signature. This is the reverse direction of ListSignaturesForPlate
-- and is the core query for plate-swap detection: a signature with
-- many distinct plates is the suspicious case. Plate hashes are
-- ordered deterministically (by the plate_hash byte string) so paged
-- callers get stable output.
SELECT DISTINCT pd.plate_hash
FROM plate_detections pd
WHERE pd.signature_id = $1
ORDER BY pd.plate_hash ASC;

-- name: UpdateEncounterSignature :exec
-- Set signature_id on a single plate_encounters row. The encounter
-- aggregator computes the canonical signature for an encounter (the
-- mode of its detections) and writes it here in a follow-up pass.
-- Intentionally :exec rather than :one -- the caller already has the
-- encounter row and only needs the side effect.
UPDATE plate_encounters
SET signature_id = sqlc.narg('signature_id'),
    updated_at   = now()
WHERE id = sqlc.arg('id');
