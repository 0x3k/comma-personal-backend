-- name: InsertDetection :one
-- Inserts a single plate detection. Returns the freshly-allocated id and
-- created_at so the caller can correlate the row to whatever in-memory
-- detection it came from.
INSERT INTO plate_detections (
    dongle_id,
    route,
    segment,
    frame_offset_ms,
    plate_ciphertext,
    plate_hash,
    bbox,
    confidence,
    ocr_corrected,
    gps_lat,
    gps_lng,
    gps_heading_deg,
    frame_ts,
    thumb_path
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING id, dongle_id, route, segment, frame_offset_ms,
          plate_ciphertext, plate_hash, bbox, confidence, ocr_corrected,
          gps_lat, gps_lng, gps_heading_deg, frame_ts, thumb_path,
          created_at;

-- name: ListDetectionsForRoute :many
-- All detections for a single route, in chronological order. Used by the
-- encounter aggregator and the per-route review UI. signature_id is
-- included so the aggregator can compute the mode signature per
-- encounter without a second query.
SELECT id, dongle_id, route, segment, frame_offset_ms,
       plate_ciphertext, plate_hash, bbox, confidence, ocr_corrected,
       gps_lat, gps_lng, gps_heading_deg, frame_ts, thumb_path,
       created_at, signature_id
FROM plate_detections
WHERE dongle_id = $1 AND route = $2
ORDER BY frame_ts ASC, id ASC;

-- name: ListDetectionsForRouteSince :many
-- Detections for a route at or after the given frame_ts. Used by the
-- aggregator when it processes a route incrementally (segment-by-segment)
-- so it does not re-scan already-aggregated detections on every pass.
SELECT id, dongle_id, route, segment, frame_offset_ms,
       plate_ciphertext, plate_hash, bbox, confidence, ocr_corrected,
       gps_lat, gps_lng, gps_heading_deg, frame_ts, thumb_path,
       created_at
FROM plate_detections
WHERE dongle_id = $1
  AND route     = $2
  AND frame_ts >= $3
ORDER BY frame_ts ASC, id ASC;

-- name: DeleteDetectionsOlderThan :execrows
-- Retention sweep: drop every detection older than the cutoff regardless
-- of watchlist state. This is the unconditional path used when the
-- operator opts out of "keep evidence for flagged plates" or when the
-- detection retention window is shorter than the alert evidence window.
-- Returns the number of rows actually deleted.
DELETE FROM plate_detections
WHERE frame_ts < $1;

-- name: DeleteDetectionsOlderThanForUnflagged :execrows
-- Retention sweep variant that preserves detections whose plate_hash
-- appears in plate_watchlist (whitelist or alerted). The default privacy
-- posture is to forget plates aggressively except for the small subset
-- the operator explicitly flagged. Returns the number of rows deleted.
DELETE FROM plate_detections d
WHERE d.frame_ts < $1
  AND NOT EXISTS (
        SELECT 1 FROM plate_watchlist w
        WHERE w.plate_hash = d.plate_hash
  );

-- name: DeleteDetectionsOlderThanExcludingFlagged :execrows
-- Tiered retention sweep: delete detections older than $1 EXCEPT those
-- whose plate_hash is in the supplied "flagged set" $2. The flagged set
-- is computed by the worker as the union of alerted+unacked watchlist
-- rows and severity >= 4 alerted rows; whitelisted and note-kind rows
-- are intentionally NOT in the flagged set so the operator's "this is
-- fine" classification drops the plate to the unflagged retention tier.
-- Returns the number of rows deleted.
--
-- The NOT IN (SELECT UNNEST(...)) form expands the bytea[] argument to
-- a row set Postgres can hash for the anti-join.
DELETE FROM plate_detections d
WHERE d.frame_ts < $1
  AND d.plate_hash NOT IN (
        SELECT UNNEST(@flagged_hashes::BYTEA[])
  );

-- name: UpdateDetectionPlate :exec
-- Manual-correction path: replace the encrypted plate text and its hash
-- on a single detection, and flip ocr_corrected to true. Both columns
-- are updated together so a corrected plate text always matches its
-- hash. Caller computes the new ciphertext and hash; the DB just
-- persists.
UPDATE plate_detections
SET plate_ciphertext = $2,
    plate_hash       = $3,
    ocr_corrected    = TRUE
WHERE id = $1;

-- name: UpdateDetectionPlateHash :execrows
-- Plate-merge path: rewrite plate_hash (and the matching ciphertext) on
-- every detection that currently points at the source hash. Returns the
-- number of rows rewritten so the merge UI can confirm the operation
-- touched what it expected. Both columns are written together so the
-- hash and ciphertext stay consistent.
UPDATE plate_detections
SET plate_hash       = sqlc.arg('new_plate_hash'),
    plate_ciphertext = sqlc.narg('new_plate_ciphertext')
WHERE plate_hash = sqlc.arg('old_plate_hash');

-- name: UpdateDetectionSignature :exec
-- Stamp the signature link plus the denormalized vehicle attribute
-- columns onto a single detection row. Called by the signature
-- classifier after it produces a (signature_id, make, model, color,
-- body_type) tuple for a frame. The denormalized columns are
-- intentionally co-written with the FK so review queries that filter
-- by attribute do not have to join through vehicle_signatures.
UPDATE plate_detections
SET signature_id        = sqlc.narg('signature_id'),
    det_make            = sqlc.narg('det_make'),
    det_model           = sqlc.narg('det_model'),
    det_color           = sqlc.narg('det_color'),
    det_body_type       = sqlc.narg('det_body_type'),
    det_attr_confidence = sqlc.narg('det_attr_confidence')
WHERE id = sqlc.arg('id');
