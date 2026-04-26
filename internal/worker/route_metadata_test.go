package worker

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"comma-personal-backend/internal/cereal"
	"comma-personal-backend/internal/db"
)

// seedRouteForMetadata inserts a device + route + segment with the given
// segment age so the route is immediately eligible for the metadata
// worker. Crucially the route is created with NULL start_time / end_time /
// geometry to mimic the production state created by the upload handler
// (internal/api/upload.go calls CreateRoute with only DongleID + RouteName).
//
// Returns the route id so tests can assert the worker's UPDATE landed on
// the right row.
func seedRouteForMetadata(t *testing.T, pool *pgxpool.Pool, dongleID, routeName string, segmentAge time.Duration) int32 {
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
	if err := pool.QueryRow(ctx, `
		INSERT INTO routes (dongle_id, route_name)
		VALUES ($1, $2)
		RETURNING id
	`, dongleID, routeName).Scan(&routeID); err != nil {
		t.Fatalf("seed route: %v", err)
	}

	segCreatedAt := time.Now().Add(-segmentAge)
	if _, err := pool.Exec(ctx, `
		INSERT INTO segments (route_id, segment_number, created_at, qlog_uploaded)
		VALUES ($1, $2, $3, true)
	`, routeID, 0, segCreatedAt); err != nil {
		t.Fatalf("seed segment: %v", err)
	}

	return routeID
}

// newRouteMetadataWorkerForTest builds a worker with tight timings and an
// injected Extractor that returns the supplied RouteMetadata for every
// route. This avoids touching the real filesystem so the test can focus
// on the SQL update path. Storage is left nil because the Extractor short
// circuits the on-disk concatenation.
func newRouteMetadataWorkerForTest(pool *pgxpool.Pool, meta *cereal.RouteMetadata) *RouteMetadataWorker {
	w := NewRouteMetadataWorker(db.New(pool), nil)
	w.PollInterval = 10 * time.Millisecond
	w.FinalizedAfter = 100 * time.Millisecond
	w.BatchLimit = 50
	w.Extractor = func(_ context.Context, _ string, _ string) (*cereal.RouteMetadata, error) {
		// Return a copy so concurrent passes do not share mutable slice
		// headers in case the worker ever appends to Track in place.
		out := *meta
		out.Track = append([]cereal.GpsPoint(nil), meta.Track...)
		return &out, nil
	}
	return w
}

// fetchRouteRow re-reads the canonical columns we expect the worker to
// update so each assertion can talk about valid Postgres values directly.
type routeRow struct {
	startTime time.Time
	endTime   time.Time
	hasGeom   bool
	numPoints int32
}

func fetchRouteRow(t *testing.T, pool *pgxpool.Pool, routeID int32) routeRow {
	t.Helper()
	ctx := context.Background()
	var (
		row       routeRow
		startNull *time.Time
		endNull   *time.Time
		numPoints *int32
	)
	if err := pool.QueryRow(ctx, `
		SELECT start_time,
		       end_time,
		       geometry IS NOT NULL AS has_geom,
		       CASE WHEN geometry IS NOT NULL THEN ST_NumPoints(geometry) ELSE NULL END AS num_points
		FROM routes
		WHERE id = $1
	`, routeID).Scan(&startNull, &endNull, &row.hasGeom, &numPoints); err != nil {
		t.Fatalf("fetch route %d: %v", routeID, err)
	}
	if startNull != nil {
		row.startTime = startNull.UTC()
	}
	if endNull != nil {
		row.endTime = endNull.UTC()
	}
	if numPoints != nil {
		row.numPoints = *numPoints
	}
	return row
}

func TestRouteMetadataWorker_PopulatesGeometryAndTimes(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	dongle := "meta-gps-1"
	route := "2024-05-01--10-00-00"
	routeID := seedRouteForMetadata(t, pool, dongle, route, 1*time.Hour)

	start := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	meta := &cereal.RouteMetadata{
		StartTime: start,
		EndTime:   end,
		Track: []cereal.GpsPoint{
			{Lat: 37.700, Lng: -122.400, Time: start},
			{Lat: 37.701, Lng: -122.399, Time: start.Add(30 * time.Second)},
			{Lat: 37.702, Lng: -122.398, Time: start.Add(60 * time.Second)},
		},
	}

	w := newRouteMetadataWorkerForTest(pool, meta)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	row := fetchRouteRow(t, pool, routeID)
	if !row.startTime.Equal(start) {
		t.Errorf("start_time = %v, want %v", row.startTime, start)
	}
	if !row.endTime.Equal(end) {
		t.Errorf("end_time = %v, want %v", row.endTime, end)
	}
	if !row.hasGeom {
		t.Fatal("geometry is NULL after RunOnce, want a populated LineString")
	}
	if row.numPoints != 3 {
		t.Errorf("ST_NumPoints = %d, want 3", row.numPoints)
	}
}

func TestRouteMetadataWorker_NoGpsStillSetsTimestamps(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	dongle := "meta-nogps-1"
	route := "2024-05-02--10-00-00"
	routeID := seedRouteForMetadata(t, pool, dongle, route, 1*time.Hour)

	start := time.Date(2024, 5, 2, 10, 0, 0, 0, time.UTC)
	end := start.Add(45 * time.Second)
	meta := &cereal.RouteMetadata{StartTime: start, EndTime: end}

	w := newRouteMetadataWorkerForTest(pool, meta)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	row := fetchRouteRow(t, pool, routeID)
	if !row.startTime.Equal(start) {
		t.Errorf("start_time = %v, want %v", row.startTime, start)
	}
	if !row.endTime.Equal(end) {
		t.Errorf("end_time = %v, want %v", row.endTime, end)
	}
	if row.hasGeom {
		t.Errorf("geometry was set despite no GPS points: numPoints=%d", row.numPoints)
	}
}

// TestRouteMetadataWorker_RerunIsIdempotent confirms two important shapes
// at once:
//
//  1. Once start_time AND geometry are populated the route is no longer
//     returned by ListRoutesNeedingMetadata, so a second pass executes
//     no UPDATE and downstream consumers (notably trips.computed_at) do
//     not get re-stamped.
//  2. If the worker were re-listed for some reason, the UPDATE itself is
//     also a no-op on the column values because the extractor returns
//     deterministic data.
func TestRouteMetadataWorker_RerunIsIdempotent(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "meta-idem-1"
	route := "2024-05-03--10-00-00"
	routeID := seedRouteForMetadata(t, pool, dongle, route, 1*time.Hour)

	start := time.Date(2024, 5, 3, 10, 0, 0, 0, time.UTC)
	end := start.Add(60 * time.Second)
	meta := &cereal.RouteMetadata{
		StartTime: start,
		EndTime:   end,
		Track: []cereal.GpsPoint{
			{Lat: 37.700, Lng: -122.400},
			{Lat: 37.701, Lng: -122.399},
		},
	}
	w := newRouteMetadataWorkerForTest(pool, meta)

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	first := fetchRouteRow(t, pool, routeID)
	if !first.hasGeom {
		t.Fatal("first pass did not populate geometry")
	}

	// After the first pass the candidate query must not return this
	// route again. Verify directly so a regression in the WHERE clause
	// does not silently let the worker keep re-UPDATEing.
	cutoff := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	candidates, err := w.Queries.ListRoutesNeedingMetadata(ctx, db.ListRoutesNeedingMetadataParams{
		FinalizedBefore: cutoff,
		Limit:           50,
	})
	if err != nil {
		t.Fatalf("ListRoutesNeedingMetadata after first pass: %v", err)
	}
	for _, c := range candidates {
		if c.ID == routeID {
			t.Errorf("route %d still in candidates after metadata populated", routeID)
		}
	}

	// Second RunOnce should be a no-op for this route. The values must
	// still match the first pass's results -- a buggy UPDATE that
	// COALESCEd the wrong way around would either zero them out or
	// leave them unchanged, and we want to assert the latter.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	second := fetchRouteRow(t, pool, routeID)
	if !second.startTime.Equal(first.startTime) {
		t.Errorf("start_time changed across reruns: first=%v second=%v",
			first.startTime, second.startTime)
	}
	if !second.endTime.Equal(first.endTime) {
		t.Errorf("end_time changed across reruns: first=%v second=%v",
			first.endTime, second.endTime)
	}
	if second.numPoints != first.numPoints {
		t.Errorf("geometry numPoints changed across reruns: first=%d second=%d",
			first.numPoints, second.numPoints)
	}
}

// TestRouteMetadataWorker_UpdatePartialOnly ensures the COALESCE pattern in
// UpdateRouteMetadata leaves untouched columns alone. Seeds a route with a
// pre-existing start_time, then runs the worker with metadata that only
// supplies an end_time -- the original start_time must survive.
func TestRouteMetadataWorker_UpdatePartialOnly(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "meta-partial-1"
	route := "2024-05-04--10-00-00"
	routeID := seedRouteForMetadata(t, pool, dongle, route, 1*time.Hour)

	preStart := time.Date(2024, 5, 4, 9, 30, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE routes SET start_time = $1 WHERE id = $2`, preStart, routeID); err != nil {
		t.Fatalf("pre-set start_time: %v", err)
	}

	end := preStart.Add(90 * time.Second)
	meta := &cereal.RouteMetadata{
		// StartTime intentionally zero so the worker leaves it alone.
		EndTime: end,
	}
	w := newRouteMetadataWorkerForTest(pool, meta)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	row := fetchRouteRow(t, pool, routeID)
	if !row.startTime.Equal(preStart) {
		t.Errorf("start_time = %v, want preserved %v", row.startTime, preStart)
	}
	if !row.endTime.Equal(end) {
		t.Errorf("end_time = %v, want %v", row.endTime, end)
	}
}
