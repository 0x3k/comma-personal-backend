package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestALPRVehicleSignatures_MigrationUpDown verifies that
// 016_alpr_vehicle_signatures.up.sql creates the vehicle_signatures
// table and the new columns/constraints on plate_detections,
// plate_encounters, and plate_watchlist, and that the matching down
// migration removes them cleanly without breaking the rest of the ALPR
// schema.
//
// Skips when DATABASE_URL is unset or unreachable, matching the rest of
// the integration tests in this package.
func TestALPRVehicleSignatures_MigrationUpDown(t *testing.T) {
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

	// Apply every up.sql in order. This implicitly exercises that the
	// 016 migration composes cleanly on top of 015 (FK ADD CONSTRAINT
	// path runs against the existing plate_encounters.signature_id
	// column).
	applyALPRSchemaUpMigrations(ctx, t, pool, migDir)

	// vehicle_signatures must now exist.
	if !tableExists(ctx, t, pool, "vehicle_signatures") {
		t.Fatalf("expected vehicle_signatures to exist after 016 up migration")
	}

	// plate_detections must have the new det_* + signature_id columns.
	for _, col := range []string{
		"signature_id", "det_make", "det_model", "det_color",
		"det_body_type", "det_attr_confidence",
	} {
		if !columnExists(ctx, t, pool, "plate_detections", col) {
			t.Errorf("expected plate_detections.%s to exist", col)
		}
	}
	// plate_watchlist must have signature_id.
	if !columnExists(ctx, t, pool, "plate_watchlist", "signature_id") {
		t.Errorf("expected plate_watchlist.signature_id to exist")
	}
	// plate_encounters.signature_id was pre-declared in 015; the FK
	// constraint should now exist.
	if !constraintExists(ctx, t, pool, "plate_encounters_signature_id_fkey") {
		t.Errorf("expected plate_encounters_signature_id_fkey FK constraint to exist")
	}

	// Idempotency of the up migration: applying it a second time on
	// the already-up schema must not error.
	upBytes, err := os.ReadFile(filepath.Join(migDir, "016_alpr_vehicle_signatures.up.sql"))
	if err != nil {
		t.Fatalf("read 016 up: %v", err)
	}
	if _, err := pool.Exec(ctx, string(upBytes)); err != nil {
		t.Fatalf("re-apply 016 up: %v", err)
	}

	// Down: applying just 016's down must remove the table, the
	// columns this migration added, and the FK on plate_encounters --
	// while leaving the rest of the ALPR schema (and the 015-declared
	// plate_encounters.signature_id column) intact.
	downBytes, err := os.ReadFile(filepath.Join(migDir, "016_alpr_vehicle_signatures.down.sql"))
	if err != nil {
		t.Fatalf("read 016 down: %v", err)
	}
	if _, err := pool.Exec(ctx, string(downBytes)); err != nil {
		t.Fatalf("apply 016 down: %v", err)
	}

	if tableExists(ctx, t, pool, "vehicle_signatures") {
		t.Errorf("expected vehicle_signatures to be dropped after 016 down")
	}
	for _, col := range []string{
		"signature_id", "det_make", "det_model", "det_color",
		"det_body_type", "det_attr_confidence",
	} {
		if columnExists(ctx, t, pool, "plate_detections", col) {
			t.Errorf("expected plate_detections.%s to be dropped after 016 down", col)
		}
	}
	if columnExists(ctx, t, pool, "plate_watchlist", "signature_id") {
		t.Errorf("expected plate_watchlist.signature_id to be dropped after 016 down")
	}
	if constraintExists(ctx, t, pool, "plate_encounters_signature_id_fkey") {
		t.Errorf("expected plate_encounters_signature_id_fkey to be dropped after 016 down")
	}
	// 015-declared plate_encounters.signature_id column itself must
	// survive.
	if !columnExists(ctx, t, pool, "plate_encounters", "signature_id") {
		t.Errorf("plate_encounters.signature_id (declared by 015) was unexpectedly dropped")
	}
	// The 015-created tables must still be there.
	for _, table := range []string{"plate_detections", "plate_encounters", "plate_watchlist"} {
		if !tableExists(ctx, t, pool, table) {
			t.Errorf("expected %s to survive 016 down", table)
		}
	}

	// Down twice: also idempotent.
	if _, err := pool.Exec(ctx, string(downBytes)); err != nil {
		t.Fatalf("re-apply 016 down: %v", err)
	}
}

// TestALPRVehicleSignatures_BodyTypeCheck confirms the CHECK constraint
// on vehicle_signatures.body_type rejects values outside the fixed
// vocabulary while accepting NULL and the listed kinds.
func TestALPRVehicleSignatures_BodyTypeCheck(t *testing.T) {
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

	// Bogus body_type must be rejected.
	if _, err := pool.Exec(ctx,
		`INSERT INTO vehicle_signatures (signature_key, body_type) VALUES ($1, $2)`,
		"bogus|spaceship", "spaceship",
	); err == nil {
		t.Errorf("expected CHECK violation for body_type='spaceship', got nil")
	}

	// NULL body_type is allowed.
	if _, err := pool.Exec(ctx,
		`INSERT INTO vehicle_signatures (signature_key) VALUES ($1)`,
		"unknown|attrs",
	); err != nil {
		t.Errorf("expected NULL body_type to be accepted, got %v", err)
	}

	// All listed body_type values must be accepted.
	for _, bt := range []string{"sedan", "suv", "truck", "hatchback", "coupe", "van", "wagon", "motorcycle", "other"} {
		_, err := pool.Exec(ctx,
			`INSERT INTO vehicle_signatures (signature_key, body_type) VALUES ($1, $2)`,
			"key|"+bt, bt,
		)
		if err != nil {
			t.Errorf("expected body_type=%q to be accepted, got %v", bt, err)
		}
	}
}

// columnExists reports whether the given table has a column with the
// supplied name.
func columnExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM   information_schema.columns
			WHERE  table_schema = 'public'
			  AND  table_name   = $1
			  AND  column_name  = $2
		)
	`, table, column).Scan(&exists)
	if err != nil {
		t.Fatalf("columnExists(%s.%s): %v", table, column, err)
	}
	return exists
}

// constraintExists reports whether a constraint with the given name is
// attached to any table in the public schema.
func constraintExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM   pg_constraint c
			JOIN   pg_namespace  n ON n.oid = c.connamespace
			WHERE  n.nspname = 'public'
			  AND  c.conname = $1
		)
	`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("constraintExists(%s): %v", name, err)
	}
	return exists
}
