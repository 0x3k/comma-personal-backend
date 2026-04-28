// worker.go bridges the pure scoring function (scoring.go) to the
// database. It is responsible for:
//
//  1. Loading the inputs Score needs (recent encounters, whitelist
//     status, settings-resolved Thresholds).
//  2. Calling Score.
//  3. Persisting the result: an INSERT into plate_alert_events for
//     every evaluation (audit trail), and an UPSERT into
//     plate_watchlist when severity >= 2.
//  4. Emitting an AlertCreated event when an alert is genuinely new
//     or strictly upgraded -- never on routine re-confirmations.
//
// The worker is driven by EncountersUpdated events from the
// aggregator (worker.EncountersUpdated). Each event carries the set
// of plate_hashes whose encounters the aggregator just rewrote; we
// re-score only those plates so a single route completion does not
// fan out into rescoring the entire encounter table.
package heuristic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// AlertCreated is the notification event emitted when the heuristic
// produces a NEW alert or strictly upgrades an existing one. Other
// subsystems (push notifications, dashboard alert badges) listen for
// these to surface the alert promptly without polling. It is NOT
// emitted on every re-evaluation -- a routine recompute that confirms
// an already-known alert is intentionally silent.
type AlertCreated struct {
	PlateHash []byte
	Severity  int
	// Route is the route id that triggered this evaluation. Useful
	// in the notification UI ("alert raised after route X").
	Route string
	// DongleID is the device the encounter belonged to. Tagged on
	// the event so multi-device installs can route notifications
	// correctly.
	DongleID string
	// ComputedAt is the wall time at which the heuristic decided to
	// emit the event. Set by the worker via its Now() hook so tests
	// can assert deterministically.
	ComputedAt time.Time
}

// AlertSuppressed is emitted when an operator action causes a
// previously-alerted plate to stop being alert-worthy. The current
// trigger is whitelisting a plate that had `kind='alerted'` -- the
// watchlist API needs to wake up notification subsystems so a
// dashboard alert badge counter can decrement promptly without
// polling. PriorSeverity carries the severity the watchlist row had
// before the suppression so consumers that group counts by severity
// can update the right bucket.
type AlertSuppressed struct {
	PlateHash []byte
	// PriorSeverity is the watchlist row's severity column at the
	// moment of suppression (0 if the column was NULL -- shouldn't
	// happen for kind='alerted' but defended against).
	PriorSeverity int
	// SuppressedAt is the wall time at which the suppression was
	// recorded. Set by the producer (typically the API handler) so
	// tests can pin time deterministically.
	SuppressedAt time.Time
}

// HeuristicMetrics is the subset of *metrics.Metrics this worker
// uses. Defined as an interface so tests can pass nil (the metrics
// struct is already a no-op when nil) or supply a fake.
//
// All current observability for the heuristic flows through the
// alert_events table (severity, components, heuristic_version), so
// the metrics surface is intentionally minimal -- just the per-
// evaluation duration and a per-severity counter.
type HeuristicMetrics interface {
	ObserveALPRHeuristicEval(d time.Duration)
	IncALPRHeuristicAlerts(severity int)
}

// HeuristicQuerier is the subset of *db.Queries the worker uses.
// Carved out so tests can supply an in-memory fake without a real
// Postgres connection.
type HeuristicQuerier interface {
	ListEncountersForPlateInWindowWithStartGPS(ctx context.Context, arg db.ListEncountersForPlateInWindowWithStartGPSParams) ([]db.ListEncountersForPlateInWindowWithStartGPSRow, error)
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
	UpsertWatchlistAlerted(ctx context.Context, arg db.UpsertWatchlistAlertedParams) (db.UpsertWatchlistAlertedRow, error)
	UpsertWatchlistAlertedPreserveAck(ctx context.Context, arg db.UpsertWatchlistAlertedPreserveAckParams) (db.UpsertWatchlistAlertedPreserveAckRow, error)
	InsertAlertEvent(ctx context.Context, arg db.InsertAlertEventParams) (db.PlateAlertEvent, error)
}

// EncountersUpdatedEvent is the local mirror of
// worker.EncountersUpdated. The worker package imports this package
// (cmd/server constructs the dependency graph), so to avoid a circular
// import the heuristic worker takes its input as a generic struct
// shape rather than a worker.EncountersUpdated. cmd/server adapts.
type EncountersUpdatedEvent struct {
	DongleID            string
	Route               string
	PlateHashesAffected [][]byte
}

// Worker is the long-running heuristic worker. Construct via
// NewWorker and drive with Run.
type Worker struct {
	// Updates is the input channel produced by the encounter
	// aggregator. Required.
	Updates <-chan EncountersUpdatedEvent

	// Queries is the sqlc-generated handle. Required at run time.
	Queries HeuristicQuerier

	// Settings is the runtime tunables store. May be nil; in that
	// case Thresholds fall back to DefaultThresholds and the master
	// alpr_enabled flag is treated as false (events are dropped).
	Settings *settings.Store

	// Metrics receives per-evaluation observations. Nil-safe.
	Metrics HeuristicMetrics

	// Alerts is the channel onto which AlertCreated events are
	// published when severity is genuinely new or upgraded. Nil-safe.
	Alerts chan<- AlertCreated

	// Concurrency caps the number of concurrent plate evaluations.
	// Defaults to 1.
	Concurrency int

	// Now is a clock hook so tests can pin wall time. Defaults to
	// time.Now.
	Now func() time.Time

	// alprEnabledForTest is a test-only override; when non-nil,
	// alprEnabled returns its dereferenced value instead of reading
	// settings.
	alprEnabledForTest *bool
}

// NewWorker wires defaults but does not start the worker.
func NewWorker(updates <-chan EncountersUpdatedEvent, q HeuristicQuerier, s *settings.Store, m HeuristicMetrics, alerts chan<- AlertCreated) *Worker {
	return &Worker{
		Updates:     updates,
		Queries:     q,
		Settings:    s,
		Metrics:     m,
		Alerts:      alerts,
		Concurrency: 1,
		Now:         time.Now,
	}
}

// Run drives the worker until ctx is cancelled or Updates is closed.
// Errors per-plate are logged and never crash the worker; transient
// DB failures on one plate do not stop the next event from being
// processed.
func (w *Worker) Run(ctx context.Context) {
	if w.Updates == nil {
		log.Printf("alpr heuristic: no input channel configured; worker will idle")
		<-ctx.Done()
		return
	}
	if w.Queries == nil {
		log.Printf("alpr heuristic: queries not configured; worker will idle")
		w.drain(ctx)
		return
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	concurrency := w.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case ev, ok := <-w.Updates:
			if !ok {
				wg.Wait()
				return
			}
			if !w.alprEnabled(ctx) {
				log.Printf("alpr heuristic: alpr_enabled=false; dropping event for %s/%s", ev.DongleID, ev.Route)
				continue
			}
			for _, ph := range ev.PlateHashesAffected {
				select {
				case <-ctx.Done():
					wg.Wait()
					return
				case sem <- struct{}{}:
				}
				wg.Add(1)
				plate := append([]byte(nil), ph...)
				go func(plateHash []byte, route, dongle string) {
					defer wg.Done()
					defer func() { <-sem }()
					if err := w.EvaluatePlate(ctx, plateHash, route, dongle); err != nil {
						log.Printf("alpr heuristic: %x in %s/%s: %v", plateHash, dongle, route, err)
					}
				}(plate, ev.Route, ev.DongleID)
			}
		}
	}
}

// drain pulls events off the input channel and discards them. Used
// when the worker is mis-configured so the producer's send-side does
// not block forever on a non-cancelled context.
func (w *Worker) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-w.Updates:
			if !ok {
				return
			}
		}
	}
}

// DryRunResult is the outcome of EvaluatePlateDryRun: the severity
// the plate would land at under the current settings, plus the
// existing watchlist severity for diff'ing. The dry-run path is the
// preview surface behind POST /v1/alpr/heuristic/reevaluate?dry_run=true;
// callers iterate the affected plates, tally results into bucket counts,
// and return the before/after histogram to the operator.
type DryRunResult struct {
	// PriorSeverity is the watchlist row's severity before the
	// dry-run, or 0 when no watchlist row exists yet.
	PriorSeverity int
	// ProposedSeverity is the severity Score returned for the plate
	// under the active Thresholds. Whitelisted plates always
	// proposed-score as 0 (matching the live worker's whitelist
	// override).
	ProposedSeverity int
	// Whitelisted reports whether the plate is currently
	// whitelisted, surfaced separately so the UI can label
	// "whitelisted" rows distinctly from "score below threshold".
	Whitelisted bool
}

// EvaluatePlateDryRun scores a single plate against the current
// settings without writing watchlist updates, alert_events, or
// emitting AlertCreated. Used by the re-evaluation endpoint's
// dry-run path so the operator can preview how a tuning change
// re-shapes the alert histogram.
func (w *Worker) EvaluatePlateDryRun(ctx context.Context, plateHash []byte) (DryRunResult, error) {
	if w.Queries == nil {
		return DryRunResult{}, errors.New("queries not configured")
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	now := w.Now()
	thresholds := w.thresholds(ctx)
	lookbackDays := w.lookbackDays(ctx)
	windowStart := now.Add(-time.Duration(lookbackDays) * 24 * time.Hour)
	rows, err := w.Queries.ListEncountersForPlateInWindowWithStartGPS(ctx, db.ListEncountersForPlateInWindowWithStartGPSParams{
		PlateHash:   plateHash,
		LastSeenTs:  pgtype.Timestamptz{Time: windowStart, Valid: true},
		FirstSeenTs: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return DryRunResult{}, fmt.Errorf("list encounters: %w", err)
	}
	encs := make([]Encounter, 0, len(rows))
	for _, r := range rows {
		encs = append(encs, encounterFromRow(r))
	}
	var (
		priorSeverity int
		whitelisted   bool
	)
	wl, err := w.Queries.GetWatchlistByHash(ctx, plateHash)
	switch {
	case err == nil:
		whitelisted = wl.Kind == "whitelist"
		if wl.Severity.Valid {
			priorSeverity = int(wl.Severity.Int16)
		}
	case errors.Is(err, pgx.ErrNoRows):
		// no prior row.
	default:
		return DryRunResult{}, fmt.Errorf("get watchlist: %w", err)
	}
	res := Score(ScoringInput{
		PlateHash:        plateHash,
		RecentEncounters: encs,
		Whitelisted:      whitelisted,
		Thresholds:       thresholds,
	})
	return DryRunResult{
		PriorSeverity:    priorSeverity,
		ProposedSeverity: res.Severity,
		Whitelisted:      whitelisted,
	}, nil
}

// EvaluatePlate is the per-plate work unit: load inputs, call Score,
// persist result, emit alert if appropriate. Driven internally by
// Run, exported so the heuristic re-evaluation API
// (POST /v1/alpr/heuristic/reevaluate) can synchronously rescore a
// plate without going through the EncountersUpdated channel. The
// route / dongleID arguments are tagged onto the alert_events row and
// any AlertCreated emitted; for ad-hoc re-runs (no causal route) the
// caller supplies an empty string and the audit trail records that
// the row was produced by a manual re-evaluation rather than a
// pipeline event.
func (w *Worker) EvaluatePlate(ctx context.Context, plateHash []byte, route, dongleID string) error {
	if w.Queries == nil {
		return errors.New("queries not configured")
	}
	now := w.Now()
	start := time.Now()
	defer func() {
		if w.Metrics != nil {
			w.Metrics.ObserveALPRHeuristicEval(time.Since(start))
		}
	}()

	thresholds := w.thresholds(ctx)
	lookbackDays := w.lookbackDays(ctx)

	windowStart := now.Add(-time.Duration(lookbackDays) * 24 * time.Hour)
	rows, err := w.Queries.ListEncountersForPlateInWindowWithStartGPS(ctx, db.ListEncountersForPlateInWindowWithStartGPSParams{
		PlateHash:   plateHash,
		LastSeenTs:  pgtype.Timestamptz{Time: windowStart, Valid: true},
		FirstSeenTs: pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("list encounters: %w", err)
	}

	encs := make([]Encounter, 0, len(rows))
	for _, r := range rows {
		encs = append(encs, encounterFromRow(r))
	}

	// Whitelist lookup -- pgx.ErrNoRows means "no watchlist row at
	// all", which is the common case (a plate the heuristic has
	// never seen before). We resolve to "not whitelisted, no
	// previous severity".
	var (
		existingSeverity int16
		existingExists   bool
		whitelisted      bool
	)
	wl, err := w.Queries.GetWatchlistByHash(ctx, plateHash)
	switch {
	case err == nil:
		existingExists = true
		whitelisted = wl.Kind == "whitelist"
		if wl.Severity.Valid {
			existingSeverity = wl.Severity.Int16
		}
	case errors.Is(err, pgx.ErrNoRows):
		// no row -- proceed with defaults.
	default:
		return fmt.Errorf("get watchlist: %w", err)
	}

	in := ScoringInput{
		PlateHash:        plateHash,
		RecentEncounters: encs,
		Whitelisted:      whitelisted,
		Thresholds:       thresholds,
	}
	res := Score(in)

	// Always insert an alert_events row. The audit trail is the
	// load-bearing artefact behind every alert badge.
	componentsJSON, err := json.Marshal(res.Components)
	if err != nil {
		// json.Marshal on a Component slice should never fail
		// (Evidence is map[string]any with primitive values), but
		// if it does we still want to know without poisoning the
		// pipeline.
		log.Printf("alpr heuristic: marshal components: %v", err)
		componentsJSON = []byte("[]")
	}
	if _, err := w.Queries.InsertAlertEvent(ctx, db.InsertAlertEventParams{
		PlateHash:        plateHash,
		Route:            pgtype.Text{String: route, Valid: route != ""},
		DongleID:         pgtype.Text{String: dongleID, Valid: dongleID != ""},
		Severity:         int16(res.Severity),
		Components:       componentsJSON,
		HeuristicVersion: HeuristicVersion,
	}); err != nil {
		return fmt.Errorf("insert alert event: %w", err)
	}

	if w.Metrics != nil {
		w.Metrics.IncALPRHeuristicAlerts(res.Severity)
	}

	// Severity 0/1 -- nothing more to do (the audit row above is
	// the entire artefact). Severity 1 is reserved for manual
	// 'note' rows from the UI; the heuristic intentionally never
	// emits it so an existing severity-1 row should never be
	// touched here.
	if res.Severity < 2 {
		return nil
	}

	severityStrictlyIncreased := res.Severity > int(existingSeverity)
	alertAt := pgtype.Timestamptz{Time: now, Valid: true}
	if severityStrictlyIncreased {
		// Strict upgrade: clear acked_at so the user re-sees the
		// alert at its new severity. UpsertWatchlistAlerted is the
		// existing query that clears ack on update.
		if _, err := w.Queries.UpsertWatchlistAlerted(ctx, db.UpsertWatchlistAlertedParams{
			PlateHash: plateHash,
			Severity:  pgtype.Int2{Int16: int16(res.Severity), Valid: true},
			AlertAt:   alertAt,
		}); err != nil {
			return fmt.Errorf("upsert watchlist (upgrade): %w", err)
		}
	} else {
		// Re-evaluation at same-or-lower severity: preserve ack.
		// The UPSERT itself uses GREATEST() so a lower computed
		// severity never demotes the row -- only the ack-clearing
		// behaviour differs from the upgrade path.
		if _, err := w.Queries.UpsertWatchlistAlertedPreserveAck(ctx, db.UpsertWatchlistAlertedPreserveAckParams{
			PlateHash: plateHash,
			Severity:  pgtype.Int2{Int16: int16(res.Severity), Valid: true},
			AlertAt:   alertAt,
		}); err != nil {
			return fmt.Errorf("upsert watchlist (preserve ack): %w", err)
		}
	}

	// Emit AlertCreated only when the alert is genuinely new (no
	// prior watchlist row) or strictly upgraded. Confirmations of
	// an existing alert at the same severity stay silent.
	shouldEmit := !existingExists || severityStrictlyIncreased
	if shouldEmit {
		w.emit(ctx, AlertCreated{
			PlateHash:  append([]byte(nil), plateHash...),
			Severity:   res.Severity,
			Route:      route,
			DongleID:   dongleID,
			ComputedAt: now,
		})
	}
	return nil
}

// emit publishes an AlertCreated event without blocking. The channel
// is buffered at the wiring layer; if the consumer is slow we log and
// drop rather than stall the heuristic.
func (w *Worker) emit(ctx context.Context, ev AlertCreated) {
	if w.Alerts == nil {
		return
	}
	select {
	case w.Alerts <- ev:
	case <-ctx.Done():
	default:
		log.Printf("alpr heuristic: alerts channel full; dropping AlertCreated for %x severity=%d",
			ev.PlateHash, ev.Severity)
	}
}

// encounterFromRow translates a sqlc row into the heuristic's
// Encounter shape. NULL gps fields fall through as HasGPS=false.
func encounterFromRow(r db.ListEncountersForPlateInWindowWithStartGPSRow) Encounter {
	e := Encounter{
		EncounterID: r.ID,
		Route:       r.Route,
		TurnCount:   int(r.TurnCount),
	}
	if r.FirstSeenTs.Valid {
		e.FirstSeen = r.FirstSeenTs.Time
	}
	if r.LastSeenTs.Valid {
		e.LastSeen = r.LastSeenTs.Time
	}
	if r.StartLat.Valid && r.StartLng.Valid {
		e.StartLat = r.StartLat.Float64
		e.StartLng = r.StartLng.Float64
		e.HasGPS = true
	}
	return e
}

// alprEnabled mirrors the same-named helpers on the extractor and
// aggregator. Falls back to false when the settings store is nil or
// unreadable -- the safe default for an opt-in feature.
func (w *Worker) alprEnabled(ctx context.Context) bool {
	if w.alprEnabledForTest != nil {
		return *w.alprEnabledForTest
	}
	if w.Settings == nil {
		return false
	}
	v, err := w.Settings.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		log.Printf("alpr heuristic: read alpr_enabled: %v", err)
		return false
	}
	return v
}

// thresholds reads every alpr_heuristic_* setting and falls back to
// DefaultThresholds() for any missing or unparseable value. The
// precedence (settings > default) is enforced by the *Or() helpers in
// the settings package.
func (w *Worker) thresholds(ctx context.Context) Thresholds {
	t := DefaultThresholds()
	if w.Settings == nil {
		return t
	}
	t.TurnsMin = w.intOr(ctx, settings.KeyALPRHeuristicTurnsMin, t.TurnsMin)
	t.TurnsPointsCap = w.floatOr(ctx, settings.KeyALPRHeuristicTurnsPointsCap, t.TurnsPointsCap)
	t.PersistenceMinutesMid = w.floatOr(ctx, settings.KeyALPRHeuristicPersistenceMinutesMid, t.PersistenceMinutesMid)
	t.PersistenceMinutesHigh = w.floatOr(ctx, settings.KeyALPRHeuristicPersistenceMinutesHigh, t.PersistenceMinutesHigh)
	t.PersistenceMidPoints = w.floatOr(ctx, settings.KeyALPRHeuristicPersistenceMidPoints, t.PersistenceMidPoints)
	t.PersistenceHighPoints = w.floatOr(ctx, settings.KeyALPRHeuristicPersistenceHighPoints, t.PersistenceHighPoints)
	t.DistinctRoutesMid = w.intOr(ctx, settings.KeyALPRHeuristicDistinctRoutesMid, t.DistinctRoutesMid)
	t.DistinctRoutesMidPoints = w.floatOr(ctx, settings.KeyALPRHeuristicDistinctRoutesMidPoints, t.DistinctRoutesMidPoints)
	t.DistinctRoutesHigh = w.intOr(ctx, settings.KeyALPRHeuristicDistinctRoutesHigh, t.DistinctRoutesHigh)
	t.DistinctRoutesHighPoints = w.floatOr(ctx, settings.KeyALPRHeuristicDistinctRoutesHighPoints, t.DistinctRoutesHighPoints)
	t.DistinctAreasMin = w.intOr(ctx, settings.KeyALPRHeuristicDistinctAreasMin, t.DistinctAreasMin)
	t.DistinctAreasPoints = w.floatOr(ctx, settings.KeyALPRHeuristicDistinctAreasPoints, t.DistinctAreasPoints)
	t.AreaCellKm = w.floatOr(ctx, settings.KeyALPRHeuristicAreaCellKm, t.AreaCellKm)
	t.TimingWindowHours = w.floatOr(ctx, settings.KeyALPRHeuristicTimingWindowHours, t.TimingWindowHours)
	t.TimingPoints = w.floatOr(ctx, settings.KeyALPRHeuristicTimingPoints, t.TimingPoints)
	t.SeverityBuckets[0] = w.floatOr(ctx, settings.KeyALPRHeuristicSeverityBucketSev2, t.SeverityBuckets[0])
	t.SeverityBuckets[1] = w.floatOr(ctx, settings.KeyALPRHeuristicSeverityBucketSev3, t.SeverityBuckets[1])
	t.SeverityBuckets[2] = w.floatOr(ctx, settings.KeyALPRHeuristicSeverityBucketSev4, t.SeverityBuckets[2])
	t.SeverityBuckets[3] = w.floatOr(ctx, settings.KeyALPRHeuristicSeverityBucketSev5, t.SeverityBuckets[3])
	return t
}

// lookbackDays reads the configured lookback or returns the default.
// Negative or zero values fall back to the default so a misconfigured
// row does not turn off all history.
func (w *Worker) lookbackDays(ctx context.Context) int {
	if w.Settings == nil {
		return DefaultLookbackDays
	}
	v, err := w.Settings.IntOr(ctx, settings.KeyALPRHeuristicLookbackDays, DefaultLookbackDays)
	if err != nil || v <= 0 {
		return DefaultLookbackDays
	}
	return v
}

// intOr reads an int setting with a default. Logs (and returns def)
// on unexpected DB errors; ErrNotFound is silently absorbed by the
// settings.IntOr helper.
func (w *Worker) intOr(ctx context.Context, key string, def int) int {
	v, err := w.Settings.IntOr(ctx, key, def)
	if err != nil && !errors.Is(err, settings.ErrNotFound) {
		log.Printf("alpr heuristic: read %s: %v", key, err)
		return def
	}
	return v
}

// floatOr reads a float setting with a default. Same error-handling
// shape as intOr.
func (w *Worker) floatOr(ctx context.Context, key string, def float64) float64 {
	v, err := w.Settings.FloatOr(ctx, key, def)
	if err != nil && !errors.Is(err, settings.ErrNotFound) {
		log.Printf("alpr heuristic: read %s: %v", key, err)
		return def
	}
	return v
}
