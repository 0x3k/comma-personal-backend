-- name: CreateRouteDataRequest :one
-- Inserts a fresh request row. status defaults to 'pending'; the caller
-- transitions it to 'dispatched' (or 'failed') after attempting the RPC.
INSERT INTO route_data_requests (
    route_id, requested_by, kind, status, files_requested,
    dispatched_at, error
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, route_id, requested_by, requested_at, kind, status,
          dispatched_at, completed_at, error, files_requested;

-- name: GetRouteDataRequestByID :one
SELECT id, route_id, requested_by, requested_at, kind, status,
       dispatched_at, completed_at, error, files_requested
FROM route_data_requests
WHERE id = $1;

-- name: GetLatestRouteDataRequestByRoute :one
-- Returns the most recent request for a route+kind. Used by the POST handler
-- to short-circuit duplicate dispatches within the idempotency window, and by
-- the polling UI to surface the current state without knowing the request id.
SELECT id, route_id, requested_by, requested_at, kind, status,
       dispatched_at, completed_at, error, files_requested
FROM route_data_requests
WHERE route_id = $1 AND kind = $2
ORDER BY requested_at DESC
LIMIT 1;

-- name: UpdateRouteDataRequestStatus :exec
-- Updates status (and the appropriate timestamp/error fields) on an existing
-- row. NULL inputs leave the corresponding column untouched so partial
-- progress updates do not stomp prior data.
UPDATE route_data_requests
SET status        = sqlc.arg('status'),
    dispatched_at = COALESCE(sqlc.narg('dispatched_at')::timestamptz, dispatched_at),
    completed_at  = COALESCE(sqlc.narg('completed_at')::timestamptz, completed_at),
    error         = COALESCE(sqlc.narg('error')::text, error)
WHERE id = sqlc.arg('id');

-- name: ListPendingRouteDataRequests :many
-- Returns pending rows that the dispatcher worker should retry. Joined with
-- routes so the worker has the dongle id needed to look the device up on the
-- WebSocket hub without a second query per row.
SELECT r.id          AS request_id,
       r.route_id    AS route_id,
       r.requested_by,
       r.requested_at,
       r.kind        AS kind,
       r.status      AS status,
       r.files_requested,
       rt.dongle_id  AS dongle_id,
       rt.route_name AS route_name
FROM route_data_requests r
JOIN routes rt ON rt.id = r.route_id
WHERE r.status = 'pending'
ORDER BY r.requested_at ASC
LIMIT $1;
