package db

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setupAnnotationsDB mirrors the helper in internal/worker: it connects to
// DATABASE_URL, wipes the schema, and replays every *.up.sql migration
// under sql/migrations. Returns (nil, nil, true) when the environment is
// not configured for integration tests so the caller can t.Skip cleanly.
func setupAnnotationsDB(t *testing.T) (*pgxpool.Pool, func(), bool) {
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

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		t.Skipf("DATABASE_URL points at an unreachable database: %v", err)
	}

	resetAnnotationsSchema(ctx, t, pool)
	applyAnnotationsMigrations(ctx, t, pool)

	cleanup := func() {
		resetAnnotationsSchema(context.Background(), t, pool)
		pool.Close()
	}
	return pool, cleanup, false
}

func resetAnnotationsSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		`DROP TABLE IF EXISTS crashes CASCADE`,
		`DROP TABLE IF EXISTS route_data_requests CASCADE`,
		`DROP TABLE IF EXISTS events CASCADE`,
		`DROP TABLE IF EXISTS trips CASCADE`,
		`DROP TABLE IF EXISTS segments CASCADE`,
		`DROP TABLE IF EXISTS route_tags CASCADE`,
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

func applyAnnotationsMigrations(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	migDir := findAnnotationsMigrationsDir(t)
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

func findAnnotationsMigrationsDir(t *testing.T) string {
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

// seedDeviceAndRoute inserts a device and one route with empty geometry and
// returns the route id so the test can poke at the annotation columns.
func seedDeviceAndRoute(t *testing.T, pool *pgxpool.Pool, dongleID, routeName string) int32 {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO devices (dongle_id, serial, public_key)
		VALUES ($1, $2, $3)
		ON CONFLICT (dongle_id) DO NOTHING
	`, dongleID, "serial-"+dongleID, "pk-"+dongleID); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	now := time.Now().UTC()
	var routeID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO routes (dongle_id, route_name, start_time, end_time)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, dongleID, routeName, now.Add(-10*time.Minute), now).Scan(&routeID); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	return routeID
}

func TestRouteAnnotations_DefaultsAndUpdates(t *testing.T) {
	pool, cleanup, skip := setupAnnotationsDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	q := New(pool)
	ctx := context.Background()

	routeID := seedDeviceAndRoute(t, pool, "d1", "2024-01-01--00-00-00")

	// Defaults: note is empty, starred is false.
	got, err := q.GetRouteByID(ctx, routeID)
	if err != nil {
		t.Fatalf("GetRouteByID: %v", err)
	}
	if got.Note != "" {
		t.Errorf("expected default note to be empty, got %q", got.Note)
	}
	if got.Starred {
		t.Errorf("expected default starred to be false")
	}

	// Set a note.
	if _, err := q.SetRouteNote(ctx, SetRouteNoteParams{ID: routeID, Note: "great drive"}); err != nil {
		t.Fatalf("SetRouteNote: %v", err)
	}

	// Star it.
	if _, err := q.SetRouteStarred(ctx, SetRouteStarredParams{ID: routeID, Starred: true}); err != nil {
		t.Fatalf("SetRouteStarred: %v", err)
	}

	got, err = q.GetRouteByID(ctx, routeID)
	if err != nil {
		t.Fatalf("GetRouteByID post-update: %v", err)
	}
	if got.Note != "great drive" {
		t.Errorf("note = %q, want %q", got.Note, "great drive")
	}
	if !got.Starred {
		t.Errorf("expected starred to be true")
	}
}

func TestRouteTags_AddRemoveList(t *testing.T) {
	pool, cleanup, skip := setupAnnotationsDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	q := New(pool)
	ctx := context.Background()

	r1 := seedDeviceAndRoute(t, pool, "d1", "2024-01-01--00-00-00")
	r2 := seedDeviceAndRoute(t, pool, "d1", "2024-01-02--00-00-00")
	rOther := seedDeviceAndRoute(t, pool, "d2", "2024-01-01--00-00-00")

	mustAdd := func(routeID int32, tag string) {
		if err := q.AddRouteTag(ctx, AddRouteTagParams{RouteID: routeID, Tag: tag}); err != nil {
			t.Fatalf("AddRouteTag(%d, %q): %v", routeID, tag, err)
		}
	}

	mustAdd(r1, "highway")
	mustAdd(r1, "commute")
	mustAdd(r1, "highway") // duplicate, should be a no-op
	mustAdd(r2, "scenic")
	mustAdd(rOther, "other-device-only")

	got, err := q.ListTagsForRoute(ctx, r1)
	if err != nil {
		t.Fatalf("ListTagsForRoute: %v", err)
	}
	if want := []string{"commute", "highway"}; !stringSliceEqual(got, want) {
		t.Errorf("ListTagsForRoute(r1) = %v, want %v", got, want)
	}

	devTags, err := q.ListTagsForDevice(ctx, "d1")
	if err != nil {
		t.Fatalf("ListTagsForDevice: %v", err)
	}
	if want := []string{"commute", "highway", "scenic"}; !stringSliceEqual(devTags, want) {
		t.Errorf("ListTagsForDevice(d1) = %v, want %v", devTags, want)
	}

	// Remove one.
	if err := q.RemoveRouteTag(ctx, RemoveRouteTagParams{RouteID: r1, Tag: "commute"}); err != nil {
		t.Fatalf("RemoveRouteTag: %v", err)
	}
	got, err = q.ListTagsForRoute(ctx, r1)
	if err != nil {
		t.Fatalf("ListTagsForRoute after remove: %v", err)
	}
	if want := []string{"highway"}; !stringSliceEqual(got, want) {
		t.Errorf("ListTagsForRoute(r1) after remove = %v, want %v", got, want)
	}
}

func TestRouteTags_CheckConstraintRejectsBadTags(t *testing.T) {
	pool, cleanup, skip := setupAnnotationsDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	q := New(pool)
	ctx := context.Background()

	routeID := seedDeviceAndRoute(t, pool, "d1", "2024-01-01--00-00-00")

	bad := []string{
		"",                      // too short
		" highway",              // untrimmed
		"Highway",               // non-lowercase
		strings.Repeat("a", 33), // too long
	}
	for _, b := range bad {
		err := q.AddRouteTag(ctx, AddRouteTagParams{RouteID: routeID, Tag: b})
		if err == nil {
			t.Errorf("AddRouteTag(%q) succeeded; expected CHECK failure", b)
		}
	}
}

func TestReplaceRouteTags_TransactionalSwap(t *testing.T) {
	pool, cleanup, skip := setupAnnotationsDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	q := New(pool)
	ctx := context.Background()

	routeID := seedDeviceAndRoute(t, pool, "d1", "2024-01-01--00-00-00")

	for _, tag := range []string{"alpha", "beta"} {
		if err := q.AddRouteTag(ctx, AddRouteTagParams{RouteID: routeID, Tag: tag}); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}

	// Replace with a disjoint set plus a duplicate to exercise the no-op
	// ON CONFLICT path.
	if err := q.ReplaceRouteTags(ctx, routeID, []string{"gamma", "delta", "gamma"}); err != nil {
		t.Fatalf("ReplaceRouteTags: %v", err)
	}

	got, err := q.ListTagsForRoute(ctx, routeID)
	if err != nil {
		t.Fatalf("ListTagsForRoute: %v", err)
	}
	if want := []string{"delta", "gamma"}; !stringSliceEqual(got, want) {
		t.Errorf("after ReplaceRouteTags = %v, want %v", got, want)
	}

	// Replacing with an empty slice clears the set.
	if err := q.ReplaceRouteTags(ctx, routeID, nil); err != nil {
		t.Fatalf("ReplaceRouteTags(nil): %v", err)
	}
	got, err = q.ListTagsForRoute(ctx, routeID)
	if err != nil {
		t.Fatalf("ListTagsForRoute after clear: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after clear, expected no tags, got %v", got)
	}
}

func TestReplaceRouteTags_RollsBackOnBadTag(t *testing.T) {
	pool, cleanup, skip := setupAnnotationsDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	q := New(pool)
	ctx := context.Background()

	routeID := seedDeviceAndRoute(t, pool, "d1", "2024-01-01--00-00-00")
	for _, tag := range []string{"keep-me"} {
		if err := q.AddRouteTag(ctx, AddRouteTagParams{RouteID: routeID, Tag: tag}); err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}

	// "Bad-Tag" violates the CHECK constraint. The whole transaction must
	// roll back, leaving the original tag intact.
	if err := q.ReplaceRouteTags(ctx, routeID, []string{"ok", "Bad-Tag"}); err == nil {
		t.Fatalf("expected ReplaceRouteTags to fail on bad tag")
	}

	got, err := q.ListTagsForRoute(ctx, routeID)
	if err != nil {
		t.Fatalf("ListTagsForRoute: %v", err)
	}
	if want := []string{"keep-me"}; !stringSliceEqual(got, want) {
		t.Errorf("after failed replace, tags = %v, want %v (rollback expected)", got, want)
	}
}

func TestListRoutesByDeviceWithCounts_IncludesAnnotations(t *testing.T) {
	pool, cleanup, skip := setupAnnotationsDB(t)
	if skip {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	defer cleanup()

	q := New(pool)
	ctx := context.Background()

	r1 := seedDeviceAndRoute(t, pool, "d1", "2024-01-01--00-00-00")
	r2 := seedDeviceAndRoute(t, pool, "d1", "2024-01-02--00-00-00")

	if _, err := q.SetRouteNote(ctx, SetRouteNoteParams{ID: r1, Note: "first"}); err != nil {
		t.Fatalf("SetRouteNote: %v", err)
	}
	if _, err := q.SetRouteStarred(ctx, SetRouteStarredParams{ID: r2, Starred: true}); err != nil {
		t.Fatalf("SetRouteStarred: %v", err)
	}
	if err := q.AddRouteTag(ctx, AddRouteTagParams{RouteID: r1, Tag: "alpha"}); err != nil {
		t.Fatalf("AddRouteTag: %v", err)
	}
	if err := q.AddRouteTag(ctx, AddRouteTagParams{RouteID: r1, Tag: "beta"}); err != nil {
		t.Fatalf("AddRouteTag: %v", err)
	}

	rows, err := q.ListRoutesByDeviceWithCounts(ctx, ListRoutesByDevicePaginatedParams{
		DongleID: "d1",
		Limit:    10,
		Offset:   0,
	})
	if err != nil {
		t.Fatalf("ListRoutesByDeviceWithCounts: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	byID := map[int32]RouteWithSegmentCount{rows[0].ID: rows[0], rows[1].ID: rows[1]}

	if got := byID[r1]; got.Note != "first" || got.Starred {
		t.Errorf("r1: note=%q starred=%v, want note=%q starred=false", got.Note, got.Starred, "first")
	}
	if got := byID[r1]; !stringSliceEqual(got.Tags, []string{"alpha", "beta"}) {
		t.Errorf("r1 tags = %v, want [alpha beta]", got.Tags)
	}
	if got := byID[r2]; got.Note != "" || !got.Starred {
		t.Errorf("r2: note=%q starred=%v, want note=\"\" starred=true", got.Note, got.Starred)
	}
	if got := byID[r2]; len(got.Tags) != 0 {
		t.Errorf("r2 tags = %v, want empty", got.Tags)
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
