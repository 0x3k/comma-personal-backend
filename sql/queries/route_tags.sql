-- name: AddRouteTag :exec
-- Adds a single tag to a route. Idempotent: the PRIMARY KEY (route_id, tag)
-- plus ON CONFLICT DO NOTHING means re-adding an existing tag is a no-op.
INSERT INTO route_tags (route_id, tag)
VALUES ($1, $2)
ON CONFLICT (route_id, tag) DO NOTHING;

-- name: RemoveRouteTag :exec
DELETE FROM route_tags
WHERE route_id = $1 AND tag = $2;

-- name: ListTagsForRoute :many
-- Returns the tags attached to a route in stable alphabetical order.
SELECT tag
FROM route_tags
WHERE route_id = $1
ORDER BY tag ASC;

-- name: ListTagsForDevice :many
-- Returns the distinct set of tags that exist across all of a device's routes,
-- alphabetically sorted. Backs the tag-picker/autocomplete in the UI.
SELECT DISTINCT rt.tag
FROM route_tags rt
JOIN routes r ON r.id = rt.route_id
WHERE r.dongle_id = $1
ORDER BY rt.tag ASC;

-- name: ReplaceRouteTagsDelete :exec
-- First half of ReplaceRouteTags: clears the existing tag set for a route.
-- Callers must run this inside the same transaction as the subsequent
-- AddRouteTag inserts; the Go wrapper ReplaceRouteTags handles that.
DELETE FROM route_tags
WHERE route_id = $1;
