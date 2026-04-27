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

// TestALPRSchema_MigrationUpDown verifies that the 015_alpr_schema migration
// applies cleanly on top of a fresh database (after every prior up.sql) and
// rolls back cleanly via 015_alpr_schema.down.sql.
//
// After up.sql runs, every table named in the feature spec must exist. After
// down.sql runs, none of those tables may remain. The test reapplies the up
// migration after the down to confirm the up file is idempotent (CREATE
// TABLE IF NOT EXISTS) and re-runs the down to leave the DB clean for the
// next test.
//
// Skips when DATABASE_URL is unset or unreachable, matching the convention
// used by the other integration tests in this package.
func TestALPRSchema_MigrationUpDown(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		t.Skipf("DATABASE_URL unreachable: %v", err)
	}

	migDir := findALPRSchemaMigrationsDir(t)

	// Reset both the pre-existing tables and the ALPR tables in case a
	// previous test run left state behind.
	resetALPRSchema(ctx, t, pool)
	t.Cleanup(func() { resetALPRSchema(context.Background(), t, pool) })

	// Up: replay every *.up.sql in lexical order. This includes the new
	// 015_alpr_schema.up.sql and asserts it composes with the existing
	// migrations (e.g. shared types, no name collisions).
	applyALPRSchemaUpMigrations(ctx, t, pool, migDir)

	expectedTables := []string{
		"plate_detections",
		"plate_encounters",
		"plate_watchlist",
		"plate_alert_events",
		"route_turns",
		"alpr_segment_progress",
		"alpr_audit_log",
	}
	for _, table := range expectedTables {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("expected table %s to exist after up migration", table)
		}
	}

	// Down: applying just the ALPR-schema down file must remove every
	// table the up file created.
	downBytes, err := os.ReadFile(filepath.Join(migDir, "015_alpr_schema.down.sql"))
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(downBytes)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	for _, table := range expectedTables {
		if tableExists(ctx, t, pool, table) {
			t.Errorf("expected table %s to be dropped after down migration", table)
		}
	}

	// Idempotency: re-applying the up migration on the now-clean database
	// must succeed (CREATE TABLE IF NOT EXISTS).
	upBytes, err := os.ReadFile(filepath.Join(migDir, "015_alpr_schema.up.sql"))
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(upBytes)); err != nil {
		t.Fatalf("re-apply up migration: %v", err)
	}
	// And a second time -- still must succeed.
	if _, err := pool.Exec(ctx, string(upBytes)); err != nil {
		t.Fatalf("re-apply up migration twice: %v", err)
	}

	// Down twice: also idempotent.
	if _, err := pool.Exec(ctx, string(downBytes)); err != nil {
		t.Fatalf("re-apply down migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(downBytes)); err != nil {
		t.Fatalf("re-apply down migration twice: %v", err)
	}
}

// TestALPRSchema_PlateHashLengthCheck confirms the CHECK constraint on
// plate_hash rejects rows whose hash is not exactly 32 bytes. This guards
// against accidental truncation by HMAC code paths that only produce the
// hex form.
func TestALPRSchema_PlateHashLengthCheck(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		t.Skipf("DATABASE_URL unreachable: %v", err)
	}

	migDir := findALPRSchemaMigrationsDir(t)
	resetALPRSchema(ctx, t, pool)
	t.Cleanup(func() { resetALPRSchema(context.Background(), t, pool) })
	applyALPRSchemaUpMigrations(ctx, t, pool, migDir)

	// 31 bytes: must be rejected by the CHECK constraint.
	short := make([]byte, 31)
	_, err = pool.Exec(ctx, `
		INSERT INTO plate_detections
			(dongle_id, route, segment, frame_offset_ms, plate_hash, bbox,
			 confidence, frame_ts)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, now())
	`, "d1", "r1", 0, 0, short, `{"x":0,"y":0,"w":1,"h":1}`, 0.5)
	if err == nil {
		t.Errorf("expected CHECK violation for 31-byte plate_hash, got nil")
	}

	// 32 bytes: must succeed.
	good := make([]byte, 32)
	_, err = pool.Exec(ctx, `
		INSERT INTO plate_detections
			(dongle_id, route, segment, frame_offset_ms, plate_hash, bbox,
			 confidence, frame_ts)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, now())
	`, "d1", "r1", 0, 0, good, `{"x":0,"y":0,"w":1,"h":1}`, 0.5)
	if err != nil {
		t.Errorf("expected 32-byte plate_hash to insert cleanly, got %v", err)
	}
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM   information_schema.tables
			WHERE  table_schema = 'public'
			  AND  table_name   = $1
		)
	`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("tableExists(%s): %v", name, err)
	}
	return exists
}

// resetALPRSchema wipes both the ALPR tables and the pre-existing tables so
// each test run starts from an empty database.
func resetALPRSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	stmts := []string{
		// ALPR tables (drop first so leftover state from prior runs is gone).
		`DROP TABLE IF EXISTS alpr_audit_log         CASCADE`,
		`DROP TABLE IF EXISTS alpr_segment_progress  CASCADE`,
		`DROP TABLE IF EXISTS route_turns            CASCADE`,
		`DROP TABLE IF EXISTS plate_alert_events     CASCADE`,
		`DROP TABLE IF EXISTS plate_watchlist        CASCADE`,
		`DROP TABLE IF EXISTS plate_encounters       CASCADE`,
		`DROP TABLE IF EXISTS plate_detections       CASCADE`,
		// Pre-existing tables from previous migrations.
		`DROP TABLE IF EXISTS crashes                CASCADE`,
		`DROP TABLE IF EXISTS route_data_requests    CASCADE`,
		`DROP TABLE IF EXISTS events                 CASCADE`,
		`DROP TABLE IF EXISTS trips                  CASCADE`,
		`DROP TABLE IF EXISTS segments               CASCADE`,
		`DROP TABLE IF EXISTS route_tags             CASCADE`,
		`DROP TABLE IF EXISTS routes                 CASCADE`,
		`DROP TABLE IF EXISTS device_params          CASCADE`,
		`DROP TABLE IF EXISTS devices                CASCADE`,
		`DROP TABLE IF EXISTS ui_users               CASCADE`,
		`DROP TABLE IF EXISTS settings               CASCADE`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("reset schema (%s): %v", s, err)
		}
	}
}

func applyALPRSchemaUpMigrations(ctx context.Context, t *testing.T, pool *pgxpool.Pool, migDir string) {
	t.Helper()
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir %s: %v", migDir, err)
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
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("migration %s failed: %v", name, err)
		}
	}
}

func findALPRSchemaMigrationsDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
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
