package worker

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"comma-personal-backend/internal/db"
)

// approx returns true when a and b agree within tol. Used so the
// floating-point bearing/wrap helpers don't have to land on exact
// binary values to pass.
func approx(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestWrapTo180(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		// Trivial in-range values pass through unchanged.
		{0, 0},
		{45, 45},
		{-45, -45},
		// The classic bearing-wrap edge case: 358 -> 2 should be a +4
		// crossing of north, NOT a -356 deg sweep. The acceptance
		// criteria call this out explicitly.
		{358 - 2, -4}, // sanity: subtraction without wrap
		{2 - 358, 4},  // <-- the failure mode
		{4, 4},        // already wrapped form
		// 180 / -180 are the same physical U-turn. Either sign is
		// acceptable because the detector thresholds on |delta|;
		// document the actual convention here. Our formula maps both
		// to -180 (the [-180, 180) half-open form).
		{180, -180},
		{-180, -180},
		// Multi-revolution wraps converge.
		{360, 0},
		{720, 0},
		{-360, 0},
		// 270 deg right turn is equivalent to a 90 deg left turn.
		{270, -90},
		{-270, 90},
	}
	for _, c := range cases {
		got := wrapTo180(c.in)
		if !approx(got, c.want, 1e-9) {
			t.Errorf("wrapTo180(%g) = %g, want %g", c.in, got, c.want)
		}
	}
}

func TestBearingDeg_CardinalDirections(t *testing.T) {
	// Reference point: 37 N, 122 W. Move ~1km in each cardinal
	// direction and check the resulting bearing.
	const lat0, lng0 = 37.0, -122.0
	const dLat = 0.009 // ~1km north
	const dLng = 0.011 // ~1km east at 37 N (longitudes are tighter near the poles)

	cases := []struct {
		name       string
		lat2, lng2 float64
		wantDeg    float64
	}{
		{"north", lat0 + dLat, lng0, 0},
		{"east", lat0, lng0 + dLng, 90},
		{"south", lat0 - dLat, lng0, 180},
		{"west", lat0, lng0 - dLng, 270},
	}
	for _, c := range cases {
		got := bearingDeg(lat0, lng0, c.lat2, c.lng2)
		// 1 degree tolerance: the spherical formula at 37 N with these
		// step sizes lands within a fraction of a degree of the
		// cardinal target. A larger tolerance would mask copy-paste
		// bugs (lat/lng swap, sign flip in sin/cos, ...) which is
		// exactly what this test guards against.
		if !approx(got, c.wantDeg, 1.0) {
			t.Errorf("bearing %s = %g, want ~%g", c.name, got, c.wantDeg)
		}
	}
}

// makeStraightLine produces a vertex+time pair at constant heading,
// used to verify the detector emits 0 turns on a straight line.
func makeStraightLine(n int, dtMs int64) ([]LatLng, []int64) {
	verts := make([]LatLng, n)
	times := make([]int64, n)
	for i := 0; i < n; i++ {
		verts[i] = LatLng{Lat: 37.0 + float64(i)*0.0001, Lng: -122.0}
		times[i] = int64(i) * dtMs
	}
	return verts, times
}

// makeRightAngleTurn returns a synthetic geometry that goes east for
// `secondsPerLeg` seconds at 1 Hz, then turns 90 degrees and goes
// south for another `secondsPerLeg` seconds. The corner vertex sits at
// index `secondsPerLeg-1`; the next vertex begins the south leg.
//
// We deliberately do NOT duplicate the corner point -- two coincident
// vertices produce an undefined-bearing edge that would defeat the
// windowed average and hide the very transition we are trying to
// detect.
func makeRightAngleTurn(secondsPerLeg int) ([]LatLng, []int64) {
	const lat0 = 37.0
	const lng0 = -122.0
	const dLng = 0.0001 // ~10m at 37 N
	const dLat = 0.0001

	verts := make([]LatLng, 0, 2*secondsPerLeg)
	times := make([]int64, 0, 2*secondsPerLeg)
	t := int64(0)
	dt := int64(1000) // 1 Hz
	// East leg: lat constant, lng increases each step.
	for i := 0; i < secondsPerLeg; i++ {
		verts = append(verts, LatLng{Lat: lat0, Lng: lng0 + float64(i)*dLng})
		times = append(times, t)
		t += dt
	}
	// South leg starts one dLat south of the corner. The last east
	// vertex IS the corner; the first south vertex is corner + dLat
	// south. Bearings: (last east -> first south) is heading 180.
	cornerLng := lng0 + float64(secondsPerLeg-1)*dLng
	for i := 1; i <= secondsPerLeg; i++ {
		verts = append(verts, LatLng{Lat: lat0 - float64(i)*dLat, Lng: cornerLng})
		times = append(times, t)
		t += dt
	}
	return verts, times
}

func TestDetectTurns_StraightLine_ZeroTurns(t *testing.T) {
	verts, times := makeStraightLine(60, 1000) // 60s, 1 Hz, due north
	turns := DetectTurns(verts, times, TurnConfig{
		WindowSeconds: 4,
		DeltaDegMin:   35,
		DedupSeconds:  5,
	})
	if len(turns) != 0 {
		t.Errorf("straight line: got %d turns (%v), want 0", len(turns), turns)
	}
}

func TestDetectTurns_RightAngle_ExactlyOne(t *testing.T) {
	// 30 s east, then 30 s south. The corner sits at the boundary
	// between v[29] (last east) and v[30] (first south). The
	// algorithm fires at the FIRST interior vertex whose windowed
	// |delta| crosses the threshold, then dedups subsequent
	// candidates -- so we get exactly ONE turn.
	//
	// Because the after-window centred at v[28] still pulls in two
	// edges (one east, one south), the windowed-mean bearing there is
	// ~135 deg, yielding a delta of ~45. That is enough to cross the
	// 35 deg threshold, so the emission lands near the corner with a
	// delta in the [35, 90] range. The point of the test is that:
	//
	//   1. One (and only one) turn fires for one right-angle turn.
	//   2. The fired turn sits near the corner (within ~3 s).
	//   3. The delta is unambiguously a turn (>= the 35 deg threshold).
	verts, times := makeRightAngleTurn(30)
	turns := DetectTurns(verts, times, TurnConfig{
		WindowSeconds: 4,
		DeltaDegMin:   35,
		DedupSeconds:  5,
	})
	if len(turns) != 1 {
		t.Fatalf("right-angle turn: got %d turns, want 1; turns=%+v", len(turns), turns)
	}
	got := turns[0]
	if got.OffsetMs < 26_000 || got.OffsetMs > 32_000 {
		t.Errorf("turn offset_ms = %d, want roughly 28000-30000", got.OffsetMs)
	}
	// The detected delta must be at least the threshold, with the
	// expected sign for a clockwise right turn (positive in our
	// convention: bearing increased from 90 toward 180).
	if got.DeltaDeg < 35 || got.DeltaDeg > 95 {
		t.Errorf("turn delta_deg = %g, want roughly +35 to +90 (right turn east->south)", got.DeltaDeg)
	}
	// And the per-turn endpoints must agree with the right-turn
	// direction: bearing_before near 90, bearing_after between
	// 90 and 180.
	if got.BearingBefore < 80 || got.BearingBefore > 100 {
		t.Errorf("bearing_before = %g, want ~90", got.BearingBefore)
	}
	if got.BearingAfter < 90 || got.BearingAfter > 180 {
		t.Errorf("bearing_after = %g, want between 90 and 180", got.BearingAfter)
	}
}

// makeCircle produces a closed roughly-circular trajectory of n samples
// at 1 Hz around (lat0, lng0). One full revolution = 360 deg of
// heading change spread across n seconds. With n = 30 (12 deg/s), a
// 4 s window sees ~48 deg of swing -- enough to comfortably trip the
// 35 deg default threshold, which is the case we want to stress.
func makeCircle(n int) ([]LatLng, []int64) {
	const lat0 = 37.0
	const lng0 = -122.0
	const r = 0.001
	verts := make([]LatLng, n)
	times := make([]int64, n)
	for i := 0; i < n; i++ {
		theta := 2 * math.Pi * float64(i) / float64(n)
		verts[i] = LatLng{Lat: lat0 + r*math.Sin(theta), Lng: lng0 + r*math.Cos(theta)}
		times[i] = int64(i) * 1000
	}
	return verts, times
}

func TestDetectTurns_Circle_DedupBounded(t *testing.T) {
	// A tight 20-vertex circle at 1 Hz (one lap in 20 s = 18 deg/s of
	// heading change) crosses the 35 deg threshold inside the 4 s
	// detector window: each per-vertex windowed delta lands around
	// ~36 deg. Without dedup the detector would fire at every
	// interior vertex; with the 5 s dedup it is bounded to roughly
	// ceil(20 / 5) = 4 turns per lap. The test asserts both bounds
	// (>= 1 and <= dedup limit + 1) and that consecutive emissions
	// honour the dedup gap -- the invariant that protects production
	// from a roundabout firing 12 times.
	verts, times := makeCircle(20)
	turns := DetectTurns(verts, times, TurnConfig{
		WindowSeconds: 4,
		DeltaDegMin:   35,
		DedupSeconds:  5,
	})
	maxAllowed := 20 / 5 // dedup of 5 s caps at ~one turn per 5 s
	if len(turns) < 1 {
		t.Errorf("circle: got %d turns, expected at least 1 (dedup-bound count)", len(turns))
	}
	if len(turns) > maxAllowed+1 {
		t.Errorf("circle: got %d turns, dedup-bound is ~%d", len(turns), maxAllowed)
	}
	for i := 1; i < len(turns); i++ {
		gap := turns[i].OffsetMs - turns[i-1].OffsetMs
		if gap < int64(5000) {
			t.Errorf("circle: turn[%d] gap = %d ms, dedup expected >= 5000", i, gap)
		}
	}
}

func TestDetectTurns_BearingWrapEdgeCase_NoSpuriousTurn(t *testing.T) {
	// Construct a trajectory whose heading wobbles across due-north:
	// vertices alternate between heading ~358 deg and heading ~2 deg
	// (a 4 deg wobble). The mathematical truth is "going north"; a
	// naive non-wrapped delta would compute -356 deg and fire a
	// (wildly wrong) turn at every vertex.
	//
	// We build this by stepping mostly north with a tiny lateral
	// jitter. Each pair of segments crosses the meridian: vertex i is
	// slightly east of i-1, vertex i+1 is slightly west of i, etc.
	const lat0 = 37.0
	const lng0 = -122.0
	const dLat = 0.0002     // ~22 m per step
	const jitter = 0.000007 // ~0.7 m E/W -- ~2 deg off due-north
	n := 50
	verts := make([]LatLng, n)
	times := make([]int64, n)
	for i := 0; i < n; i++ {
		eastWest := jitter
		if i%2 == 1 {
			eastWest = -jitter
		}
		verts[i] = LatLng{Lat: lat0 + float64(i)*dLat, Lng: lng0 + eastWest}
		times[i] = int64(i) * 1000
	}
	turns := DetectTurns(verts, times, TurnConfig{
		WindowSeconds: 4,
		DeltaDegMin:   35,
		DedupSeconds:  5,
	})
	if len(turns) != 0 {
		t.Errorf("bearing-wrap wobble around north: got %d turns (%+v), want 0", len(turns), turns)
	}
}

func TestDetectTurns_TooFewVertices_NoTurns(t *testing.T) {
	// 5 vertices is below the 30-vertex floor at the worker level,
	// but DetectTurns itself has a softer minimum (3) so the full
	// length-vs-min-route guard is in the worker. Here we just
	// confirm that DetectTurns gracefully returns nothing for an
	// underflow input rather than panicking.
	verts := []LatLng{
		{Lat: 37.0, Lng: -122.0},
		{Lat: 37.0001, Lng: -122.0},
	}
	times := []int64{0, 1000}
	turns := DetectTurns(verts, times, TurnConfig{
		WindowSeconds: 4,
		DeltaDegMin:   35,
		DedupSeconds:  5,
	})
	if len(turns) != 0 {
		t.Errorf("under-min vertices: got %d turns, want 0", len(turns))
	}
}

func TestDetectTurns_MismatchedArrays_NoTurns(t *testing.T) {
	// Defensive: if a caller hands us a mis-paired (vertices, times)
	// the function returns nil rather than producing nonsense
	// timestamps. The worker also catches this before calling, but we
	// belt-and-suspenders it here.
	verts := []LatLng{
		{Lat: 37.0, Lng: -122.0},
		{Lat: 37.001, Lng: -122.0},
		{Lat: 37.002, Lng: -122.0},
	}
	times := []int64{0, 1000} // length mismatch
	turns := DetectTurns(verts, times, TurnConfig{
		WindowSeconds: 4,
		DeltaDegMin:   35,
		DedupSeconds:  5,
	})
	if len(turns) != 0 {
		t.Errorf("mismatched array lengths: got %d turns, want 0", len(turns))
	}
}

func TestParseLineStringWKT_RoundTrip(t *testing.T) {
	wkt := "LINESTRING(-122 37, -122.001 37, -122.001 37.001)"
	verts, err := parseLineStringWKT(wkt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(verts) != 3 {
		t.Fatalf("got %d verts, want 3", len(verts))
	}
	// WKT uses (lng lat); our LatLng holds (lat, lng). The swap
	// happens in the parser; we validate both axes.
	if !approx(verts[0].Lat, 37, 1e-9) || !approx(verts[0].Lng, -122, 1e-9) {
		t.Errorf("verts[0] = %+v, want {37, -122}", verts[0])
	}
	if !approx(verts[2].Lat, 37.001, 1e-9) || !approx(verts[2].Lng, -122.001, 1e-9) {
		t.Errorf("verts[2] = %+v, want {37.001, -122.001}", verts[2])
	}
}

func TestParseLineStringWKT_BadInput(t *testing.T) {
	cases := []string{
		"",                       // empty
		"POINT(1 2)",             // wrong type
		"LINESTRING(1 2, 3)",     // odd field count
		"LINESTRING(abc 2, 3 4)", // non-numeric
		"LINESTRING(1 2, 3 4",    // missing close paren
	}
	for _, c := range cases {
		if _, err := parseLineStringWKT(c); err == nil {
			t.Errorf("parseLineStringWKT(%q) = nil err, want error", c)
		}
	}
}

// TestTurnDetector_PersistAndIdempotent exercises the persist path
// against a real database (postgres + postgis). It asserts that:
//
//  1. A first pass over a finalized route produces at least one row in
//     route_turns.
//  2. A second pass over the same route is a no-op at the row-count
//     level -- the delete-then-insert is idempotent.
//  3. The turns query (ListTurnsForRoute) returns the same rows the
//     worker just wrote (round-trip).
//
// Skipped when DATABASE_URL is unset (local dev without postgres).
func TestTurnDetector_PersistAndIdempotent(t *testing.T) {
	pool, cleanup, skip := setupTestDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	ctx := context.Background()
	dongle := "turn-detector-1"
	route := "2024-05-01--10-00-00"

	// Build a 60-vertex right-angle WKT so we have plenty of vertices
	// and a clean turn. Vertex 0 = (37, -122), east leg 30 vertices
	// at lat=37, lng increasing; south leg 30 vertices at lng=corner,
	// lat decreasing.
	verts, times := makeRightAngleTurn(30)
	wkt := buildWKT(verts)
	timesArr := make([]int64, len(times))
	copy(timesArr, times)

	startTime := time.Now().Add(-10 * time.Minute).UTC()
	endTime := startTime.Add(time.Duration(times[len(times)-1]) * time.Millisecond).UTC()

	// Seed device + route + segment. We do this via raw SQL because
	// the worker's prerequisites (geometry + geometry_times +
	// start_time + finalized segment) are exactly what the route
	// metadata worker would have written; we shortcut that here.
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices (dongle_id, serial, public_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (dongle_id) DO NOTHING
	`, dongle, "serial-"+dongle, "pk-"+dongle); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	var routeID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO routes (dongle_id, route_name, start_time, end_time, geometry, geometry_times)
		VALUES ($1, $2, $3, $4, ST_GeomFromText($5, 4326), $6)
		RETURNING id
	`, dongle, route, startTime, endTime, wkt, timesArr).Scan(&routeID); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	// A segment older than the worker's FinalizedAfter window so the
	// route looks "done uploading".
	if _, err := pool.Exec(ctx, `
		INSERT INTO segments (route_id, segment_number, created_at)
		VALUES ($1, 0, $2)
	`, routeID, time.Now().Add(-1*time.Hour)); err != nil {
		t.Fatalf("seed segment: %v", err)
	}

	queries := db.New(pool)
	w := NewTurnDetectorWorker(queries, pool, nil, nil)
	w.PollInterval = 10 * time.Millisecond
	w.FinalizedAfter = 100 * time.Millisecond
	w.BackfillLimit = 0 // disable the one-shot backfill in this test

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	count1, err := queries.CountTurnsForRoute(ctx, db.CountTurnsForRouteParams{
		DongleID: dongle,
		Route:    route,
	})
	if err != nil {
		t.Fatalf("CountTurnsForRoute pass 1: %v", err)
	}
	if count1 < 1 {
		t.Fatalf("pass 1 wrote %d turns, want >= 1", count1)
	}

	rows1, err := queries.ListTurnsForRoute(ctx, db.ListTurnsForRouteParams{
		DongleID: dongle,
		Route:    route,
	})
	if err != nil {
		t.Fatalf("ListTurnsForRoute pass 1: %v", err)
	}
	if int64(len(rows1)) != count1 {
		t.Fatalf("list pass 1 length=%d, count=%d", len(rows1), count1)
	}

	// Pass 2: the worker's filter requires "no existing turns" so it
	// will not pick this route up via ListRoutesForTurnDetection.
	// Drive processRoute directly to verify the delete-then-insert
	// idempotency invariant.
	if err := w.processRoute(ctx, dongle, route); err != nil {
		t.Fatalf("processRoute pass 2: %v", err)
	}
	count2, err := queries.CountTurnsForRoute(ctx, db.CountTurnsForRouteParams{
		DongleID: dongle,
		Route:    route,
	})
	if err != nil {
		t.Fatalf("CountTurnsForRoute pass 2: %v", err)
	}
	if count2 != count1 {
		t.Errorf("pass 2 turn count = %d, want %d (idempotent re-run)", count2, count1)
	}
}

// buildWKT renders a slice of LatLng as a "LINESTRING(lng lat, ...)" so
// the integration test can write geometry that round-trips through
// PostGIS. The (lng lat) ordering matches the WKT spec; the worker's
// parseLineStringWKT swaps back into (lat, lng) on read.
func buildWKT(verts []LatLng) string {
	parts := make([]string, len(verts))
	for i, v := range verts {
		parts[i] = fmt.Sprintf("%g %g", v.Lng, v.Lat)
	}
	return "LINESTRING(" + strings.Join(parts, ", ") + ")"
}

func TestTurnConfig_Normalized_ClampsBadValues(t *testing.T) {
	zero := TurnConfig{}.normalized()
	if zero.WindowSeconds != defaultTurnWindowSeconds {
		t.Errorf("zero window not clamped: %g", zero.WindowSeconds)
	}
	if zero.DeltaDegMin != defaultTurnDeltaDegMin {
		t.Errorf("zero delta not clamped: %g", zero.DeltaDegMin)
	}
	// Dedup of zero is allowed (means "no suppression"), only negative
	// gets coerced.
	negative := TurnConfig{DedupSeconds: -1}.normalized()
	if negative.DedupSeconds != defaultTurnDedupSeconds {
		t.Errorf("negative dedup not clamped: %g", negative.DedupSeconds)
	}
}
