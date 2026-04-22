-- name: UpsertTrip :one
INSERT INTO trips (
    route_id,
    distance_meters,
    duration_seconds,
    max_speed_mps,
    avg_speed_mps,
    engaged_seconds,
    start_address,
    end_address,
    start_lat,
    start_lng,
    end_lat,
    end_lng,
    computed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (route_id) DO UPDATE SET
    distance_meters  = EXCLUDED.distance_meters,
    duration_seconds = EXCLUDED.duration_seconds,
    max_speed_mps    = EXCLUDED.max_speed_mps,
    avg_speed_mps    = EXCLUDED.avg_speed_mps,
    engaged_seconds  = EXCLUDED.engaged_seconds,
    start_address    = EXCLUDED.start_address,
    end_address      = EXCLUDED.end_address,
    start_lat        = EXCLUDED.start_lat,
    start_lng        = EXCLUDED.start_lng,
    end_lat          = EXCLUDED.end_lat,
    end_lng          = EXCLUDED.end_lng,
    computed_at      = EXCLUDED.computed_at
RETURNING id, route_id, distance_meters, duration_seconds, max_speed_mps,
          avg_speed_mps, engaged_seconds, start_address, end_address,
          start_lat, start_lng, end_lat, end_lng, computed_at;

-- name: GetTripByRouteID :one
SELECT id, route_id, distance_meters, duration_seconds, max_speed_mps,
       avg_speed_mps, engaged_seconds, start_address, end_address,
       start_lat, start_lng, end_lat, end_lng, computed_at
FROM trips
WHERE route_id = $1;

-- name: ListTripsByDongleID :many
SELECT t.id, t.route_id, t.distance_meters, t.duration_seconds, t.max_speed_mps,
       t.avg_speed_mps, t.engaged_seconds, t.start_address, t.end_address,
       t.start_lat, t.start_lng, t.end_lat, t.end_lng, t.computed_at,
       r.dongle_id, r.route_name, r.start_time
FROM trips t
JOIN routes r ON r.id = t.route_id
WHERE r.dongle_id = $1
ORDER BY r.start_time DESC NULLS LAST, r.id DESC
LIMIT $2 OFFSET $3;

-- name: SumTripStatsByDongleID :one
SELECT
    COALESCE(SUM(t.distance_meters), 0)::DOUBLE PRECISION AS total_distance,
    COALESCE(SUM(t.duration_seconds), 0)::BIGINT          AS total_duration,
    COALESCE(SUM(t.engaged_seconds), 0)::BIGINT           AS total_engaged,
    COUNT(*)::BIGINT                                      AS trip_count
FROM trips t
JOIN routes r ON r.id = t.route_id
WHERE r.dongle_id = $1;
