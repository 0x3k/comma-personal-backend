-- name: GetSegment :one
SELECT id, route_id, segment_number,
       rlog_uploaded, qlog_uploaded,
       fcamera_uploaded, ecamera_uploaded,
       dcamera_uploaded, qcamera_uploaded,
       created_at
FROM segments
WHERE route_id = $1 AND segment_number = $2;

-- name: CreateSegment :one
INSERT INTO segments (route_id, segment_number)
VALUES ($1, $2)
RETURNING id, route_id, segment_number,
          rlog_uploaded, qlog_uploaded,
          fcamera_uploaded, ecamera_uploaded,
          dcamera_uploaded, qcamera_uploaded,
          created_at;

-- name: ListSegmentsByRoute :many
SELECT id, route_id, segment_number,
       rlog_uploaded, qlog_uploaded,
       fcamera_uploaded, ecamera_uploaded,
       dcamera_uploaded, qcamera_uploaded,
       created_at
FROM segments
WHERE route_id = $1
ORDER BY segment_number ASC;

-- name: UpdateSegmentUpload :exec
UPDATE segments
SET rlog_uploaded = COALESCE(sqlc.narg('rlog_uploaded'), rlog_uploaded),
    qlog_uploaded = COALESCE(sqlc.narg('qlog_uploaded'), qlog_uploaded),
    fcamera_uploaded = COALESCE(sqlc.narg('fcamera_uploaded'), fcamera_uploaded),
    ecamera_uploaded = COALESCE(sqlc.narg('ecamera_uploaded'), ecamera_uploaded),
    dcamera_uploaded = COALESCE(sqlc.narg('dcamera_uploaded'), dcamera_uploaded),
    qcamera_uploaded = COALESCE(sqlc.narg('qcamera_uploaded'), qcamera_uploaded)
WHERE route_id = sqlc.arg('route_id') AND segment_number = sqlc.arg('segment_number');
