// Package worker -- alpr_aggregator.go consumes RouteAlprDetectionsComplete
// events from alpr_detector.go and collapses each route's per-frame
// plate_detections rows into per-encounter plate_encounters rows.
//
// # Why an aggregation step
//
// A plate that stays in frame for a 30-second car-following window is
// recorded as ~30 raw detections (at 1 fps with the default sampling
// rate). The downstream stalking heuristic and the review UI both want
// "the plate was seen N times" rather than "we have 30 frames of the
// same plate," so the aggregator groups consecutive detections of the
// same plate within a single route into encounters. A new encounter
// begins only when the gap to the prior detection exceeds the
// configured threshold (default 60 s) -- in practice the lead car
// turning, accelerating away, or being blocked by another vehicle.
//
// # Idempotency
//
// Each route's aggregation runs inside a single transaction:
// DELETE FROM plate_encounters WHERE dongle_id=? AND route=? followed
// by INSERT for each computed encounter. The unique constraint on
// (dongle_id, route, plate_hash, first_seen_ts) defends against double
// inserts even outside the transaction. Re-running on the same route
// (after a manual correction or threshold tuning) replaces the prior
// encounters wholesale.
//
// # Race with turn-detector
//
// turn_count is computed with CountTurnsInWindow, which returns 0 when
// no rows exist for the route -- that is the correct fallback when the
// turn-detector hasn't yet processed the route. The race is benign: if
// turn-detector lands later, an operator can re-trigger aggregation
// (manual correction API, future re-evaluation endpoint) and the
// encounter rows are recomputed with the now-correct turn counts. The
// transaction-level idempotency makes this safe.
//
// # Toggle responsiveness
//
// The worker subscribes to RouteAlprDetectionsComplete; events that
// arrive while alpr_enabled is false are dropped. We never aggregate
// stale data after the operator has disabled ALPR.
//
// # Concurrency
//
// Routes are processed via a semaphore-bounded goroutine pool. Default
// concurrency is 1 (set ALPR_AGGREGATOR_CONCURRENCY to override) so a
// single-replica deployment cannot accidentally interleave per-route
// transactions. The semaphore is independent of the upstream detector's
// concurrency: encounter aggregation is light (one DB read, sorted
// in-memory walk, one tx), so the bottleneck is the detector and the
// aggregator's parallelism is mostly a knob for operators with parallel
// routes from a fleet of devices.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// Defaults for the encounter aggregator. Exposed (where worth it) so
// the wiring layer in cmd/server can populate the worker without
// reaching into unexported fields.
const (
	// DefaultALPRAggregatorConcurrency is the number of routes that
	// can be aggregated in parallel. The default of 1 matches the
	// "process at most one route at a time" acceptance criterion;
	// operators with parallel routes from a fleet of devices can raise
	// it via ALPR_AGGREGATOR_CONCURRENCY.
	DefaultALPRAggregatorConcurrency = 1

	// DefaultALPREncounterGapSeconds is the maximum allowed gap (in
	// seconds) between consecutive detections within one encounter.
	// 60 s matches the spec: a detection >60 s after the prior one
	// means "they came back," which is a separate encounter.
	DefaultALPREncounterGapSeconds = 60.0

	// minALPREncounterGapSeconds clamps misconfigured runtime values.
	// A zero or negative gap would collapse every detection into its
	// own encounter (the inverse of the intended behaviour); 1 s is the
	// smallest value that still has meaningful semantics at our
	// sampling rates.
	minALPREncounterGapSeconds = 1.0
)

// EncountersUpdated is the event the aggregator emits after a route's
// encounter rows have been (re)written. The downstream alpr-stalking-
// heuristic listens for this so it can run only on plates whose state
// actually changed, rather than rescanning the entire encounter table
// on a periodic timer.
type EncountersUpdated struct {
	DongleID            string
	Route               string
	PlateHashesAffected [][]byte
}

// ALPRAggregatorMetrics is the subset of *metrics.Metrics this worker
// uses. Defined as an interface so tests can pass nil (a nil *Metrics
// is already a no-op) or a fake without spinning up a Prometheus
// registry.
type ALPRAggregatorMetrics interface {
	AddALPREncounters(n int)
	ObserveALPREncounterCompute(d time.Duration)
	ObserveALPREncountersPerRoute(n int)
}

// ALPRAggregatorQuerier is the subset of *db.Queries the aggregator
// uses. Carved out so tests can supply an in-memory fake without a real
// Postgres connection. WithTxQuerier wraps q.WithTx(tx) for the
// per-route transaction; the fake typically returns itself because its
// in-memory state already participates in the test's notion of a
// transaction.
type ALPRAggregatorQuerier interface {
	ListDetectionsForRoute(ctx context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error)
	CountTurnsInWindow(ctx context.Context, arg db.CountTurnsInWindowParams) (int64, error)
	DeleteEncountersForRoute(ctx context.Context, arg db.DeleteEncountersForRouteParams) (int64, error)
	UpsertEncounter(ctx context.Context, arg db.UpsertEncounterParams) (db.PlateEncounter, error)

	// WithTxQuerier returns a querier bound to the given pgx.Tx. The
	// production *db.Queries-backed adapter wraps q.WithTx(tx) so the
	// returned value still implements every method of this interface.
	WithTxQuerier(tx pgx.Tx) ALPRAggregatorQuerier
}

// pgxAggregatorQuerier wraps a *db.Queries so it can satisfy
// ALPRAggregatorQuerier. We need a wrapper for the same reason as the
// detector's pgxQuerier: WithTxQuerier returns the interface (so a fake
// can return itself) while *db.Queries.WithTx returns *db.Queries.
type pgxAggregatorQuerier struct {
	*db.Queries
}

// WrapPgxQueriesForAggregator adapts a *db.Queries to the aggregator's
// querier interface. Used at wiring time in cmd/server.
func WrapPgxQueriesForAggregator(q *db.Queries) ALPRAggregatorQuerier {
	return &pgxAggregatorQuerier{Queries: q}
}

// WithTxQuerier returns a pgxAggregatorQuerier whose embedded
// *db.Queries is bound to tx.
func (p *pgxAggregatorQuerier) WithTxQuerier(tx pgx.Tx) ALPRAggregatorQuerier {
	return &pgxAggregatorQuerier{Queries: p.Queries.WithTx(tx)}
}

// Compile-time assertion that the production wrapper satisfies the
// worker's querier contract.
var _ ALPRAggregatorQuerier = (*pgxAggregatorQuerier)(nil)

// ALPRAggregatorTxBeginner is the transactional pool interface.
// Production passes *pgxpool.Pool.
type ALPRAggregatorTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ALPRAggregator is the worker. Construct via NewALPRAggregator and
// drive with Run.
type ALPRAggregator struct {
	// Completions is the input channel produced by alpr_detector.go.
	// Required. The worker ranges over this channel; closure of the
	// channel terminates the worker.
	Completions <-chan RouteAlprDetectionsComplete

	// Queries is the sqlc-generated db handle wrapped via
	// WrapPgxQueriesForAggregator. Required at run time.
	Queries ALPRAggregatorQuerier

	// Pool is the pgx pool used for the per-route transaction.
	// Required.
	Pool ALPRAggregatorTxBeginner

	// Settings is the runtime tunables store. May be nil, in which
	// case alpr_enabled is treated as false (events are dropped) and
	// the gap falls back to DefaultEncounterGapSeconds.
	Settings *settings.Store

	// Metrics receives per-route counters and the compute-duration
	// histogram. Safe to leave nil; degrades to logs only.
	Metrics ALPRAggregatorMetrics

	// EncountersUpdated is the channel onto which the worker emits an
	// EncountersUpdated event once per processed route. May be nil; in
	// that case aggregation still runs but no event is emitted.
	EncountersUpdated chan<- EncountersUpdated

	// Concurrency is the size of the per-route semaphore. Defaults to
	// DefaultALPRAggregatorConcurrency when <= 0.
	Concurrency int

	// DefaultEncounterGapSeconds is the fallback gap threshold when
	// the runtime settings store has no override. Operators populate
	// this from ENCOUNTER_GAP_SECONDS at startup.
	DefaultEncounterGapSeconds float64

	// alprEnabledForTest is a test-only override for the master flag.
	// When non-nil, alprEnabled returns the dereferenced value
	// instead of consulting Settings.
	alprEnabledForTest *bool
}

// NewALPRAggregator wires defaults but does not start the worker.
// Completions, Queries, and Pool are required.
func NewALPRAggregator(
	completions <-chan RouteAlprDetectionsComplete,
	queries ALPRAggregatorQuerier,
	pool ALPRAggregatorTxBeginner,
	settingsStore *settings.Store,
	m ALPRAggregatorMetrics,
	updates chan<- EncountersUpdated,
) *ALPRAggregator {
	return &ALPRAggregator{
		Completions:                completions,
		Queries:                    queries,
		Pool:                       pool,
		Settings:                   settingsStore,
		Metrics:                    m,
		EncountersUpdated:          updates,
		Concurrency:                DefaultALPRAggregatorConcurrency,
		DefaultEncounterGapSeconds: DefaultALPREncounterGapSeconds,
	}
}

// Run drives the aggregator until ctx is cancelled or Completions is
// closed. Per-route errors are logged and never crash the worker; a
// transient DB failure on one route does not stop the next event from
// being processed.
func (w *ALPRAggregator) Run(ctx context.Context) {
	if w.Completions == nil {
		log.Printf("alpr aggregator: no input channel configured; worker will idle")
		<-ctx.Done()
		return
	}
	if w.Queries == nil || w.Pool == nil {
		log.Printf("alpr aggregator: queries or pool not configured; worker will idle")
		w.drain(ctx)
		return
	}

	concurrency := w.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultALPRAggregatorConcurrency
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case ev, ok := <-w.Completions:
			if !ok {
				wg.Wait()
				return
			}
			if !w.alprEnabled(ctx) {
				// Operator disabled ALPR while events were in flight;
				// drop the event so we don't aggregate stale data.
				log.Printf("alpr aggregator: alpr_enabled=false; dropping event for %s/%s", ev.DongleID, ev.Route)
				continue
			}
			select {
			case <-ctx.Done():
				wg.Wait()
				return
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func(event RouteAlprDetectionsComplete) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := w.processRoute(ctx, event); err != nil {
					log.Printf("alpr aggregator: route %s/%s: %v",
						event.DongleID, event.Route, err)
				}
			}(ev)
		}
	}
}

// drain pulls events off the input channel and discards them. Used
// when the worker is mis-configured so the producer's send-side does
// not block forever on a non-cancelled context.
func (w *ALPRAggregator) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-w.Completions:
			if !ok {
				return
			}
		}
	}
}

// processRoute aggregates one route's detections into encounters and
// persists them. Returns an error only on unrecoverable failure (DB
// outage, etc.); everything else (no detections, parse glitches,
// stale events) is logged and reported as success.
func (w *ALPRAggregator) processRoute(ctx context.Context, ev RouteAlprDetectionsComplete) error {
	start := time.Now()
	defer func() {
		if w.Metrics != nil {
			w.Metrics.ObserveALPREncounterCompute(time.Since(start))
		}
	}()

	gapSeconds := w.encounterGap(ctx)

	rows, err := w.Queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
		DongleID: ev.DongleID,
		Route:    ev.Route,
	})
	if err != nil {
		return fmt.Errorf("list detections: %w", err)
	}

	encounters := computeEncounters(rows, gapSeconds)
	plateHashes := uniquePlateHashes(encounters)

	// Per-encounter turn_count uses the existing CountTurnsInWindow
	// query; if the turn-detector has not yet populated route_turns,
	// the query returns 0 rows -> 0 turns, which is the correct
	// fallback. The race resolves on a future re-aggregation pass.
	for i := range encounters {
		n, err := w.Queries.CountTurnsInWindow(ctx, db.CountTurnsInWindowParams{
			DongleID:    ev.DongleID,
			Route:       ev.Route,
			WindowStart: encounters[i].FirstSeenTs,
			WindowEnd:   encounters[i].LastSeenTs,
		})
		if err != nil {
			return fmt.Errorf("count turns in window: %w", err)
		}
		// int32 cast is safe: a single route cannot plausibly contain
		// >2^31 turns; the upstream detector caps emissions per route.
		encounters[i].TurnCount = int32(n)
	}

	if err := w.persist(ctx, ev.DongleID, ev.Route, encounters); err != nil {
		return fmt.Errorf("persist encounters: %w", err)
	}

	if w.Metrics != nil {
		w.Metrics.AddALPREncounters(len(encounters))
		w.Metrics.ObserveALPREncountersPerRoute(len(encounters))
	}

	w.emit(ctx, EncountersUpdated{
		DongleID:            ev.DongleID,
		Route:               ev.Route,
		PlateHashesAffected: plateHashes,
	})
	return nil
}

// persist runs DeleteEncountersForRoute followed by an UpsertEncounter
// per computed encounter inside a single transaction. The unique
// constraint on (dongle_id, route, plate_hash, first_seen_ts) backs
// this up: a re-run that produces the same encounter set is a no-op
// result-wise.
//
// We use UpsertEncounter (rather than a pure INSERT) so a tx crash
// after DELETE but before commit -- which would roll back the DELETE
// and leave prior rows -- recovers gracefully on the next run. The
// DELETE-then-UPSERT pattern is also the same shape as
// turn_detector.persistTurns, so the worker family stays consistent.
func (w *ALPRAggregator) persist(ctx context.Context, dongleID, route string, encounters []encounter) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := w.Queries.WithTxQuerier(tx)

	if _, err := qtx.DeleteEncountersForRoute(ctx, db.DeleteEncountersForRouteParams{
		DongleID: dongleID,
		Route:    route,
	}); err != nil {
		return fmt.Errorf("delete prior encounters: %w", err)
	}

	for _, e := range encounters {
		if _, err := qtx.UpsertEncounter(ctx, db.UpsertEncounterParams{
			DongleID:              dongleID,
			Route:                 route,
			PlateHash:             e.PlateHash,
			FirstSeenTs:           e.FirstSeenTs,
			LastSeenTs:            e.LastSeenTs,
			DetectionCount:        e.DetectionCount,
			TurnCount:             e.TurnCount,
			MaxInternalGapSeconds: e.MaxInternalGapSeconds,
			SignatureID:           e.SignatureID,
			Status:                "open",
			BboxFirst:             e.BboxFirst,
			BboxLast:              e.BboxLast,
		}); err != nil {
			return fmt.Errorf("upsert encounter (plate_hash=%x first_seen_ts=%v): %w",
				e.PlateHash, e.FirstSeenTs.Time, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}

// emit sends an EncountersUpdated event without blocking. The channel
// is buffered at the wiring layer; if the consumer is slow we log and
// drop rather than stall the aggregator. The downstream stalking
// heuristic can fall back to its own periodic scan to catch missed
// events.
func (w *ALPRAggregator) emit(ctx context.Context, ev EncountersUpdated) {
	if w.EncountersUpdated == nil {
		return
	}
	select {
	case w.EncountersUpdated <- ev:
	case <-ctx.Done():
	default:
		log.Printf("alpr aggregator: encounters-updated channel full; dropping event for %s/%s (%d plates)",
			ev.DongleID, ev.Route, len(ev.PlateHashesAffected))
	}
}

// encounter is the in-memory representation of one aggregated
// encounter, ready to be written via UpsertEncounter. plateHashKey is
// a string view of PlateHash (Go maps cannot key on []byte directly).
type encounter struct {
	PlateHash             []byte
	FirstSeenTs           pgtype.Timestamptz
	LastSeenTs            pgtype.Timestamptz
	DetectionCount        int32
	TurnCount             int32
	MaxInternalGapSeconds int32
	SignatureID           pgtype.Int8
	BboxFirst             []byte
	BboxLast              []byte
}

// computeEncounters is the pure aggregation algorithm. Given a route's
// detections in arbitrary order plus the per-encounter gap threshold
// in seconds, return the encounters in chronological order grouped by
// plate.
//
// Algorithm:
//
//  1. Bucket detections by plate_hash (string view) preserving the row
//     pointer in each bucket.
//  2. Sort each bucket by frame_ts (then by id as a tiebreaker so
//     equal-timestamp inputs produce deterministic output).
//  3. Walk each bucket. The first row starts the current encounter;
//     each subsequent row either extends the current encounter (gap <=
//     threshold) or closes it and starts a new one.
//  4. Per encounter, compute signature_id as the mode of the per-row
//     signature_id (ignoring nulls). Tie-break by lowest signature_id
//     so re-runs are deterministic.
//  5. Concatenate all per-plate encounter lists, then re-sort by
//     first_seen_ts so the final list is chronologically ordered
//     across plates (UpsertEncounter does not require this, but
//     deterministic insertion order makes test assertions cleaner).
//
// Pure: no I/O, no allocation tied to the database driver. This is
// what the unit tests exercise.
func computeEncounters(rows []db.ListDetectionsForRouteRow, gapSeconds float64) []encounter {
	if len(rows) == 0 {
		return nil
	}
	if gapSeconds < minALPREncounterGapSeconds {
		gapSeconds = minALPREncounterGapSeconds
	}
	gap := time.Duration(gapSeconds * float64(time.Second))

	// Group rows by plate_hash. The string conversion of a []byte is
	// a Go-map-key idiom; the bytes are not aliased between map keys
	// and the rows themselves.
	byPlate := make(map[string][]db.ListDetectionsForRouteRow)
	for _, r := range rows {
		// Defensive: a detection without a frame_ts cannot be
		// localized in time and would corrupt the encounter window.
		// Drop rather than error -- the upstream detector should
		// never produce such a row, but if it does we don't want to
		// poison the entire route's aggregation.
		if !r.FrameTs.Valid {
			continue
		}
		k := string(r.PlateHash)
		byPlate[k] = append(byPlate[k], r)
	}

	var out []encounter
	for _, group := range byPlate {
		sort.Slice(group, func(i, j int) bool {
			ti := group[i].FrameTs.Time
			tj := group[j].FrameTs.Time
			if ti.Equal(tj) {
				return group[i].ID < group[j].ID
			}
			return ti.Before(tj)
		})
		out = append(out, encountersForPlateBucket(group, gap)...)
	}

	// Final ordering by first_seen_ts (then plate_hash for stable
	// output across runs).
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].FirstSeenTs.Time
		tj := out[j].FirstSeenTs.Time
		if ti.Equal(tj) {
			return string(out[i].PlateHash) < string(out[j].PlateHash)
		}
		return ti.Before(tj)
	})
	return out
}

// encountersForPlateBucket walks a single plate's chronologically
// sorted detections and produces the encounter list. See
// computeEncounters for the algorithm description.
func encountersForPlateBucket(group []db.ListDetectionsForRouteRow, gap time.Duration) []encounter {
	if len(group) == 0 {
		return nil
	}

	var out []encounter
	current := startEncounter(group[0])
	prevTs := group[0].FrameTs.Time

	for i := 1; i < len(group); i++ {
		row := group[i]
		ts := row.FrameTs.Time
		delta := ts.Sub(prevTs)
		if delta > gap {
			// Gap exceeded; close and emit the current encounter.
			out = append(out, current)
			current = startEncounter(row)
		} else {
			// Extend the current encounter.
			current.LastSeenTs = row.FrameTs
			current.DetectionCount++
			gapSec := int32(delta / time.Second)
			if gapSec > current.MaxInternalGapSeconds {
				current.MaxInternalGapSeconds = gapSec
			}
			current.BboxLast = append([]byte(nil), row.Bbox...)
			// signature accounting happens after the walk; defer
			// the per-row tally to chooseSignatureID.
		}
		prevTs = ts
	}
	out = append(out, current)

	// Stamp signature_id per encounter using the mode of the rows
	// that produced it. We cannot do this incrementally above
	// because closing-out an encounter requires the full count
	// across all of its rows; rather than carry a partial counter
	// per encounter, we re-walk each emitted encounter and tally.
	stampSignatureIDs(out, group, gap)
	return out
}

// startEncounter constructs a fresh encounter starting at row.
func startEncounter(row db.ListDetectionsForRouteRow) encounter {
	return encounter{
		PlateHash:             append([]byte(nil), row.PlateHash...),
		FirstSeenTs:           row.FrameTs,
		LastSeenTs:            row.FrameTs,
		DetectionCount:        1,
		TurnCount:             0,
		MaxInternalGapSeconds: 0,
		BboxFirst:             append([]byte(nil), row.Bbox...),
		BboxLast:              append([]byte(nil), row.Bbox...),
	}
}

// stampSignatureIDs computes signature_id for each encounter as the
// mode of its constituent rows' signature_id (ignoring nulls). Ties
// resolve to the lowest signature_id so re-runs are deterministic.
// Walks the (already sorted) group again, partitioning into the same
// encounters by gap so per-encounter tallies stay aligned.
func stampSignatureIDs(encounters []encounter, group []db.ListDetectionsForRouteRow, gap time.Duration) {
	if len(encounters) == 0 || len(group) == 0 {
		return
	}
	// Tally per encounter. encIdx tracks which encounter the current
	// row belongs to; advance when the gap to the prior row exceeds
	// the threshold (mirroring encountersForPlateBucket).
	encIdx := 0
	tallies := make([]map[int64]int, len(encounters))
	tallies[0] = make(map[int64]int)

	prevTs := group[0].FrameTs.Time
	if group[0].SignatureID.Valid {
		tallies[0][group[0].SignatureID.Int64]++
	}
	for i := 1; i < len(group); i++ {
		row := group[i]
		ts := row.FrameTs.Time
		if ts.Sub(prevTs) > gap {
			encIdx++
			if encIdx >= len(encounters) {
				// Defensive: tally bookkeeping disagrees with the
				// emitted-encounter list. Should be impossible
				// because both walks use the same group + gap; log
				// and stop tallying rather than write past the
				// slice.
				return
			}
			tallies[encIdx] = make(map[int64]int)
		}
		if row.SignatureID.Valid {
			tallies[encIdx][row.SignatureID.Int64]++
		}
		prevTs = ts
	}

	for i := range encounters {
		encounters[i].SignatureID = modeSignatureID(tallies[i])
	}
}

// modeSignatureID returns the mode of a tally; null when the tally is
// empty (no row had a signature). Ties resolve to the lowest
// signature_id so re-runs are deterministic.
func modeSignatureID(tally map[int64]int) pgtype.Int8 {
	if len(tally) == 0 {
		return pgtype.Int8{}
	}
	var bestID int64
	bestCount := -1
	for id, count := range tally {
		if count > bestCount || (count == bestCount && id < bestID) {
			bestCount = count
			bestID = id
		}
	}
	if bestCount <= 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: bestID, Valid: true}
}

// uniquePlateHashes returns the distinct plate hashes across the
// computed encounter list, used to populate
// EncountersUpdated.PlateHashesAffected. Sorted for deterministic
// output (helpful in tests; consumers that need a different ordering
// are responsible for re-sorting).
func uniquePlateHashes(encounters []encounter) [][]byte {
	if len(encounters) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(encounters))
	var out [][]byte
	for _, e := range encounters {
		k := string(e.PlateHash)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, append([]byte(nil), e.PlateHash...))
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i]) < string(out[j])
	})
	return out
}

// alprEnabled mirrors the same-named helpers on the extractor and
// detector. Falls back to false when the settings store is nil or
// unreadable -- the safe default for an opt-in feature.
func (w *ALPRAggregator) alprEnabled(ctx context.Context) bool {
	if w.alprEnabledForTest != nil {
		return *w.alprEnabledForTest
	}
	if w.Settings == nil {
		return false
	}
	v, err := w.Settings.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		log.Printf("alpr aggregator: read alpr_enabled: %v", err)
		return false
	}
	return v
}

// encounterGap reads the runtime threshold or falls back to the
// struct default. The precedence is: settings row > struct default
// (which the wiring layer populated from ENCOUNTER_GAP_SECONDS or the
// hard-coded default). Out-of-range values clamp to the minimum so a
// misconfigured row cannot collapse every detection into its own
// encounter.
func (w *ALPRAggregator) encounterGap(ctx context.Context) float64 {
	def := w.DefaultEncounterGapSeconds
	if def < minALPREncounterGapSeconds {
		def = DefaultALPREncounterGapSeconds
	}
	if w.Settings == nil {
		return def
	}
	v, err := w.Settings.FloatOr(ctx, settings.KeyALPREncounterGapSeconds, def)
	if err != nil {
		// FloatOr already swallows ErrNotFound; only surface
		// unexpected DB errors here.
		if !errors.Is(err, settings.ErrNotFound) {
			log.Printf("alpr aggregator: read alpr_encounter_gap_seconds: %v", err)
		}
		return def
	}
	if v < minALPREncounterGapSeconds {
		return def
	}
	return v
}
