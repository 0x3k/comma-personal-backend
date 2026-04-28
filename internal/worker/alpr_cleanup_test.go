package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// alprCleanupTestEnv mirrors the per-schema isolation pattern from
// cleanup_test.go: each test gets a unique Postgres schema with the
// migrations applied, so the suite is parallel-safe and a failure in
// one test cannot contaminate another.
type alprCleanupTestEnv struct {
	t          *testing.T
	pool       *pgxpool.Pool
	queries    *db.Queries
	settings   *settings.Store
	schemaName string
}

// newALPRCleanupTestEnv boots Postgres, applies migrations into a
// fresh schema, and returns a ready-to-use environment. Skips the test
// when TEST_DATABASE_URL is not set.
func newALPRCleanupTestEnv(t *testing.T) *alprCleanupTestEnv {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping ALPR cleanup worker integration tests. " +
			"Set to a Postgres + PostGIS DSN (e.g. postgres://comma:comma@localhost:5432/comma?sslmode=disable) to run.")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}

	schemaName := fmt.Sprintf("alpr_cleanup_test_%d_%s",
		time.Now().UnixNano(), sanitizeForSchema(t.Name()))

	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s`, schemaName)); err != nil {
		pool.Close()
		t.Fatalf("failed to create schema %s: %v", schemaName, err)
	}

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
	settingsStore := settings.New(queries)

	env := &alprCleanupTestEnv{
		t:          t,
		pool:       pool,
		queries:    queries,
		settings:   settingsStore,
		schemaName: schemaName,
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dropSchema(cleanupCtx, pool, schemaName)
		pool.Close()
	})

	return env
}

// hashPlate produces a 32-byte SHA-256 hash of the canonical plate
// text. The retention worker only cares about the hash, not the
// ciphertext, so the test fixtures use SHA-256 directly instead of
// HMAC; the column constraint (octet_length = 32) is satisfied either
// way.
func hashPlate(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// seedDetection inserts one plate_detection row at the given timestamp
// for the given (dongle, route, plate). bbox is a stub JSON object
// because the retention sweep does not look at it. Returns the row ID
// so a subsequent assertion can check existence by ID.
func (e *alprCleanupTestEnv) seedDetection(ctx context.Context, dongle, route string, plateHash []byte, frameTS time.Time) int64 {
	e.t.Helper()

	bbox, err := json.Marshal(map[string]int{"x": 0, "y": 0, "w": 100, "h": 50})
	if err != nil {
		e.t.Fatalf("marshal bbox: %v", err)
	}

	row, err := e.queries.InsertDetection(ctx, db.InsertDetectionParams{
		DongleID:        dongle,
		Route:           route,
		Segment:         0,
		FrameOffsetMs:   0,
		PlateCiphertext: nil,
		PlateHash:       plateHash,
		Bbox:            bbox,
		Confidence:      0.9,
		OcrCorrected:    false,
		FrameTs:         pgtype.Timestamptz{Time: frameTS, Valid: true},
	})
	if err != nil {
		e.t.Fatalf("insert detection: %v", err)
	}
	return row.ID
}

// seedEncounter inserts a plate_encounter row spanning [first, last].
// The retention sweep's orphan check looks for a detection whose
// frame_ts falls inside this window; tests that want the encounter to
// be orphaned simply omit such a detection.
func (e *alprCleanupTestEnv) seedEncounter(ctx context.Context, dongle, route string, plateHash []byte, first, last time.Time) int64 {
	e.t.Helper()

	bbox, err := json.Marshal(map[string]int{"x": 0, "y": 0, "w": 100, "h": 50})
	if err != nil {
		e.t.Fatalf("marshal bbox: %v", err)
	}

	row, err := e.queries.UpsertEncounter(ctx, db.UpsertEncounterParams{
		DongleID:              dongle,
		Route:                 route,
		PlateHash:             plateHash,
		FirstSeenTs:           pgtype.Timestamptz{Time: first, Valid: true},
		LastSeenTs:            pgtype.Timestamptz{Time: last, Valid: true},
		DetectionCount:        1,
		TurnCount:             0,
		MaxInternalGapSeconds: 0,
		SignatureID:           pgtype.Int8{Valid: false},
		Status:                "active",
		BboxFirst:             bbox,
		BboxLast:              bbox,
	})
	if err != nil {
		e.t.Fatalf("upsert encounter: %v", err)
	}
	return row.ID
}

// seedWatchlistAlertedUnacked seeds an alerted, unacked watchlist row
// at the given severity. Such a plate is in the flagged set and its
// detections must survive the unflagged-tier purge.
func (e *alprCleanupTestEnv) seedWatchlistAlertedUnacked(ctx context.Context, plateHash []byte, severity int16) {
	e.t.Helper()
	now := time.Now().UTC()
	_, err := e.queries.UpsertWatchlistAlerted(ctx, db.UpsertWatchlistAlertedParams{
		PlateHash:       plateHash,
		LabelCiphertext: nil,
		Severity:        pgtype.Int2{Int16: severity, Valid: true},
		AlertAt:         pgtype.Timestamptz{Time: now, Valid: true},
		Notes:           pgtype.Text{Valid: false},
	})
	if err != nil {
		e.t.Fatalf("upsert alerted watchlist: %v", err)
	}
}

// seedWatchlistAlertedAcked seeds an alerted watchlist row that has
// been acknowledged. Severity decides whether the row stays in the
// flagged set (>=4) or demotes to unflagged retention (<=3).
func (e *alprCleanupTestEnv) seedWatchlistAlertedAcked(ctx context.Context, plateHash []byte, severity int16) {
	e.t.Helper()
	now := time.Now().UTC()
	_, err := e.queries.UpsertWatchlistAlerted(ctx, db.UpsertWatchlistAlertedParams{
		PlateHash:       plateHash,
		LabelCiphertext: nil,
		Severity:        pgtype.Int2{Int16: severity, Valid: true},
		AlertAt:         pgtype.Timestamptz{Time: now, Valid: true},
		Notes:           pgtype.Text{Valid: false},
	})
	if err != nil {
		e.t.Fatalf("upsert alerted watchlist: %v", err)
	}
	if _, err := e.queries.AckWatchlist(ctx, db.AckWatchlistParams{
		PlateHash: plateHash,
		AckedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		e.t.Fatalf("ack watchlist: %v", err)
	}
}

// seedWatchlistWhitelist seeds a whitelist row. Whitelisted plates are
// NOT in the flagged set per the user's "this is fine, drop it to
// unflagged retention" classification.
func (e *alprCleanupTestEnv) seedWatchlistWhitelist(ctx context.Context, plateHash []byte) {
	e.t.Helper()
	_, err := e.queries.UpsertWatchlistWhitelist(ctx, db.UpsertWatchlistWhitelistParams{
		PlateHash:       plateHash,
		LabelCiphertext: nil,
		Notes:           pgtype.Text{Valid: false},
	})
	if err != nil {
		e.t.Fatalf("upsert whitelist: %v", err)
	}
}

// seedAlertEvent inserts a plate_alert_events row at a specified time
// by writing the row directly (the InsertAlertEvent query uses the
// default of now() for computed_at, which we cannot override). This
// keeps the test deterministic for the orphan-events purge.
func (e *alprCleanupTestEnv) seedAlertEvent(ctx context.Context, plateHash []byte, computedAt time.Time) int64 {
	e.t.Helper()

	components, err := json.Marshal([]map[string]any{{"name": "test", "points": 1.0}})
	if err != nil {
		e.t.Fatalf("marshal components: %v", err)
	}

	var id int64
	err = e.pool.QueryRow(ctx, `
		INSERT INTO plate_alert_events (plate_hash, severity, components, heuristic_version, computed_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, plateHash, int16(3), components, "test-v1", computedAt).Scan(&id)
	if err != nil {
		e.t.Fatalf("insert alert event: %v", err)
	}
	return id
}

// detectionExists returns whether the given detection row still has a
// row in plate_detections. Used by tests to assert delete/dry-run
// behaviour.
func (e *alprCleanupTestEnv) detectionExists(ctx context.Context, id int64) bool {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM plate_detections WHERE id = $1`, id).Scan(&n)
	if err != nil {
		e.t.Fatalf("check detection: %v", err)
	}
	return n > 0
}

// encounterExists returns whether the given encounter row is still
// present.
func (e *alprCleanupTestEnv) encounterExists(ctx context.Context, id int64) bool {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM plate_encounters WHERE id = $1`, id).Scan(&n)
	if err != nil {
		e.t.Fatalf("check encounter: %v", err)
	}
	return n > 0
}

// alertEventExists returns whether the given alert event row is still
// present.
func (e *alprCleanupTestEnv) alertEventExists(ctx context.Context, id int64) bool {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM plate_alert_events WHERE id = $1`, id).Scan(&n)
	if err != nil {
		e.t.Fatalf("check alert event: %v", err)
	}
	return n > 0
}

// watchlistExists returns whether a watchlist row is still present for
// the given plate. Tests use this to confirm the worker NEVER touches
// plate_watchlist (criterion 3 step 6).
func (e *alprCleanupTestEnv) watchlistExists(ctx context.Context, plateHash []byte) bool {
	e.t.Helper()
	var n int
	err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM plate_watchlist WHERE plate_hash = $1`, plateHash).Scan(&n)
	if err != nil {
		e.t.Fatalf("check watchlist: %v", err)
	}
	return n > 0
}

// newWorker returns an ALPRCleanupWorker pre-wired to this env, with
// a frozen clock and the alpr_enabled flag forced on so the test does
// not need to seed a settings row to drive RunOnce.
func (e *alprCleanupTestEnv) newWorker(now time.Time, unflaggedDays, flaggedDays int, dryRun bool) *ALPRCleanupWorker {
	enabled := true
	return &ALPRCleanupWorker{
		Queries:                   e.queries,
		Settings:                  e.settings,
		DryRun:                    dryRun,
		EnvRetentionDaysUnflagged: unflaggedDays,
		EnvRetentionDaysFlagged:   flaggedDays,
		now:                       func() time.Time { return now },
		alprEnabledForTest:        &enabled,
	}
}

// TestALPRCleanupWorker_DeletesUnflaggedDetectionsBeyondCutoff is the
// happy-path baseline: a detection past the unflagged window with no
// watchlist entry is purged; a detection inside the window survives.
func TestALPRCleanupWorker_DeletesUnflaggedDetectionsBeyondCutoff(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	plate := hashPlate("ABC123")

	// 60-day-old (well past 30d unflagged window) -> deleted.
	staleID := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-60*24*time.Hour))
	// 5-day-old (inside the 30d unflagged window) -> survives.
	freshID := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-5*24*time.Hour))

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if env.detectionExists(ctx, staleID) {
		t.Error("expected stale unflagged detection to be deleted")
	}
	if !env.detectionExists(ctx, freshID) {
		t.Error("expected fresh detection to survive")
	}
}

// TestALPRCleanupWorker_PreservesFlaggedDetectionsInUnflaggedTier
// covers the central "flagged set protects detections" rule: a plate
// that is alerted+unacked must keep its old detections under the
// unflagged window.
func TestALPRCleanupWorker_PreservesFlaggedDetectionsInUnflaggedTier(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	flaggedPlate := hashPlate("FLAGGED1")
	otherPlate := hashPlate("OTHER1")

	// Both 60-day-old. flaggedPlate is alerted+unacked; otherPlate has
	// no watchlist row.
	flaggedDetID := env.seedDetection(ctx, "dongle_a", "route_a", flaggedPlate, now.Add(-60*24*time.Hour))
	otherDetID := env.seedDetection(ctx, "dongle_a", "route_a", otherPlate, now.Add(-60*24*time.Hour))

	env.seedWatchlistAlertedUnacked(ctx, flaggedPlate, 3)

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.detectionExists(ctx, flaggedDetID) {
		t.Error("expected flagged-plate detection to survive the unflagged-tier purge")
	}
	if env.detectionExists(ctx, otherDetID) {
		t.Error("expected non-flagged detection to be deleted")
	}
}

// TestALPRCleanupWorker_AbsoluteCeilingDeletesFlaggedTooOld is the
// belt-and-braces guarantee: a detection past the flagged retention
// window is deleted even though its plate is in the flagged set.
func TestALPRCleanupWorker_AbsoluteCeilingDeletesFlaggedTooOld(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	flaggedPlate := hashPlate("FLAGGED2")

	// Past the 365d flagged ceiling.
	veryOld := env.seedDetection(ctx, "dongle_a", "route_a", flaggedPlate, now.Add(-400*24*time.Hour))
	// Inside the flagged ceiling.
	recent := env.seedDetection(ctx, "dongle_a", "route_a", flaggedPlate, now.Add(-100*24*time.Hour))

	env.seedWatchlistAlertedUnacked(ctx, flaggedPlate, 5)

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if env.detectionExists(ctx, veryOld) {
		t.Error("expected flagged detection past the absolute ceiling to be deleted")
	}
	if !env.detectionExists(ctx, recent) {
		t.Error("expected flagged detection inside the ceiling to survive")
	}
}

// TestALPRCleanupWorker_WhitelistedDropsToUnflaggedTier verifies that
// whitelisted plates do NOT inherit the flagged-tier protection: their
// detections age out under the unflagged window.
func TestALPRCleanupWorker_WhitelistedDropsToUnflaggedTier(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	whitelistedPlate := hashPlate("MYCAR")

	// 60-day-old detection, way past unflagged window.
	id := env.seedDetection(ctx, "dongle_a", "route_a", whitelistedPlate, now.Add(-60*24*time.Hour))
	env.seedWatchlistWhitelist(ctx, whitelistedPlate)

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if env.detectionExists(ctx, id) {
		t.Error("expected whitelisted detection to be deleted under unflagged retention")
	}
	// Watchlist row must NOT be touched.
	if !env.watchlistExists(ctx, whitelistedPlate) {
		t.Error("watchlist row was deleted; the cleanup worker must never touch plate_watchlist")
	}
}

// TestALPRCleanupWorker_AckedLowSeverityDropsToUnflaggedTier covers the
// "acked + severity 2..3 demotes to unflagged retention" semantics the
// user explicitly approved in the feature notes.
func TestALPRCleanupWorker_AckedLowSeverityDropsToUnflaggedTier(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)

	// Two plates, both alerted+acked, both with 60-day-old detections.
	// One at severity 3 (demotes), one at severity 4 (stays flagged).
	lowSev := hashPlate("ACKED_LOW")
	highSev := hashPlate("ACKED_HIGH")

	lowID := env.seedDetection(ctx, "dongle_a", "route_a", lowSev, now.Add(-60*24*time.Hour))
	highID := env.seedDetection(ctx, "dongle_a", "route_a", highSev, now.Add(-60*24*time.Hour))

	env.seedWatchlistAlertedAcked(ctx, lowSev, 3)
	env.seedWatchlistAlertedAcked(ctx, highSev, 4)

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if env.detectionExists(ctx, lowID) {
		t.Error("acked low-severity (sev 3) detection should drop to unflagged retention and be deleted")
	}
	if !env.detectionExists(ctx, highID) {
		t.Error("acked high-severity (sev 4) detection should stay in the flagged set and survive")
	}
}

// TestALPRCleanupWorker_DryRunDoesNotDelete verifies the DELETE_DRY_RUN
// path: the worker should log its intent but make no DB changes.
func TestALPRCleanupWorker_DryRunDoesNotDelete(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	plate := hashPlate("DRYRUN1")

	staleID := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-60*24*time.Hour))
	freshID := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-5*24*time.Hour))

	// First, a dry-run pass: nothing should change.
	dry := env.newWorker(now, 30, 365, true)
	if err := dry.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (dry-run): %v", err)
	}

	if !env.detectionExists(ctx, staleID) {
		t.Error("dry-run must not delete the stale detection")
	}
	if !env.detectionExists(ctx, freshID) {
		t.Error("dry-run must not delete the fresh detection either")
	}

	// Now flip dry-run off: the same delete set should disappear.
	live := env.newWorker(now, 30, 365, false)
	if err := live.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce (live): %v", err)
	}

	if env.detectionExists(ctx, staleID) {
		t.Error("live pass should delete the stale detection identified during dry-run")
	}
	if !env.detectionExists(ctx, freshID) {
		t.Error("live pass must not delete the fresh detection")
	}
}

// TestALPRCleanupWorker_OrphanEncountersCleanedUp seeds an encounter
// whose underlying detections fall outside its window (and thus are
// "missing" by the orphan check) and verifies the encounter is purged.
// Also seeds a non-orphan encounter to make sure the cleanup is
// targeted.
func TestALPRCleanupWorker_OrphanEncountersCleanedUp(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	orphanPlate := hashPlate("ORPHAN1")
	livePlate := hashPlate("LIVE1")

	// Orphan encounter: window is far in the past, no detections inside
	// it. Whitelisted so the encounter survives the detection sweep
	// only by virtue of having no detections in scope; in production
	// the underlying detections would have been pruned by tier 1.
	orphanFirst := now.Add(-200 * 24 * time.Hour)
	orphanLast := now.Add(-199 * 24 * time.Hour)
	orphanID := env.seedEncounter(ctx, "dongle_a", "route_orphan", orphanPlate, orphanFirst, orphanLast)

	// Live encounter: window contains a freshly seeded detection.
	liveFirst := now.Add(-2 * time.Hour)
	liveLast := now.Add(-1 * time.Hour)
	liveID := env.seedEncounter(ctx, "dongle_a", "route_live", livePlate, liveFirst, liveLast)
	env.seedDetection(ctx, "dongle_a", "route_live", livePlate, liveFirst.Add(15*time.Minute))

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if env.encounterExists(ctx, orphanID) {
		t.Error("expected orphan encounter (no detections inside its window) to be deleted")
	}
	if !env.encounterExists(ctx, liveID) {
		t.Error("expected live encounter (detection inside window) to survive")
	}
}

// TestALPRCleanupWorker_OrphanAlertEventsPurged covers tier 4: alert
// events for plates not on the watchlist are deleted once they are
// past the 90d review window; events for watchlisted plates are
// preserved indefinitely.
func TestALPRCleanupWorker_OrphanAlertEventsPurged(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	watchlistedPlate := hashPlate("WATCHED1")
	orphanedPlate := hashPlate("ORPHANED1")

	// Watchlist row for one plate so its events stay regardless of age.
	env.seedWatchlistAlertedUnacked(ctx, watchlistedPlate, 4)

	// Old events (>90d) for both plates.
	keepID := env.seedAlertEvent(ctx, watchlistedPlate, now.Add(-120*24*time.Hour))
	purgeID := env.seedAlertEvent(ctx, orphanedPlate, now.Add(-120*24*time.Hour))
	// Fresh event for orphaned plate (<90d) -> survives this sweep.
	recentID := env.seedAlertEvent(ctx, orphanedPlate, now.Add(-30*24*time.Hour))

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.alertEventExists(ctx, keepID) {
		t.Error("alert event for watchlisted plate must be preserved")
	}
	if env.alertEventExists(ctx, purgeID) {
		t.Error("alert event for orphaned plate older than 90d must be deleted")
	}
	if !env.alertEventExists(ctx, recentID) {
		t.Error("alert event for orphaned plate within 90d must be preserved")
	}
}

// TestALPRCleanupWorker_NeverTouchesWatchlistOrSignatures verifies the
// negative criteria: the worker must never delete from plate_watchlist
// (criterion 3.6) or from vehicle_signatures (criterion 3.7), even
// when their rows have no associated detections left.
func TestALPRCleanupWorker_NeverTouchesWatchlistOrSignatures(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	plate := hashPlate("UNTOUCHABLE1")

	env.seedWatchlistAlertedUnacked(ctx, plate, 5)

	// Insert a vehicle_signature row directly so we can check it is not
	// deleted. The signatures schema lives in 016_alpr_vehicle_signatures
	// and is small enough to assert on. signature_key is the UNIQUE
	// canonical fingerprint string.
	var sigID int64
	err := env.pool.QueryRow(ctx, `
		INSERT INTO vehicle_signatures (
			signature_key, make, model, color, body_type, confidence, sample_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, "toyota|camry|white|sedan", "Toyota", "Camry", "white", "sedan", 0.9, 1).Scan(&sigID)
	if err != nil {
		t.Fatalf("insert signature: %v", err)
	}

	w := env.newWorker(now, 30, 365, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.watchlistExists(ctx, plate) {
		t.Error("plate_watchlist row was deleted; cleanup worker must never touch this table")
	}

	var sigCount int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM vehicle_signatures WHERE id = $1`, sigID).Scan(&sigCount); err != nil {
		t.Fatalf("check signature: %v", err)
	}
	if sigCount == 0 {
		t.Error("vehicle_signatures row was deleted; cleanup worker must never touch this table")
	}
}

// TestALPRCleanupWorker_RunDoesNotFireImmediately covers criterion 6:
// when the operator flips alpr_enabled true, the worker must NOT run a
// pass at startup -- the first pass should occur on the next ticker
// tick. Any data that exists at startup-time is preserved.
//
// We assert this by starting Run with a long Interval and checking
// that no DELETE happens in a generous wait window. The worker has no
// "first tick at start" path, but this test guards against future
// regressions that might add one.
func TestALPRCleanupWorker_RunDoesNotFireImmediately(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	plate := hashPlate("BACKFILL1")
	staleID := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-60*24*time.Hour))

	w := env.newWorker(now, 30, 365, false)
	w.Interval = 10 * time.Second // way longer than the test window
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	<-done

	if !env.detectionExists(ctx, staleID) {
		t.Error("Run() must not perform a cleanup pass at startup; the stale detection should still exist")
	}
}

// TestALPRCleanupWorker_DisabledFlagStopsScheduling covers criterion 7:
// when alpr_enabled is false, ticker firings produce no work. We drive
// this by toggling alprEnabledForTest off, running RunOnce-equivalent
// via the loop's tick handler indirectly (by setting a short interval
// and waiting one tick), and asserting nothing was deleted.
func TestALPRCleanupWorker_DisabledFlagStopsScheduling(t *testing.T) {
	env := newALPRCleanupTestEnv(t)

	// Enough time for a couple of ticks at the test's interval.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	plate := hashPlate("DISABLED1")
	staleID := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-60*24*time.Hour))

	disabled := false
	w := &ALPRCleanupWorker{
		Queries:                   env.queries,
		Settings:                  env.settings,
		EnvRetentionDaysUnflagged: 30,
		EnvRetentionDaysFlagged:   365,
		Interval:                  20 * time.Millisecond,
		now:                       func() time.Time { return now },
		alprEnabledForTest:        &disabled,
	}

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	<-done

	if !env.detectionExists(ctx, staleID) {
		t.Error("disabled alpr_enabled must skip ticker-driven passes; stale detection should still exist")
	}
}

// TestALPRCleanupWorker_ZeroRetentionSkipsTier verifies that retention=0
// is treated as "never delete" for the corresponding tier. A 365-day-old
// detection with unflagged=0 is preserved; the same setup with a non-
// zero unflagged window deletes it.
func TestALPRCleanupWorker_ZeroRetentionSkipsTier(t *testing.T) {
	env := newALPRCleanupTestEnv(t)
	ctx := context.Background()

	now := time.Date(2025, 1, 30, 12, 0, 0, 0, time.UTC)
	plate := hashPlate("OLD1")

	id := env.seedDetection(ctx, "dongle_a", "route_a", plate, now.Add(-365*24*time.Hour))

	// unflagged=0 (skip), flagged=0 (skip): nothing should be deleted.
	w := env.newWorker(now, 0, 0, false)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !env.detectionExists(ctx, id) {
		t.Error("retention=0 must be treated as never-delete for both tiers")
	}
}
