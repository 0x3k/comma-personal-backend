-- name: UpsertEncounter :one
-- Insert-or-update an encounter row keyed on (dongle_id, route,
-- plate_hash, first_seen_ts). Re-running the aggregator over the same
-- route is idempotent: the unique constraint catches the conflict and
-- the DO UPDATE refreshes the mutable fields (last_seen_ts,
-- detection_count, turn_count, gap, signature_id, status, bbox_last,
-- updated_at). first_seen_ts and bbox_first are NOT updated because
-- they identify the encounter and changing them would defeat
-- idempotency.
INSERT INTO plate_encounters (
    dongle_id,
    route,
    plate_hash,
    first_seen_ts,
    last_seen_ts,
    detection_count,
    turn_count,
    max_internal_gap_seconds,
    signature_id,
    status,
    bbox_first,
    bbox_last
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (dongle_id, route, plate_hash, first_seen_ts) DO UPDATE
SET last_seen_ts             = EXCLUDED.last_seen_ts,
    detection_count          = EXCLUDED.detection_count,
    turn_count               = EXCLUDED.turn_count,
    max_internal_gap_seconds = EXCLUDED.max_internal_gap_seconds,
    signature_id             = EXCLUDED.signature_id,
    status                   = EXCLUDED.status,
    bbox_last                = EXCLUDED.bbox_last,
    updated_at               = now()
RETURNING id, dongle_id, route, plate_hash, first_seen_ts, last_seen_ts,
          detection_count, turn_count, max_internal_gap_seconds,
          signature_id, status, bbox_first, bbox_last,
          created_at, updated_at;

-- name: DeleteEncountersForRoute :execrows
-- Wipe the encounter rows for one route. The aggregator calls this
-- before re-running over a route so the new pass starts from a clean
-- slate. Returns the number of rows deleted so the caller can log how
-- much state it discarded.
DELETE FROM plate_encounters
WHERE dongle_id = $1 AND route = $2;

-- name: DeleteOrphanedEncounters :execrows
-- Retention sweep: drop encounter rows whose underlying detections have
-- already been pruned. After the per-detection retention pass runs, an
-- encounter whose entire (first_seen_ts, last_seen_ts) window has been
-- emptied of detections has no evidence left and should be cleaned up
-- to keep the per-route review UI from showing zombie rows.
--
-- Match semantics: an encounter survives if ANY detection still exists
-- with the same (dongle_id, route, plate_hash) AND a frame_ts inside
-- the encounter's [first_seen_ts, last_seen_ts] window. The plate_hash
-- column scopes the join so an encounter for plate A is not kept alive
-- by detections of plate B that happen to overlap in time.
-- Returns the number of orphan encounter rows deleted.
DELETE FROM plate_encounters pe
WHERE NOT EXISTS (
        SELECT 1
        FROM plate_detections d
        WHERE d.dongle_id  = pe.dongle_id
          AND d.route      = pe.route
          AND d.plate_hash = pe.plate_hash
          AND d.frame_ts BETWEEN pe.first_seen_ts AND pe.last_seen_ts
  );

-- name: ListEncountersForRoute :many
-- All encounter rows for a route, ordered by first_seen_ts. Powers the
-- per-route review UI ("which plates did we see on this drive").
SELECT id, dongle_id, route, plate_hash, first_seen_ts, last_seen_ts,
       detection_count, turn_count, max_internal_gap_seconds,
       signature_id, status, bbox_first, bbox_last,
       created_at, updated_at
FROM plate_encounters
WHERE dongle_id = $1 AND route = $2
ORDER BY first_seen_ts ASC, id ASC;

-- name: GetMostRecentEncounterForPlate :one
-- The single most-recently-finished encounter for a plate. Used by the
-- alert/whitelist listing endpoints to populate the latest_route panel
-- (which device, which route, when) without loading the full encounter
-- history. pgx.ErrNoRows when the plate has never been seen.
SELECT id, dongle_id, route, plate_hash, first_seen_ts, last_seen_ts,
       detection_count, turn_count, max_internal_gap_seconds,
       signature_id, status, bbox_first, bbox_last,
       created_at, updated_at
FROM plate_encounters
WHERE plate_hash = $1
ORDER BY last_seen_ts DESC NULLS LAST, id DESC
LIMIT 1;

-- name: CountEncountersForPlate :one
-- Number of encounter rows for a plate, used by the alert listing to
-- render an "encounter_count" badge without paging through the full
-- list. BIGINT so the count never overflows on a heavy producer.
SELECT COUNT(*)::BIGINT
FROM plate_encounters
WHERE plate_hash = $1;

-- name: ListEncountersForPlate :many
-- All encounters of a single plate across every route, newest first.
-- Backs the plate-detail page and feeds the stalking heuristic's
-- "recurring plate" lookups.
SELECT id, dongle_id, route, plate_hash, first_seen_ts, last_seen_ts,
       detection_count, turn_count, max_internal_gap_seconds,
       signature_id, status, bbox_first, bbox_last,
       created_at, updated_at
FROM plate_encounters
WHERE plate_hash = $1
ORDER BY last_seen_ts DESC, id DESC;

-- name: ListEncountersForPlateInWindow :many
-- Encounters for a plate that overlap a given time window. The window is
-- inclusive on both ends. Used by the stalking heuristic to count how
-- many recent times the suspect plate was seen, without paging through
-- the entire history.
SELECT id, dongle_id, route, plate_hash, first_seen_ts, last_seen_ts,
       detection_count, turn_count, max_internal_gap_seconds,
       signature_id, status, bbox_first, bbox_last,
       created_at, updated_at
FROM plate_encounters
WHERE plate_hash    = $1
  AND last_seen_ts >= $2
  AND first_seen_ts <= $3
ORDER BY last_seen_ts DESC, id DESC;

-- name: ListEncountersForPlateInWindowWithStartGPS :many
-- Same window scan as ListEncountersForPlateInWindow but joined with the
-- plate's first-detection GPS for that encounter. The stalking heuristic
-- needs the start coordinates of each encounter to bucket them into geo
-- cells (cross_route_geo_spread); without the join it would have to
-- issue one detection lookup per encounter.
--
-- LEFT JOIN: an encounter without a matching detection at first_seen_ts
-- (extremely rare, but possible after a manual correction or partial
-- delete) still appears with NULL gps fields; the heuristic treats those
-- as "no GPS" rather than dropping the encounter.
SELECT pe.id, pe.dongle_id, pe.route, pe.plate_hash,
       pe.first_seen_ts, pe.last_seen_ts,
       pe.detection_count, pe.turn_count, pe.max_internal_gap_seconds,
       pe.signature_id, pe.status, pe.bbox_first, pe.bbox_last,
       pe.created_at, pe.updated_at,
       pd.gps_lat AS start_lat,
       pd.gps_lng AS start_lng
FROM plate_encounters pe
LEFT JOIN LATERAL (
    SELECT gps_lat, gps_lng
    FROM plate_detections d
    WHERE d.dongle_id  = pe.dongle_id
      AND d.route      = pe.route
      AND d.plate_hash = pe.plate_hash
      AND d.frame_ts   = pe.first_seen_ts
    LIMIT 1
) pd ON TRUE
WHERE pe.plate_hash    = $1
  AND pe.last_seen_ts >= $2
  AND pe.first_seen_ts <= $3
ORDER BY pe.last_seen_ts DESC, pe.id DESC;

-- name: BulkUpdateEncountersPlateHash :execrows
-- Plate-merge path on the encounters table: rewrite plate_hash on every
-- encounter that currently points at the source hash. After the matching
-- detection-side update runs, the encounters' hash is rewritten so the
-- per-route review UI reflects the merged identity even before the
-- aggregator re-runs. The merge handler immediately re-triggers
-- aggregation on every affected route, which will DELETE+UPSERT
-- encounters cleanly; this UPDATE is the interim coherent-state pass.
--
-- The unique index on (dongle_id, route, plate_hash, first_seen_ts) can
-- in principle be violated if two encounters share the same first_seen_ts
-- after a merge. In practice that requires both encounters to start on
-- the exact same wall-clock instant in the same route, which is
-- vanishingly unlikely; if it ever fires the transaction rolls back and
-- the operator surfaces a 500, which is the right failure mode for a
-- rare data race.
UPDATE plate_encounters
SET plate_hash = sqlc.arg('new_plate_hash')
WHERE plate_hash = sqlc.arg('old_plate_hash');

-- name: DistinctRoutesForEncountersPlateHash :many
-- All (dongle_id, route) pairs that have at least one encounter for the
-- given plate_hash. Companion to DistinctRoutesForPlateHash on the
-- detections side; the merge handler unions the two so a route whose
-- detections were retention-pruned but whose encounter survives is still
-- re-aggregated. Ordered for deterministic enqueue order.
SELECT DISTINCT dongle_id, route
FROM plate_encounters
WHERE plate_hash = $1
ORDER BY dongle_id ASC, route ASC;

-- name: ListEncountersForSignatureInArea :many
-- Encounters linked to a vehicle signature whose first detection lies
-- inside a lat/lng bounding box. Used by the signature-fusion heuristic
-- to find "the same vehicle (by signature) showing up in our area" even
-- across partial plate observations. The bbox is supplied as four
-- scalars rather than a PostGIS geometry to keep this query independent
-- of the signature schema's geometry columns.
SELECT pe.id, pe.dongle_id, pe.route, pe.plate_hash, pe.first_seen_ts,
       pe.last_seen_ts, pe.detection_count, pe.turn_count,
       pe.max_internal_gap_seconds, pe.signature_id, pe.status,
       pe.bbox_first, pe.bbox_last, pe.created_at, pe.updated_at
FROM plate_encounters pe
JOIN plate_detections pd ON pd.dongle_id = pe.dongle_id
                        AND pd.route     = pe.route
                        AND pd.plate_hash = pe.plate_hash
                        AND pd.frame_ts  = pe.first_seen_ts
WHERE pe.signature_id = sqlc.arg('signature_id')
  AND pd.gps_lat BETWEEN sqlc.arg('min_lat') AND sqlc.arg('max_lat')
  AND pd.gps_lng BETWEEN sqlc.arg('min_lng') AND sqlc.arg('max_lng')
ORDER BY pe.last_seen_ts DESC, pe.id DESC;
