-- name: MarkExtractorProcessed :exec
-- Records that the frame-extractor stage finished a segment. Idempotent:
-- a re-run sets processed_at_extractor to the latest invocation, leaving
-- processed_at_detector untouched.
INSERT INTO alpr_segment_progress (
    dongle_id, route, segment, processed_at_extractor
)
VALUES ($1, $2, $3, $4)
ON CONFLICT (dongle_id, route, segment) DO UPDATE
SET processed_at_extractor = EXCLUDED.processed_at_extractor;

-- name: MarkDetectorProcessed :exec
-- Records that the detector stage finished a segment. Same idempotent
-- pattern as MarkExtractorProcessed but for the detector column.
INSERT INTO alpr_segment_progress (
    dongle_id, route, segment, processed_at_detector
)
VALUES ($1, $2, $3, $4)
ON CONFLICT (dongle_id, route, segment) DO UPDATE
SET processed_at_detector = EXCLUDED.processed_at_detector;

-- name: ListUnprocessedSegments :many
-- Returns segments inside (or adjacent to) the route whose extractor or
-- detector pass has not yet completed. The frame extractor and the
-- detector both call this with the appropriate filter to find work.
-- Pass require_extractor=true to find segments missing the extractor
-- pass; pass require_detector=true to find segments missing the
-- detector pass. Both filters can be combined.
SELECT dongle_id, route, segment,
       processed_at_extractor, processed_at_detector
FROM alpr_segment_progress
WHERE dongle_id = $1
  AND route     = $2
  AND ((sqlc.arg('require_extractor')::BOOLEAN = TRUE
        AND processed_at_extractor IS NULL)
    OR (sqlc.arg('require_detector')::BOOLEAN  = TRUE
        AND processed_at_detector  IS NULL))
ORDER BY segment ASC;

-- name: IsExtractorProcessed :one
-- Quick yes/no check for whether a single segment has been processed by
-- the frame extractor. The worker calls this before downloading the
-- segment's video to avoid redundant work after a crash/restart. Returns
-- false when the row does not exist.
SELECT COALESCE(
    (SELECT processed_at_extractor IS NOT NULL
     FROM alpr_segment_progress
     WHERE dongle_id = $1 AND route = $2 AND segment = $3),
    FALSE
)::BOOLEAN AS processed;

-- name: CountRouteDetectorProgress :one
-- Returns (extractor_processed, detector_processed, segments_total) for
-- a route. The detection worker calls this after MarkDetectorProcessed
-- to decide whether the route is fully done so it can emit a
-- RouteAlprDetectionsComplete event exactly once.
--
-- extractor_processed counts alpr_segment_progress rows whose
-- processed_at_extractor is set; detector_processed counts those whose
-- processed_at_detector is set; segments_total counts segments table
-- rows for the route. The route is fully detector-processed when
-- detector_processed = extractor_processed = segments_total. Comparing
-- against segments_total alone is not sufficient because the extractor
-- only inserts rows for segments that actually had an fcamera.hevc on
-- disk -- a segment without front-camera video never enters the ALPR
-- pipeline and would otherwise prevent the route from ever completing.
SELECT
    (SELECT COUNT(*) FROM alpr_segment_progress p
     WHERE p.dongle_id = sqlc.arg('dongle_id')
       AND p.route     = sqlc.arg('route')
       AND p.processed_at_extractor IS NOT NULL)::BIGINT
        AS extractor_processed,
    (SELECT COUNT(*) FROM alpr_segment_progress p
     WHERE p.dongle_id = sqlc.arg('dongle_id')
       AND p.route     = sqlc.arg('route')
       AND p.processed_at_detector  IS NOT NULL)::BIGINT
        AS detector_processed,
    (SELECT COUNT(*) FROM segments seg
     JOIN routes r ON r.id = seg.route_id
     WHERE r.dongle_id  = sqlc.arg('dongle_id')
       AND r.route_name = sqlc.arg('route'))::BIGINT
        AS segments_total;
