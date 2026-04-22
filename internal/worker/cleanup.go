package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
)

// DefaultCleanupInterval is how often the cleanup worker wakes up to scan
// for stale routes when CleanupWorker.Interval is zero.
const DefaultCleanupInterval = 24 * time.Hour

// DefaultMaxDeletionsPerRun caps how many routes a single pass will remove
// when CleanupWorker.MaxDeletionsPerRun is zero. A bound protects operators
// from a runaway retention change deleting everything at once.
const DefaultMaxDeletionsPerRun = 1000

// CleanupWorker removes routes (and their on-disk files) that are older than
// the configured retention window and not marked preserved.
//
// The worker is intentionally conservative:
//   - retention_days == 0 means "never delete"; the pass is skipped.
//   - MaxDeletionsPerRun caps each pass so a misconfiguration cannot delete
//     every route at once; the next tick picks up remaining stale routes.
//   - DryRun logs what would be deleted without touching the filesystem or
//     database. This is the default on first boot -- operators opt in to
//     real deletion by setting DELETE_DRY_RUN=false.
type CleanupWorker struct {
	Queries            *db.Queries
	Storage            *storage.Storage
	Settings           *settings.Store
	Interval           time.Duration
	MaxDeletionsPerRun int
	DryRun             bool

	// EnvRetentionDays is used as the fallback retention window when the
	// settings table does not have a retention_days override. When zero,
	// effective retention also stays at zero (never delete).
	EnvRetentionDays int

	// now is overridable so tests can freeze time.
	now func() time.Time
}

// Run blocks until ctx is cancelled, performing one cleanup pass immediately
// and then once per Interval. Errors from individual passes are logged and
// do not stop the loop so transient database issues do not permanently
// disable cleanup.
func (w *CleanupWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultCleanupInterval
	}

	if w.DryRun {
		log.Printf("cleanup worker: starting (interval=%s, dry_run=true)", interval)
	} else {
		log.Printf("cleanup worker: starting (interval=%s, dry_run=false)", interval)
	}

	// Run one pass immediately so newly configured retention takes effect
	// without waiting a full interval.
	if err := w.RunOnce(ctx); err != nil {
		log.Printf("cleanup worker: pass failed: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.RunOnce(ctx); err != nil {
				log.Printf("cleanup worker: pass failed: %v", err)
			}
		}
	}
}

// RunOnce performs a single cleanup pass. It is exported so tests (and the
// operator who wants an on-demand sweep) can invoke the same code the loop
// uses without waiting for the ticker.
func (w *CleanupWorker) RunOnce(ctx context.Context) error {
	retentionDays, err := w.Settings.RetentionDays(ctx, w.EnvRetentionDays)
	if err != nil {
		// RetentionDays already falls back to EnvRetentionDays on a missing
		// row; any error here is a transport failure worth surfacing.
		return fmt.Errorf("resolve retention_days: %w", err)
	}
	if retentionDays <= 0 {
		// 0 (or an accidental negative) means "never delete".
		return nil
	}

	limit := w.MaxDeletionsPerRun
	if limit <= 0 {
		limit = DefaultMaxDeletionsPerRun
	}

	cutoff := w.nowFn().Add(-time.Duration(retentionDays) * 24 * time.Hour)

	routes, err := w.Queries.ListStaleRoutes(ctx, db.ListStaleRoutesParams{
		EndTime: pgtype.Timestamptz{Time: cutoff, Valid: true},
		Limit:   int32(limit),
	})
	if err != nil {
		return fmt.Errorf("list stale routes: %w", err)
	}

	if len(routes) == 0 {
		return nil
	}

	log.Printf("cleanup worker: found %d stale route(s) older than %s (retention_days=%d)",
		len(routes), cutoff.UTC().Format(time.RFC3339), retentionDays)

	var errs []error
	for _, r := range routes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.deleteRoute(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", r.DongleID, r.RouteName, err))
		}
	}
	return errors.Join(errs...)
}

// deleteRoute removes a single stale route's on-disk files and database row.
// In dry-run mode it only logs what would happen.
func (w *CleanupWorker) deleteRoute(ctx context.Context, r db.Route) error {
	bytes, err := w.Storage.RouteBytes(r.DongleID, r.RouteName)
	if err != nil {
		// Size accounting is best-effort; a missing or partially-readable
		// directory should not block deletion. Log and continue with 0.
		log.Printf("cleanup worker: size lookup failed for %s/%s: %v",
			r.DongleID, r.RouteName, err)
		bytes = 0
	}

	endTime := "unknown"
	if r.EndTime.Valid {
		endTime = r.EndTime.Time.UTC().Format(time.RFC3339)
	}

	if w.DryRun {
		log.Printf("cleanup worker: [dry-run] would delete route dongle_id=%s route_name=%s end_time=%s bytes_freed=%d",
			r.DongleID, r.RouteName, endTime, bytes)
		return nil
	}

	// Remove files first. If the DB delete fails after, the next pass will
	// still match this row and the (now missing) directory is a no-op. If we
	// did it the other way round and the filesystem remove failed, we would
	// have orphaned files with no DB record pointing at them.
	if err := w.Storage.RemoveRoute(r.DongleID, r.RouteName); err != nil {
		return fmt.Errorf("remove files: %w", err)
	}

	if err := w.Queries.DeleteRoute(ctx, r.ID); err != nil {
		return fmt.Errorf("delete route row: %w", err)
	}

	log.Printf("cleanup worker: deleted route dongle_id=%s route_name=%s end_time=%s bytes_freed=%d",
		r.DongleID, r.RouteName, endTime, bytes)
	return nil
}

// nowFn returns the current time using w.now when set, falling back to
// time.Now. Kept separate so RunOnce can be tested against a frozen clock.
func (w *CleanupWorker) nowFn() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}
