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

-- name: DeleteOrphanedAlertEvents :execrows
-- Retention sweep: drop alert events older than the supplied cutoff
-- whose plate_hash is no longer in plate_watchlist. The watchlist row
-- is the load-bearing record for an active alert; once an operator has
-- removed it (or it was never persisted because the alert was demoted),
-- the heuristic evaluations behind it become trail-only and can age out
-- after a generous review window. Plates still on the watchlist keep
-- their full evaluation history regardless of computed_at -- the audit
-- trail is the basis for the "why is this plate alerted" UI.
-- Returns the number of rows deleted.
DELETE FROM plate_alert_events e
WHERE e.computed_at < $1
  AND NOT EXISTS (
        SELECT 1 FROM plate_watchlist w
        WHERE w.plate_hash = e.plate_hash
  );
