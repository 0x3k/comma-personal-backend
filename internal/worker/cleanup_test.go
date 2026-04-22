package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
)

// cleanupTestEnv holds everything a single cleanup worker test needs: a
// dedicated schema inside the shared Postgres instance, a tmp STORAGE_PATH,
// and the worker under test. Each test gets its own schema so the suite is
// safe to run in parallel and a failure in one test cannot contaminate
// another.
type cleanupTestEnv struct {
	t           *testing.T
	pool        *pgxpool.Pool
	queries     *db.Queries
	store       *storage.Storage
	settings    *settings.Store
	storagePath string
	schemaName  string
}

// newCleanupTestEnv boots Postgres, applies the migrations into a fresh
// per-test schema, and returns a ready-to-use environment. It calls t.Skip
// when TEST_DATABASE_URL is not set so the suite stays runnable in
// environments that do not have Postgres available (CI without a DB,
// golangci-lint containers, etc.).
func newCleanupTestEnv(t *testing.T) *cleanupTestEnv {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping cleanup worker integration tests. " +
			"Set to a Postgres + PostGIS DSN (e.g. postgres://comma:comma@localhost:5432/comma?sslmode=disable) to run.")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}

	// Each test gets a unique schema so they are independent even under
	// -parallel. The name encodes time + the sanitized test name.
	schemaName := fmt.Sprintf("cleanup_test_%d_%s",
		time.Now().UnixNano(), sanitizeForSchema(t.Name()))

	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schemaName)); err != nil {
		pool.Close()
		t.Fatalf("failed to create schema %s: %v", schemaName, err)
	}

	// Point the connection search_path at the new schema. PostGIS types and
	// functions live in the public schema, so keep it on the search_path
	// too; otherwise GEOMETRY(LineString, 4326) fails to parse.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`SET search_path TO %s, public`, schemaName)); err != nil {
		dropSchema(ctx, pool, schemaName)
		pool.Close()
		t.Fatalf("failed to set search_path: %v", err)
	}

	if err := applyMigrations(ctx, pool, schemaName); err != nil {
		dropSchema(ctx, pool, schemaName)
		pool.Close()
		t.Fatalf("failed to apply migrations: %v", err)
	}

	queries := db.New(pool)
	storagePath := t.TempDir()
	store := storage.New(storagePath)
	settingsStore := settings.New(queries)

	env := &cleanupTestEnv{
		t:           t,
		pool:        pool,
		queries:     queries,
		store:       store,
		settings:    settingsStore,
		storagePath: storagePath,
		schemaName:  schemaName,
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dropSchema(cleanupCtx, pool, schemaName)
		pool.Close()
	})

	return env
}

// sanitizeForSchema strips characters that are invalid in an unquoted
// Postgres identifier so t.Name() can be embedded directly.
func sanitizeForSchema(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// applyMigrations runs every sql/migrations/NNN_*.up.sql file, in numeric
// order, inside the given schema. It locates the migrations directory by
// walking up from the current working directory so tests work regardless of
// whether `go test` is invoked from the repo root or the package dir.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool, schemaName string) error {
	migrationsDir, err := findMigrationsDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var upFiles []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		upFiles = append(upFiles, name)
	}
	sort.Strings(upFiles)

	for _, name := range upFiles {
		raw, err := os.ReadFile(filepath.Join(migrationsDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		sql := string(raw)
		// 001_init.up.sql runs CREATE EXTENSION postgis globally; skip that
		// in per-schema tests because the extension is usually already
		// installed in the public schema.
		sql = strings.ReplaceAll(sql,
			"CREATE EXTENSION IF NOT EXISTS postgis;",
			"-- CREATE EXTENSION IF NOT EXISTS postgis; (skipped in tests)")

		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("apply %s (schema=%s): %w", name, schemaName, err)
		}
	}
	return nil
}

// findMigrationsDir walks up from the current working directory looking for
// sql/migrations. Works both when tests run from the package directory and
// when they run from the repo root.
func findMigrationsDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "sql", "migrations")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("sql/migrations not found walking up from %s", cwd)
}

// dropSchema best-effort drops the per-test schema so a failing test does
// not leak state into the shared database.
func dropSchema(ctx context.Context, pool *pgxpool.Pool, schemaName string) {
	_, _ = pool.Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, schemaName))
}

// seedRoute inserts a device + route + one segment and writes a file under
// the route directory so bytes_freed is non-zero. Returns the route row
// sqlc gives back.
func (e *cleanupTestEnv) seedRoute(ctx context.Context, dongleID, routeName string, endTime time.Time, preserved bool) db.Route {
	e.t.Helper()

	_, err := e.pool.Exec(ctx, `
		INSERT INTO devices (dongle_id) VALUES ($1)
		ON CONFLICT (dongle_id) DO NOTHING
	`, dongleID)
	if err != nil {
		e.t.Fatalf("insert device: %v", err)
	}

	route, err := e.queries.CreateRoute(ctx, db.CreateRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
		StartTime: pgtype.Timestamptz{Time: endTime.Add(-time.Hour), Valid: true},
		EndTime:   pgtype.Timestamptz{Time: endTime, Valid: true},
	})
	if err != nil {
		e.t.Fatalf("create route: %v", err)
	}

	if preserved {
		route, err = e.queries.SetRoutePreserved(ctx, db.SetRoutePreservedParams{
			DongleID:  dongleID,
			RouteName: routeName,
			Preserved: true,
		})
		if err != nil {
			e.t.Fatalf("set preserved: %v", err)
		}
	}

	// Drop a fake camera file under the route so bytes_freed reports > 0.
	if err := e.store.Store(dongleID, routeName, "0", "fcamera.hevc", strings.NewReader("fake hevc bytes")); err != nil {
		e.t.Fatalf("seed file: %v", err)
	}
	return route
}

// routeExists reports whether a (dongle, route) pair still has a row in the
// routes table. Used by tests to assert delete/dry-run behaviour.
func (e *cleanupTestEnv) routeExists(ctx context.Context, dongleID, routeName string) bool {
	e.t.Helper()
	_, err := e.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err == nil {
		return true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	e.t.Fatalf("GetRoute: %v", err)
	return false
}

// newWorker returns a CleanupWorker pre-wired to this env and frozen at the
// given time for deterministic cutoff computation.
func (e *cleanupTestEnv) newWorker(now time.Time, retentionDays int, dryRun bool) *CleanupWorker {
	return &CleanupWorker{
		Queries:            e.queries,
		Storage:            e.store,
		Settings:           e.settings,
		MaxDeletionsPerRun: 100,
		DryRun:             dryRun,
		EnvRetentionDays:   retentionDays,
		now:                func() time.Time { return now },
	}
}

func TestCleanupWorker_DeletesStaleRoute(t *testing.T) {
	env := newCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	staleEnd := now.Add(-10 * 24 * time.Hour) // 10 days old
	freshEnd := now.Add(-1 * 24 * time.Hour)  // 1 day old

	stale := env.seedRoute(ctx, "dongle_stale", "2025-01-05--02-00-00", staleEnd, false)
	fresh := env.seedRoute(ctx, "dongle_fresh", "2025-01-14--12-00-00", freshEnd, false)

	// With retention_days=7 and now=Jan 15, cutoff is Jan 8: stale (Jan 5)
	// should be deleted, fresh (Jan 14) should survive.
	w := env.newWorker(now, 7, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if env.routeExists(ctx, stale.DongleID, stale.RouteName) {
		t.Error("expected stale route to be deleted from DB")
	}
	if !env.routeExists(ctx, fresh.DongleID, fresh.RouteName) {
		t.Error("expected fresh route to still exist in DB")
	}

	staleDir := env.store.RouteDir(stale.DongleID, stale.RouteName)
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("expected stale route directory to be removed, stat err=%v", err)
	}
	freshDir := env.store.RouteDir(fresh.DongleID, fresh.RouteName)
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("expected fresh route directory to remain, stat err=%v", err)
	}
}

func TestCleanupWorker_SkipsPreservedRoute(t *testing.T) {
	env := newCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	oldEnd := now.Add(-30 * 24 * time.Hour)

	preserved := env.seedRoute(ctx, "dongle_keep", "2024-12-16--08-00-00", oldEnd, true)

	w := env.newWorker(now, 7, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.routeExists(ctx, preserved.DongleID, preserved.RouteName) {
		t.Error("preserved route was deleted from DB; cleanup must skip preserved=true")
	}
	dir := env.store.RouteDir(preserved.DongleID, preserved.RouteName)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("expected preserved route directory to remain, stat err=%v", err)
	}
}

func TestCleanupWorker_DryRunDoesNotDelete(t *testing.T) {
	env := newCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	staleEnd := now.Add(-30 * 24 * time.Hour)

	stale := env.seedRoute(ctx, "dongle_dry", "2024-12-16--08-00-00", staleEnd, false)

	w := env.newWorker(now, 7, true) // DryRun=true
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (dry-run): %v", err)
	}

	if !env.routeExists(ctx, stale.DongleID, stale.RouteName) {
		t.Error("dry-run must not delete the route row")
	}
	dir := env.store.RouteDir(stale.DongleID, stale.RouteName)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dry-run must not delete files; stat err=%v", err)
	}
}

func TestCleanupWorker_ZeroRetentionSkipsPass(t *testing.T) {
	env := newCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	oldEnd := now.Add(-365 * 24 * time.Hour)

	stale := env.seedRoute(ctx, "dongle_zero", "2024-01-15--12-00-00", oldEnd, false)

	// retention_days=0 must be treated as "never delete" even for very old
	// routes that would otherwise be purged.
	w := env.newWorker(now, 0, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.routeExists(ctx, stale.DongleID, stale.RouteName) {
		t.Error("retention_days=0 must skip deletion")
	}
}

func TestCleanupWorker_MaxDeletionsPerRunCaps(t *testing.T) {
	env := newCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

	// Seed three stale routes with distinct end times so the ORDER BY
	// end_time ASC in ListStaleRoutes is deterministic.
	oldest := env.seedRoute(ctx, "dongle_a", "2024-12-01--00-00-00", now.Add(-45*24*time.Hour), false)
	middle := env.seedRoute(ctx, "dongle_a", "2024-12-10--00-00-00", now.Add(-36*24*time.Hour), false)
	newest := env.seedRoute(ctx, "dongle_a", "2024-12-20--00-00-00", now.Add(-26*24*time.Hour), false)

	w := env.newWorker(now, 7, false)
	w.MaxDeletionsPerRun = 2 // cap the batch

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// The oldest two should go; the newest of the three should survive the
	// first pass.
	if env.routeExists(ctx, oldest.DongleID, oldest.RouteName) {
		t.Error("oldest stale route should be deleted first")
	}
	if env.routeExists(ctx, middle.DongleID, middle.RouteName) {
		t.Error("second-oldest stale route should be deleted within the batch cap")
	}
	if !env.routeExists(ctx, newest.DongleID, newest.RouteName) {
		t.Error("newest stale route should survive until the next pass (cap hit)")
	}
}

func TestCleanupWorker_RunOnceNoStaleRoutes(t *testing.T) {
	env := newCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	fresh := env.seedRoute(ctx, "dongle_fresh", "2025-01-14--12-00-00", now.Add(-1*24*time.Hour), false)

	w := env.newWorker(now, 7, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.routeExists(ctx, fresh.DongleID, fresh.RouteName) {
		t.Error("no routes should be deleted when none are stale")
	}
}
