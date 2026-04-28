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
//
// Two flavours, distinguished by which field is set:
//
//   - Plate alert: PlateHash is set, SignatureID is nil. The
//     stalking heuristic raised an alert keyed on a single plate.
//   - Signature-swap alert: SignatureID is set (non-nil), PlateHash
//     is empty. The fusion layer detected a single physical vehicle
//     (by signature) operating under multiple plates in the same
//     area. Notification channels (push, badge) branch on which
//     field is non-nil.
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
	// SignatureID is non-nil when the event is a signature-swap
	// alert produced by the fusion layer. The notification UI uses
	// this to render "vehicle X has been seen with N plates" instead
	// of the per-plate badge. PlateHash is empty in this mode.
	SignatureID *int64
	// PlateHashes is the chain of distinct plate hashes that
	// contributed to a signature-swap alert. Empty for plate-keyed
	// alerts. The UI uses it to render the "evidence chain" inside
	// the alert detail view.
	PlateHashes [][]byte
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
//
// The fusion-layer methods (CountDetectionsBySignatureForPlate,
// GetSignature, ListPlatesForSignature, ListPlateHashesForSignatureInWindow,
// GetWatchlistSignatureSwap, UpsertWatchlistSignatureSwap) ride on the
// same interface so callers do not have to thread two queriers
// through. All are no-op safe: if the install lacks signature data
// the underlying queries return zero rows and FuseSignatures bails
// out early.
type HeuristicQuerier interface {
	ListEncountersForPlateInWindowWithStartGPS(ctx context.Context, arg db.ListEncountersForPlateInWindowWithStartGPSParams) ([]db.ListEncountersForPlateInWindowWithStartGPSRow, error)
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
	UpsertWatchlistAlerted(ctx context.Context, arg db.UpsertWatchlistAlertedParams) (db.UpsertWatchlistAlertedRow, error)
	UpsertWatchlistAlertedPreserveAck(ctx context.Context, arg db.UpsertWatchlistAlertedPreserveAckParams) (db.UpsertWatchlistAlertedPreserveAckRow, error)
	InsertAlertEvent(ctx context.Context, arg db.InsertAlertEventParams) (db.PlateAlertEvent, error)

	// Fusion-layer reads.
	CountDetectionsBySignatureForPlate(ctx context.Context, plateHash []byte) ([]db.CountDetectionsBySignatureForPlateRow, error)
	GetSignature(ctx context.Context, id int64) (db.VehicleSignature, error)
	ListPlatesForSignature(ctx context.Context, signatureID pgtype.Int8) ([][]byte, error)
	ListPlateHashesForSignatureInWindow(ctx context.Context, arg db.ListPlateHashesForSignatureInWindowParams) ([]db.ListPlateHashesForSignatureInWindowRow, error)

	// Fusion-layer writes (signature-keyed plate-swap alerts).
	GetWatchlistSignatureSwap(ctx context.Context, signatureID pgtype.Int8) (db.PlateWatchlist, error)
	UpsertWatchlistSignatureSwap(ctx context.Context, arg db.UpsertWatchlistSignatureSwapParams) (db.PlateWatchlist, error)
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
					if err := w.evaluatePlate(ctx, plateHash, route, dongle); err != nil {
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

// evaluatePlate is the per-plate work unit: load inputs, call Score,
// persist result, emit alert if appropriate. Public via Run; exposed
// here for direct invocation by tests + an admin API in a follow-up.
func (w *Worker) evaluatePlate(ctx context.Context, plateHash []byte, route, dongleID string) error {
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

	// Fusion layer: corroboration + plate-swap detection. Strictly
	// additive on top of the stalking heuristic. If the install has
	// no signature-bearing detections (vehicle-attributes engine off
	// or unsupported), this returns zero/empty and the rest of the
	// pipeline behaves identically to a pre-fusion build.
	//
	// Whitelist suppression at the fusion layer mirrors the stalking
	// layer: a whitelisted plate contributes neither severity bumps
	// nor plate-swap alerts. The signature-keyed alert path applies
	// its own independent whitelist check (every contributing plate
	// must be non-whitelisted) inside FuseSignatures.
	var fusion FusionResult
	if !whitelisted {
		fusion, err = FuseSignatures(ctx, w.Queries, FusionInput{
			PlateHash:  plateHash,
			Now:        now,
			Thresholds: w.fusionThresholds(ctx),
		})
		if err != nil {
			// A fusion-layer failure is not fatal -- the stalking
			// heuristic's score is the load-bearing artefact, so we
			// log and continue. The plate's audit row records what
			// fusion contributed (zero, in this case).
			log.Printf("alpr heuristic: fuse signatures for %x: %v", plateHash, err)
			fusion = FusionResult{}
		}
	}

	// Merge fusion components into the audit blob and apply the
	// fusion severity bump. The bump is added at the score-points
	// level (matching how stalking components contribute) so the
	// severityForScore mapping stays the single source of truth.
	combinedComponents := append([]Component(nil), res.Components...)
	combinedComponents = append(combinedComponents, fusion.Components...)
	combinedScore := res.TotalScore + fusion.ExtraSeverity
	combinedSeverity := severityForScore(combinedScore)
	// Whitelist override still wins -- if the plate was whitelisted
	// the stalking layer already zeroed res.TotalScore and emitted
	// the suppression component. The fusion layer is gated on
	// !whitelisted above; this assertion is defensive.
	if whitelisted {
		combinedScore = 0
		combinedSeverity = 0
	}

	// Always insert an alert_events row. The audit trail is the
	// load-bearing artefact behind every alert badge.
	componentsJSON, err := json.Marshal(combinedComponents)
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
		Severity:         int16(combinedSeverity),
		Components:       componentsJSON,
		HeuristicVersion: HeuristicVersion,
	}); err != nil {
		return fmt.Errorf("insert alert event: %w", err)
	}

	if w.Metrics != nil {
		w.Metrics.IncALPRHeuristicAlerts(combinedSeverity)
	}

	// Process plate-swap alerts independently of the plate-keyed
	// path. A signature-swap alert can fire on a plate whose own
	// stalking severity is below threshold (e.g. the operator only
	// just saw plate #3 of a swap), and a plate's own severity can
	// rise without any swap happening. Errors per-alert are logged
	// and do not stop the plate's primary watchlist write below.
	for _, sa := range fusion.PlateSwapAlerts {
		if err := w.persistSwapAlert(ctx, sa, route, dongleID, now); err != nil {
			log.Printf("alpr heuristic: persist swap alert (sig=%d): %v", sa.SignatureID, err)
		}
	}

	// Override res.Severity with the combined value so the rest of
	// the worker (watchlist UPSERT + AlertCreated emission) reasons
	// about the fully-fused severity.
	res.Severity = combinedSeverity
	res.TotalScore = combinedScore

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

// fusionThresholds reads the alpr_signature_* settings and falls back
// to DefaultFusionThresholds() for any missing or unparseable value.
// The fusion layer's tunables intentionally live behind a separate
// settings prefix so a misconfigured fusion threshold does not bleed
// into the stalking heuristic.
func (w *Worker) fusionThresholds(ctx context.Context) FusionThresholds {
	t := DefaultFusionThresholds()
	if w.Settings == nil {
		return t
	}
	t.ConsistencyShareMin = w.floatOr(ctx, settings.KeyALPRSignatureConsistencyShareMin, t.ConsistencyShareMin)
	t.ConsistencyPoints = w.floatOr(ctx, settings.KeyALPRSignatureConsistencyPoints, t.ConsistencyPoints)
	t.ConflictShareMin = w.floatOr(ctx, settings.KeyALPRSignatureConflictShareMin, t.ConflictShareMin)
	t.ConflictMinSignatures = w.intOr(ctx, settings.KeyALPRSignatureConflictMinSignatures, t.ConflictMinSignatures)
	t.PlateSwapMinPlates = w.intOr(ctx, settings.KeyALPRSignatureSwapMinPlates, t.PlateSwapMinPlates)
	t.PlateSwapAreaCellKm = w.floatOr(ctx, settings.KeyALPRSignatureSwapAreaCellKm, t.PlateSwapAreaCellKm)
	t.PlateSwapLookbackDays = w.intOr(ctx, settings.KeyALPRSignatureSwapLookbackDays, t.PlateSwapLookbackDays)
	t.PlateSwapSeverity = w.intOr(ctx, settings.KeyALPRSignatureSwapSeverity, t.PlateSwapSeverity)
	return t
}

// persistSwapAlert UPSERTs the signature-keyed plate-swap row and
// emits an AlertCreated when the alert is genuinely new or strictly
// upgraded. Mirrors the plate-keyed write path in evaluatePlate so
// the two flavours have parallel ack-preserve / upgrade-clear
// semantics; the only behavioural difference is keying on
// signature_id with plate_hash IS NULL.
func (w *Worker) persistSwapAlert(ctx context.Context, sa SwapAlert, route, dongleID string, now time.Time) error {
	if w.Queries == nil {
		return errors.New("queries not configured")
	}
	sigParam := pgtype.Int8{Int64: sa.SignatureID, Valid: true}

	// Look up the existing signature-keyed row (if any) so we can
	// decide whether this is a brand-new alert (emit AlertCreated)
	// or a re-fire (silently refresh last_alert_at).
	var (
		existingSeverity int16
		existingExists   bool
	)
	prior, err := w.Queries.GetWatchlistSignatureSwap(ctx, sigParam)
	switch {
	case err == nil:
		existingExists = true
		if prior.Severity.Valid {
			existingSeverity = prior.Severity.Int16
		}
	case errors.Is(err, pgx.ErrNoRows):
		// no existing row -- the alert is new.
	default:
		return fmt.Errorf("get signature-swap watchlist: %w", err)
	}

	alertAt := pgtype.Timestamptz{Time: now, Valid: true}
	if _, err := w.Queries.UpsertWatchlistSignatureSwap(ctx, db.UpsertWatchlistSignatureSwapParams{
		SignatureID: sigParam,
		Severity:    pgtype.Int2{Int16: int16(sa.Severity), Valid: true},
		AlertAt:     alertAt,
	}); err != nil {
		return fmt.Errorf("upsert signature-swap watchlist: %w", err)
	}

	// Emit AlertCreated only when the alert is genuinely new or its
	// severity strictly increased. A re-fire of the same signature
	// at the same severity is intentionally silent.
	severityStrictlyIncreased := sa.Severity > int(existingSeverity)
	shouldEmit := !existingExists || severityStrictlyIncreased
	if !shouldEmit {
		return nil
	}
	sigID := sa.SignatureID
	plateCopies := make([][]byte, len(sa.PlateHashes))
	for i, p := range sa.PlateHashes {
		plateCopies[i] = append([]byte(nil), p...)
	}
	w.emit(ctx, AlertCreated{
		Severity:    sa.Severity,
		Route:       route,
		DongleID:    dongleID,
		ComputedAt:  now,
		SignatureID: &sigID,
		PlateHashes: plateCopies,
	})
	return nil
}
