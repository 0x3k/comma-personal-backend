-- name: InsertAlertEvent :one
-- Records why a heuristic decided a plate was alert-worthy. Append-only;
-- this table is never touched by retention sweeps because it is the
-- evidence trail behind every alert badge the operator might later
-- review.
INSERT INTO plate_alert_events (
    plate_hash,
    route,
    dongle_id,
    severity,
    components,
    heuristic_version
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, plate_hash, route, dongle_id, computed_at, severity,
          components, heuristic_version;

-- name: ListEventsForPlate :many
-- Paginated list of every heuristic evaluation for a plate, newest
-- first. Powers the "why is this plate alerted" drill-down on the plate
-- detail page.
SELECT id, plate_hash, route, dongle_id, computed_at, severity,
       components, heuristic_version
FROM plate_alert_events
WHERE plate_hash = $1
ORDER BY computed_at DESC, id DESC
LIMIT $2 OFFSET $3;
