-- name: DeleteTurnsForRoute :exec
DELETE FROM route_turns
WHERE dongle_id = $1 AND route = $2;

-- name: InsertTurn :exec
INSERT INTO route_turns (
    dongle_id,
    route,
    turn_ts,
    turn_offset_ms,
    bearing_before_deg,
    bearing_after_deg,
    delta_deg,
    gps_lat,
    gps_lng
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
ON CONFLICT (dongle_id, route, turn_offset_ms) DO NOTHING;

-- name: ListTurnsForRoute :many
SELECT id, dongle_id, route, turn_ts, turn_offset_ms,
       bearing_before_deg, bearing_after_deg, delta_deg,
       gps_lat, gps_lng
FROM route_turns
WHERE dongle_id = $1 AND route = $2
ORDER BY turn_offset_ms ASC, id ASC;

-- name: CountTurnsInWindow :one
SELECT COUNT(*) AS turn_count
FROM route_turns
WHERE dongle_id = $1
  AND route     = $2
  AND turn_ts  >= $3
  AND turn_ts  <= $4;

-- name: CountTurnsForRoute :one
SELECT COUNT(*) AS turn_count
FROM route_turns
WHERE dongle_id = $1 AND route = $2;
