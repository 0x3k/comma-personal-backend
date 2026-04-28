package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// DefaultALPRCleanupInterval mirrors the route cleanup worker's once-
// per-day cadence. Aligning the schedules keeps operator mental models
// simple ("retention sweeps run nightly") and avoids unnecessary churn
// in pg_stat_*.
const DefaultALPRCleanupInterval = 24 * time.Hour

// DefaultALPRRetentionDaysUnflagged is the fallback unflagged retention
// window when neither the env var nor the settings row supplies one.
// Mirrors the alprDefaults value in internal/config/alpr.go so the
// worker has a sane default even if it is constructed without a config.
const DefaultALPRRetentionDaysUnflagged = 30

// DefaultALPRRetentionDaysFlagged is the fallback flagged retention
// window when neither the env var nor the settings row supplies one.
// Acts as the absolute ceiling regardless of watchlist state.
const DefaultALPRRetentionDaysFlagged = 365

// alertEventsRetentionDays is how far back the orphan-alert-events
// pruner reaches. Alert events for plates still on the watchlist are
// kept indefinitely (they are the audit trail behind the badge); only
// rows whose plate_hash has been removed from plate_watchlist are
// eligible, and even then we keep 90 days of history so a freshly
// removed plate's review trail does not vanish on the next sweep.
const alertEventsRetentionDays = 90

// alprCleanupSampleSize caps how many plate_hashes the dry-run path
// echoes to the log per pass. A handful of samples is enough to spot-
// check correctness; logging every hash would explode log volume on a
// busy deployment.
const alprCleanupSampleSize = 5

// ALPRCleanupQuerier is the subset of *db.Queries the worker uses.
// Carved into an interface so the test suite can pass a fake or a
// transactional wrapper without depending on the concrete sqlc type.
// Production passes a *db.Queries directly.
type ALPRCleanupQuerier interface {
	ListFlaggedPlateHashes(ctx context.Context) ([][]byte, error)
	DeleteDetectionsOlderThanExcludingFlagged(ctx context.Context, arg db.DeleteDetectionsOlderThanExcludingFlaggedParams) (int64, error)
	DeleteDetectionsOlderThan(ctx context.Context, frameTs pgtype.Timestamptz) (int64, error)
	DeleteOrphanedEncounters(ctx context.Context) (int64, error)
	DeleteOrphanedAlertEvents(ctx context.Context, computedAt pgtype.Timestamptz) (int64, error)
}

// Compile-time assertion that the production sqlc handle satisfies the
// worker's contract.
var _ ALPRCleanupQuerier = (*db.Queries)(nil)

// ALPRCleanupMetrics is the subset of *metrics.Metrics this worker
// uses. A nil value is permitted; the worker treats it as a no-op.
type ALPRCleanupMetrics interface {
	AddALPRCleanupDeletedDetections(tier string, n int64)
	AddALPRCleanupDeletedEncounters(n int64)
	AddALPRCleanupDeletedAlertEvents(n int64)
	ObserveALPRCleanupRun(d time.Duration)
}

// ALPRCleanupWorker prunes the ALPR storage tiers (plate_detections,
// plate_encounters, plate_alert_events) according to a tiered retention
// policy:
//
//   - Unflagged detections (the bulk of the table) age out after
//     RetentionDaysUnflagged. Detections whose plate_hash is in the
//     "flagged set" -- alerted+unacked OR alerted with severity >= 4 --
//     are preserved.
//   - All detections, including flagged ones, hit an absolute ceiling
//     after RetentionDaysFlagged. Without this ceiling a long-running
//     alert would retain evidence forever.
//   - plate_encounters are purged when their underlying detections are
//     gone (orphan cleanup); the row is keyed on (dongle_id, route,
//     plate_hash, [first_seen_ts, last_seen_ts]) so a residual
//     overlapping detection keeps the encounter alive.
//   - plate_alert_events for plates no longer on the watchlist age out
//     after alertEventsRetentionDays (90 days). Plates still on the
//     watchlist keep their full audit trail.
//   - plate_watchlist and vehicle_signatures are NEVER touched: the
//     former is user-curated state, the latter is a small long-lived
//     identity index used for cross-route fusion.
//
// The worker self-gates on alpr_enabled so flipping the master flag
// false at runtime stops scheduling new passes; an in-flight pass
// completes naturally because we never cancel mid-pass.
//
// Each tier runs in its own DB call (no enclosing transaction). The
// passes are independently retry-safe: re-running after a partial
// failure just deletes whatever is left to delete. Wrapping all four
// passes in a single transaction would hold locks across the cutoff
// computation and the largest DELETE on the database; the modest gain
// of strict atomicity is not worth the lock contention against the
// detection pipeline.
type ALPRCleanupWorker struct {
	// Queries is the sqlc-generated db handle. Required.
	Queries ALPRCleanupQuerier

	// Settings is the runtime tunables store. May be nil; in that case
	// alprEnabled() reports false and the worker stays idle.
	Settings *settings.Store

	// Metrics receives per-pass counters and the run-duration histogram.
	// May be nil; the helpers handle nil-receiver gracefully.
	Metrics ALPRCleanupMetrics

	// Interval is the cron-like sweep cadence. Defaults to
	// DefaultALPRCleanupInterval (24h) when zero.
	Interval time.Duration

	// DryRun, when true, logs the deletes the worker would issue but
	// does NOT execute them. Operators set this from DELETE_DRY_RUN at
	// startup; it is the same flag the route cleanup worker honors.
	DryRun bool

	// EnvRetentionDaysUnflagged and EnvRetentionDaysFlagged are the
	// fallback retention windows used when the corresponding settings
	// row is missing. Operators populate these from
	// ALPR_RETENTION_DAYS_UNFLAGGED / ALPR_RETENTION_DAYS_FLAGGED at
	// startup. A zero value at this layer is interpreted as
	// "never delete for that tier" and skips the corresponding pass --
	// matching the route cleanup worker's semantics.
	EnvRetentionDaysUnflagged int
	EnvRetentionDaysFlagged   int

	// now is overridable so tests can freeze time. nil means time.Now.
	now func() time.Time

	// alprEnabledForTest is a test-only override for the master flag.
	// When non-nil, alprEnabled returns the dereferenced value.
	alprEnabledForTest *bool
}

// Run blocks until ctx is cancelled. Per criterion 6, the first pass
// fires on the next tick rather than immediately so flipping
// alpr_enabled true does not surprise the operator with a pre-existing
// data purge. Per criterion 7, alpr_enabled=false stops scheduling new
// passes; an in-flight pass completes naturally because RunOnce never
// observes the flag mid-pass.
func (w *ALPRCleanupWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultALPRCleanupInterval
	}

	if w.DryRun {
		log.Printf("alpr cleanup worker: starting (interval=%s, dry_run=true)", interval)
	} else {
		log.Printf("alpr cleanup worker: starting (interval=%s, dry_run=false)", interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !w.alprEnabled(ctx) {
				// Runtime flag is off: skip this tick. We re-check on
				// the next tick so a runtime flip-back resumes the
				// schedule without restart.
				continue
			}
			if err := w.RunOnce(ctx); err != nil {
				log.Printf("alpr cleanup worker: pass failed: %v", err)
			}
		}
	}
}

// RunOnce performs a single retention pass. Exported so the test suite
// (and a future ad-hoc admin trigger) can drive the same code as the
// loop without waiting for the ticker.
//
// Errors from individual tiers are logged and accumulated; later tiers
// still run so a flake on the unflagged DELETE does not leave orphans
// or stale alert events around.
func (w *ALPRCleanupWorker) RunOnce(ctx context.Context) error {
	if w.Queries == nil {
		return errors.New("alpr cleanup worker: Queries is nil")
	}

	start := w.nowFn()
	defer func() {
		if w.Metrics != nil {
			w.Metrics.ObserveALPRCleanupRun(time.Since(start))
		}
	}()

	unflaggedDays := w.unflaggedRetentionDays(ctx)
	flaggedDays := w.flaggedRetentionDays(ctx)

	// Read the flagged set once at the start. A racing ack/whitelist is
	// acceptable -- the next pass corrects -- and one snapshot keeps the
	// per-tier DELETEs internally consistent.
	flagged, err := w.Queries.ListFlaggedPlateHashes(ctx)
	if err != nil {
		return fmt.Errorf("list flagged plate hashes: %w", err)
	}
	if flagged == nil {
		flagged = [][]byte{}
	}

	now := start
	var errs []error

	// Tier 1: unflagged detections. Skip when retention is 0 (never
	// delete) so an unconfigured deployment cannot accidentally purge.
	if unflaggedDays > 0 {
		cutoff := now.Add(-time.Duration(unflaggedDays) * 24 * time.Hour)
		n, err := w.deleteUnflaggedDetections(ctx, cutoff, flagged)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete unflagged detections: %w", err))
		} else if w.Metrics != nil {
			w.Metrics.AddALPRCleanupDeletedDetections("unflagged", n)
		}
	}

	// Tier 2: absolute ceiling. Skip when 0.
	if flaggedDays > 0 {
		cutoff := now.Add(-time.Duration(flaggedDays) * 24 * time.Hour)
		n, err := w.deleteFlaggedDetections(ctx, cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("delete flagged detections: %w", err))
		} else if w.Metrics != nil {
			w.Metrics.AddALPRCleanupDeletedDetections("flagged", n)
		}
	}

	// Tier 3: orphan encounter cleanup. Always runs (cheap; an idle
	// table is a no-op).
	n, err := w.deleteOrphanEncounters(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("delete orphan encounters: %w", err))
	} else if w.Metrics != nil {
		w.Metrics.AddALPRCleanupDeletedEncounters(n)
	}

	// Tier 4: orphan alert events. Always runs.
	cutoff := now.Add(-alertEventsRetentionDays * 24 * time.Hour)
	n, err = w.deleteOrphanAlertEvents(ctx, cutoff)
	if err != nil {
		errs = append(errs, fmt.Errorf("delete orphan alert events: %w", err))
	} else if w.Metrics != nil {
		w.Metrics.AddALPRCleanupDeletedAlertEvents(n)
	}

	return errors.Join(errs...)
}

// deleteUnflaggedDetections runs (or simulates, in dry-run) the tier-1
// purge: detections older than cutoff whose plate_hash is NOT in the
// flagged set. flagged may be empty, in which case the DELETE collapses
// to "every detection older than cutoff that does not match an empty
// set" -- which is correctly "every detection".
func (w *ALPRCleanupWorker) deleteUnflaggedDetections(ctx context.Context, cutoff time.Time, flagged [][]byte) (int64, error) {
	if w.DryRun {
		// Dry-run cannot run a COUNT here without a separate query; the
		// SELECT-then-DELETE shape would still race the live pipeline.
		// We log the cutoff and the flagged-set size so an operator can
		// reason about the intended scope and disable dry-run when
		// satisfied.
		log.Printf("alpr cleanup worker: [dry-run] would delete unflagged detections older than %s; flagged_set=%d (sample=%s)",
			cutoff.UTC().Format(time.RFC3339), len(flagged), sampleHashes(flagged))
		return 0, nil
	}

	n, err := w.Queries.DeleteDetectionsOlderThanExcludingFlagged(ctx,
		db.DeleteDetectionsOlderThanExcludingFlaggedParams{
			FrameTs:       pgtype.Timestamptz{Time: cutoff, Valid: true},
			FlaggedHashes: flagged,
		})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("alpr cleanup worker: deleted %d unflagged detection(s) older than %s (flagged_set=%d)",
			n, cutoff.UTC().Format(time.RFC3339), len(flagged))
	}
	return n, nil
}

// deleteFlaggedDetections runs the tier-2 absolute ceiling: every
// detection older than cutoff regardless of flag state. Acts as the
// belt-and-braces guarantee that even a long-running alert cannot keep
// evidence past the operator-configured maximum window.
func (w *ALPRCleanupWorker) deleteFlaggedDetections(ctx context.Context, cutoff time.Time) (int64, error) {
	if w.DryRun {
		log.Printf("alpr cleanup worker: [dry-run] would delete all detections older than %s (absolute ceiling)",
			cutoff.UTC().Format(time.RFC3339))
		return 0, nil
	}

	n, err := w.Queries.DeleteDetectionsOlderThan(ctx,
		pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("alpr cleanup worker: deleted %d detection(s) at absolute ceiling older than %s",
			n, cutoff.UTC().Format(time.RFC3339))
	}
	return n, nil
}

// deleteOrphanEncounters runs the tier-3 cleanup: encounters whose
// underlying detections have all been pruned. Cheap when the encounter
// table is idle.
func (w *ALPRCleanupWorker) deleteOrphanEncounters(ctx context.Context) (int64, error) {
	if w.DryRun {
		log.Printf("alpr cleanup worker: [dry-run] would delete orphan encounters")
		return 0, nil
	}

	n, err := w.Queries.DeleteOrphanedEncounters(ctx)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("alpr cleanup worker: deleted %d orphan encounter(s)", n)
	}
	return n, nil
}

// deleteOrphanAlertEvents runs the tier-4 cleanup: alert events older
// than cutoff for plates not on the watchlist. Plates still on the
// watchlist keep their full audit trail.
func (w *ALPRCleanupWorker) deleteOrphanAlertEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	if w.DryRun {
		log.Printf("alpr cleanup worker: [dry-run] would delete alert events older than %s for plates not on watchlist",
			cutoff.UTC().Format(time.RFC3339))
		return 0, nil
	}

	n, err := w.Queries.DeleteOrphanedAlertEvents(ctx,
		pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("alpr cleanup worker: deleted %d orphan alert event(s) older than %s",
			n, cutoff.UTC().Format(time.RFC3339))
	}
	return n, nil
}

// alprEnabled reads the master ALPR flag from the settings store.
// A nil store or read error treats the flag as false (the safe default
// for an opt-in feature).
func (w *ALPRCleanupWorker) alprEnabled(ctx context.Context) bool {
	if w.alprEnabledForTest != nil {
		return *w.alprEnabledForTest
	}
	if w.Settings == nil {
		return false
	}
	v, err := w.Settings.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		log.Printf("alpr cleanup worker: read alpr_enabled: %v", err)
		return false
	}
	return v
}

// unflaggedRetentionDays resolves the effective unflagged retention
// window: settings row if present, else the env-derived default.
// 0 means "never delete unflagged detections".
func (w *ALPRCleanupWorker) unflaggedRetentionDays(ctx context.Context) int {
	if w.Settings != nil {
		v, err := w.Settings.IntOr(ctx, settings.KeyALPRRetentionDaysUnflagged, w.EnvRetentionDaysUnflagged)
		if err != nil {
			log.Printf("alpr cleanup worker: read %s: %v; using env default %d",
				settings.KeyALPRRetentionDaysUnflagged, err, w.EnvRetentionDaysUnflagged)
			return w.EnvRetentionDaysUnflagged
		}
		return v
	}
	return w.EnvRetentionDaysUnflagged
}

// flaggedRetentionDays resolves the effective flagged-tier (absolute
// ceiling) retention window. Same fallback semantics as
// unflaggedRetentionDays.
func (w *ALPRCleanupWorker) flaggedRetentionDays(ctx context.Context) int {
	if w.Settings != nil {
		v, err := w.Settings.IntOr(ctx, settings.KeyALPRRetentionDaysFlagged, w.EnvRetentionDaysFlagged)
		if err != nil {
			log.Printf("alpr cleanup worker: read %s: %v; using env default %d",
				settings.KeyALPRRetentionDaysFlagged, err, w.EnvRetentionDaysFlagged)
			return w.EnvRetentionDaysFlagged
		}
		return v
	}
	return w.EnvRetentionDaysFlagged
}

// nowFn returns the current time using w.now when set (test-friendly),
// falling back to time.Now in production.
func (w *ALPRCleanupWorker) nowFn() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

// sampleHashes formats up to alprCleanupSampleSize plate hashes as a
// comma-separated base64 list for the dry-run log line. Returns the
// literal string "(none)" when the input is empty so the log entry
// still has a sane shape.
func sampleHashes(hashes [][]byte) string {
	if len(hashes) == 0 {
		return "(none)"
	}
	limit := alprCleanupSampleSize
	if len(hashes) < limit {
		limit = len(hashes)
	}
	out := ""
	for i := 0; i < limit; i++ {
		if i > 0 {
			out += ","
		}
		out += base64.StdEncoding.EncodeToString(hashes[i])
	}
	if len(hashes) > limit {
		out += fmt.Sprintf(",...+%d", len(hashes)-limit)
	}
	return out
}
