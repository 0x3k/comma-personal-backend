-- name: InsertAudit :one
-- Append an audit-log entry for an ALPR admin action. Returns the row so
-- the caller can echo the assigned id and created_at to the operator
-- (useful for the "show recent admin actions" UI).
INSERT INTO alpr_audit_log (action, actor, payload)
VALUES ($1, $2, $3)
RETURNING id, action, actor, payload, created_at;

-- name: ListAudit :many
-- Paginated audit log, newest first, with optional filters. Pass NULL
-- for @action_filter to disable that filter; same for @actor_filter.
-- The watchlist API and tuning UI both use this view, narrowing on the
-- relevant action set client-side or via the action filter.
SELECT id, action, actor, payload, created_at
FROM alpr_audit_log
WHERE (sqlc.narg('action_filter')::TEXT IS NULL
       OR action = sqlc.narg('action_filter')::TEXT)
  AND (sqlc.narg('actor_filter')::TEXT  IS NULL
       OR actor  = sqlc.narg('actor_filter')::TEXT)
ORDER BY created_at DESC, id DESC
LIMIT $1 OFFSET $2;
