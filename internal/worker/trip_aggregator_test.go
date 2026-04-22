package worker

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"comma-personal-backend/internal/db"
)

// setupTestDB connects to the database named by DATABASE_URL and ensures
// a clean schema by applying every *.up.sql migration in order. The
// caller gets back a pgxpool ready for use and a cleanup function that
// closes the pool and drops the schema so back-to-back test runs stay
// deterministic. Returns (nil, nil, true) when the environment is not
// configured for integration tests, signaling the caller to t.Skip.
func setupTestDB(t *testing.T) (*pgxpool.Pool, func(), bool) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, nil, true
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to DATABASE_URL: %v", err)
	}

	// Liveness check: give up quickly if the server is not actually
	// reachable rather than failing later in a confusing spot.
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("DATABASE_URL points at an unreachable database: %v", err)
	}

	resetSchema(ctx, t, pool)
	applyMigrationsT(ctx, t, pool)

	cleanup := func() {
		resetSchema(context.Background(), t, pool)
		pool.Close()
	}
	return pool, cleanup, false
}

// resetSchema drops every object we create in the migrations so the
// next test run starts from a known-empty public schema. We don't drop
// the PostGIS extension because it is expensive to reinstall and the
// other tables live in the public schema.
func resetSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	// Order matters only for ergonomics; CASCADE drops dependents anyway.
	stmts := []string{
		`DROP TABLE IF EXISTS events CASCADE`,
		`DROP TABLE IF EXISTS trips CASCADE`,
		`DROP TABLE IF EXISTS segments CASCADE`,
		`DROP TABLE IF EXISTS routes CASCADE`,
		`DROP TABLE IF EXISTS device_params CASCADE`,
		`DROP TABLE IF EXISTS devices CASCADE`,
		`DROP TABLE IF EXISTS ui_users CASCADE`,
		`DROP TABLE IF EXISTS settings CASCADE`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("reset schema (%s): %v", s, err)
		}
	}
}

// applyMigrations applies all sql/migrations/*.up.sql files in numeric
// order against the pool. We hunt for the migrations directory by
// walking upward from the current working directory so this works both
// under `go test ./internal/worker/...` and from the repo root.
func applyMigrationsT(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	migDir := findMigrationsDirT(t)
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("failed to read migrations dir %s: %v", migDir, err)
	}
	var ups []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	sort.Strings(ups)

	for _, name := range ups {
		path := filepath.Join(migDir, name)
		bytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(bytes)); err != nil {
			t.Fatalf("migration %s failed: %v", name, err)
		}
	}
}

// findMigrationsDir walks up the tree from the current working directory
// looking for sql/migrations. Go runs tests with cwd = the package dir,
// so we need to go two levels up for internal/worker.
func findMigrationsDirT(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get cwd: %v", err)
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "sql", "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find sql/migrations starting from %s", cwd)
	return ""
}

// seedRouteAndSegment creates a device, a route with optional WKT
// geometry, and a segment anchored in the past so the route looks
// finalized. Returns the route id so the test can look up its trip.
func seedRouteAndSegment(t *testing.T, pool *pgxpool.Pool, dongleID, routeName, wkt string, start, end time.Time, segmentAge time.Duration) int32 {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO devices (dongle_id, serial, public_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (dongle_id) DO NOTHING
	`, dongleID, "serial-"+dongleID, "pk-"+dongleID); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	var routeID int32
	if wkt == "" {
		err := pool.QueryRow(ctx, `
			INSERT INTO routes (dongle_id, route_name, start_time, end_time)
			VALUES ($1, $2, $3, $4)
			RETURNING id
		`, dongleID, routeName, start, end).Scan(&routeID)
		if err != nil {
			t.Fatalf("seed route (no geometry): %v", err)
		}
	} else {
		err := pool.QueryRow(ctx, `
			INSERT INTO routes (dongle_id, route_name, start_time, end_time, geometry)
			VALUES ($1, $2, $3, $4, ST_GeomFromText($5, 4326))
			RETURNING id
		`, dongleID, routeName, start, end, wkt).Scan(&routeID)
		if err != nil {
			t.Fatalf("seed route (with geometry): %v", err)
		}
	}

	// A segment whose created_at is older than the aggregator's
	// FinalizedAfter window makes the route eligible immediately.
	segCreatedAt := time.Now().Add(-segmentAge)
	if _, err := pool.Exec(ctx, `
		INSERT INTO segments (route_id, segment_number, created_at)
		VALUES ($1, $2, $3)
	`, routeID, 0, segCreatedAt); err != nil {
		t.Fatalf("seed segment: %v", err)
	}

	return routeID
}

func newAggregatorForTest(pool *pgxpool.Pool) *TripAggregator {
	a := NewTripAggregator(db.New(pool), nil)
	// Tight timings so tests don't wait around. FinalizedAfter is kept
	// short and equal across tests; callers push segment created_at far
	// enough into the past that the route is immediately eligible.
	a.PollInterval = 10 * time.Millisecond
	a.FinalizedAfter = 100 * time.Millisecond
	a.BatchLimit = 50
	return a
}

func TestTripAggregator_FreshRouteComputesTrip(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "aggr-fresh-1"
	route := "2024-05-01--10-00-00"
	// A ~1km WKT (three points spanning about 0.01 degrees of longitude
	// at the equator -- roughly 1.1km).
	wkt := "LINESTRING(-122.40 37.70, -122.395 37.70, -122.39 37.70)"
	start := time.Now().Add(-10 * time.Minute)
	end := start.Add(5 * time.Minute)
	routeID := seedRouteAndSegment(t, pool, dongle, route, wkt, start, end, 1*time.Hour)

	a := newAggregatorForTest(pool)
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	trip, err := a.Queries.GetTripByRouteID(ctx, routeID)
	if err != nil {
		t.Fatalf("GetTripByRouteID: %v", err)
	}

	if !trip.DistanceMeters.Valid || trip.DistanceMeters.Float64 <= 0 {
		t.Errorf("distance_meters = %+v, want valid positive value", trip.DistanceMeters)
	}
	if !trip.DurationSeconds.Valid || trip.DurationSeconds.Int32 != int32((5*time.Minute).Seconds()) {
		t.Errorf("duration_seconds = %+v, want %d", trip.DurationSeconds, int32((5 * time.Minute).Seconds()))
	}
	if !trip.AvgSpeedMps.Valid || trip.AvgSpeedMps.Float64 <= 0 {
		t.Errorf("avg_speed_mps = %+v, want valid positive value", trip.AvgSpeedMps)
	}
	if !trip.MaxSpeedMps.Valid || trip.MaxSpeedMps.Float64 <= 0 {
		t.Errorf("max_speed_mps = %+v, want valid positive value", trip.MaxSpeedMps)
	}
	// Start and end endpoints should match the WKT.
	if !trip.StartLat.Valid || abs(trip.StartLat.Float64-37.70) > 1e-6 {
		t.Errorf("start_lat = %+v, want ~37.70", trip.StartLat)
	}
	if !trip.StartLng.Valid || abs(trip.StartLng.Float64-(-122.40)) > 1e-6 {
		t.Errorf("start_lng = %+v, want ~-122.40", trip.StartLng)
	}
	if !trip.EndLat.Valid || abs(trip.EndLat.Float64-37.70) > 1e-6 {
		t.Errorf("end_lat = %+v, want ~37.70", trip.EndLat)
	}
	if !trip.EndLng.Valid || abs(trip.EndLng.Float64-(-122.39)) > 1e-6 {
		t.Errorf("end_lng = %+v, want ~-122.39", trip.EndLng)
	}
	if !trip.ComputedAt.Valid {
		t.Error("computed_at is NULL, want a timestamp")
	}
	// Engagement is deliberately left NULL until the event detector fills it.
	if trip.EngagedSeconds.Valid {
		t.Errorf("engaged_seconds = %+v, want NULL at this stage", trip.EngagedSeconds)
	}
}

func TestTripAggregator_RerunIsIdempotent(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "aggr-idem-1"
	route := "2024-05-02--10-00-00"
	wkt := "LINESTRING(-122.40 37.70, -122.39 37.70)"
	start := time.Now().Add(-10 * time.Minute)
	end := start.Add(1 * time.Minute)
	routeID := seedRouteAndSegment(t, pool, dongle, route, wkt, start, end, 1*time.Hour)

	a := newAggregatorForTest(pool)
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	first, err := a.Queries.GetTripByRouteID(ctx, routeID)
	if err != nil {
		t.Fatalf("first GetTripByRouteID: %v", err)
	}

	// Second pass: the new computed_at should be >= the first (upsert
	// updates the row), but the row identity (trips.id) must stay stable
	// and there should still be exactly one row per route.
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	second, err := a.Queries.GetTripByRouteID(ctx, routeID)
	if err != nil {
		t.Fatalf("second GetTripByRouteID: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("trips.id changed on re-run: first=%d second=%d", first.ID, second.ID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM trips WHERE route_id = $1`, routeID).Scan(&count); err != nil {
		t.Fatalf("count trips: %v", err)
	}
	if count != 1 {
		t.Errorf("trip row count = %d, want 1", count)
	}

	// Distance should be deterministic across runs.
	if first.DistanceMeters.Float64 != second.DistanceMeters.Float64 {
		t.Errorf("distance changed between runs: first=%v second=%v",
			first.DistanceMeters, second.DistanceMeters)
	}

	// Running a third pass when nothing new has happened should be a
	// no-op (route no longer eligible), so computed_at stays put.
	prevComputedAt := second.ComputedAt.Time
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	third, err := a.Queries.GetTripByRouteID(ctx, routeID)
	if err != nil {
		t.Fatalf("third GetTripByRouteID: %v", err)
	}
	if !third.ComputedAt.Time.Equal(prevComputedAt) {
		t.Errorf("computed_at advanced on steady-state re-run: prev=%v now=%v",
			prevComputedAt, third.ComputedAt.Time)
	}
}

func TestTripAggregator_MissingGeometryLeavesStatsNull(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "aggr-nogps-1"
	route := "2024-05-03--10-00-00"
	start := time.Now().Add(-10 * time.Minute)
	end := start.Add(2 * time.Minute)
	routeID := seedRouteAndSegment(t, pool, dongle, route, "", start, end, 1*time.Hour)

	a := newAggregatorForTest(pool)
	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	trip, err := a.Queries.GetTripByRouteID(ctx, routeID)
	if err != nil {
		t.Fatalf("GetTripByRouteID: %v", err)
	}
	if trip.DistanceMeters.Valid {
		t.Errorf("distance_meters = %+v, want NULL for no-GPS route", trip.DistanceMeters)
	}
	if trip.AvgSpeedMps.Valid {
		t.Errorf("avg_speed_mps = %+v, want NULL for no-GPS route", trip.AvgSpeedMps)
	}
	if trip.MaxSpeedMps.Valid {
		t.Errorf("max_speed_mps = %+v, want NULL for no-GPS route", trip.MaxSpeedMps)
	}
	if trip.StartLat.Valid || trip.StartLng.Valid || trip.EndLat.Valid || trip.EndLng.Valid {
		t.Errorf("endpoints valid despite missing geometry: %+v %+v %+v %+v",
			trip.StartLat, trip.StartLng, trip.EndLat, trip.EndLng)
	}
	// Duration is still computed from the route timestamps even without GPS.
	if !trip.DurationSeconds.Valid || trip.DurationSeconds.Int32 != int32((2*time.Minute).Seconds()) {
		t.Errorf("duration_seconds = %+v, want %d", trip.DurationSeconds, int32((2 * time.Minute).Seconds()))
	}
	if !trip.ComputedAt.Valid {
		t.Error("computed_at is NULL, want a timestamp")
	}
}

func TestTripAggregator_SkipsRoutesWithRecentSegments(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "aggr-recent-1"
	route := "2024-05-04--10-00-00"
	wkt := "LINESTRING(-122.40 37.70, -122.39 37.70)"
	start := time.Now().Add(-1 * time.Minute)
	end := start.Add(30 * time.Second)
	// Segment created "just now" (well inside the FinalizedAfter window
	// of 100ms used by newAggregatorForTest, plus a margin).
	routeID := seedRouteAndSegment(t, pool, dongle, route, wkt, start, end, 1*time.Millisecond)

	a := newAggregatorForTest(pool)
	// Pump up FinalizedAfter so the just-created segment counts as "still
	// uploading". The default in newAggregatorForTest is 100ms which is
	// too aggressive to reliably classify "1ms ago" as recent on busy CI.
	a.FinalizedAfter = 5 * time.Second

	if err := a.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	_, err := a.Queries.GetTripByRouteID(ctx, routeID)
	if err == nil {
		t.Fatal("expected no trip for route with a recent segment, got one")
	}
	if !strings.Contains(err.Error(), "no rows") {
		t.Fatalf("unexpected error fetching absent trip: %v", err)
	}
}

// abs is a local helper to keep the assertions readable without importing math.
func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
