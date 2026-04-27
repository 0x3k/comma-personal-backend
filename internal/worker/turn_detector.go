package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// Defaults for TurnDetectorWorker. The window/delta/dedup defaults are
// the same numbers exposed to operators via TURN_*_SECONDS env vars; the
// poll/finalized constants mirror the trip-aggregator/route-metadata
// pair so this worker rides the same "route-finalized" signal.
const (
	defaultTurnPollInterval   = 60 * time.Second
	defaultTurnFinalizedAfter = 5 * time.Minute
	defaultTurnBatchLimit     = 50

	defaultTurnWindowSeconds = 4.0
	defaultTurnDeltaDegMin   = 35.0
	defaultTurnDedupSeconds  = 5.0

	defaultTurnBackfillLimit   = 200
	defaultTurnBackfillPause   = 1 * time.Second
	turnDetectorMinVertices    = 30
	turnDetectorMinDurationSec = 30.0
)

// TurnDetectorMetrics is the subset of *metrics.Metrics the worker uses.
// Extracted as an interface so tests can pass nil (a nil *Metrics is a
// no-op) or a fake without spinning up a Prometheus registry.
type TurnDetectorMetrics interface {
	IncTurnDetectorRun(result string)
	AddTurnDetectorTurnsEmitted(n int)
	ObserveTurnDetectorRun(d time.Duration)
}

// TurnDetectorWorker scans finalized routes' GPS geometry and writes a
// turn timeline to route_turns. It is always-on: not gated on ALPR, since
// turns are independently useful (analytics, smart playback). The
// algorithm and thresholds match the spec: a sliding TURN_WINDOW_SECONDS
// window, an absolute delta threshold of TURN_DELTA_DEG_MIN degrees, and
// a TURN_DEDUP_SECONDS suppression window after each fired turn.
type TurnDetectorWorker struct {
	// Queries is the sqlc-generated db handle. Required.
	Queries *db.Queries

	// Pool is the pgx pool used for transactional delete-then-insert per
	// route. Required at run time; tests that exercise RunOnce against a
	// real database supply this. The worker can still run without it
	// (the geometry/insert happens via Queries) but loses transactional
	// idempotency, so production wiring sets both.
	Pool TxBeginner

	// Settings is the runtime tunables store. May be nil, in which case
	// the worker uses the env-derived defaults set on the struct fields.
	Settings *settings.Store

	// Metrics receives per-run counters and the run-duration histogram.
	// Safe to leave nil; the worker degrades to logs only.
	Metrics TurnDetectorMetrics

	// PollInterval is how long to sleep between aggregation passes.
	// Defaults to 60s when zero.
	PollInterval time.Duration

	// FinalizedAfter is the minimum age of the most recent segment
	// before a route is considered "done uploading" and eligible.
	// Defaults to 5m.
	FinalizedAfter time.Duration

	// BatchLimit caps the number of routes processed per pass so the
	// worker doesn't monopolize the DB on a backlog. Defaults to 50.
	BatchLimit int32

	// Default tunable values, used as fallbacks when the corresponding
	// row is absent from the settings table. Operators populate these
	// from the TURN_WINDOW_SECONDS / TURN_DELTA_DEG_MIN /
	// TURN_DEDUP_SECONDS env vars at startup.
	DefaultWindowSeconds float64
	DefaultDeltaDegMin   float64
	DefaultDedupSeconds  float64

	// BackfillLimit caps how many recent routes the one-shot backfill
	// processes after process start. Defaults to 200 when zero.
	BackfillLimit int

	// BackfillPause throttles the backfill loop. Defaults to 1s when
	// zero, matching the spec's "~1 route/sec" rate limit.
	BackfillPause time.Duration
}

// TxBeginner abstracts the small subset of pgxpool we use for the
// per-route delete-then-insert transaction. Callers in production pass a
// *pgxpool.Pool; tests can supply any pgx-compatible begin-able handle.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewTurnDetectorWorker constructs a worker with sane defaults. Queries
// is required at run time; the constructor does not validate it so
// callers can inject doubles in tests.
func NewTurnDetectorWorker(queries *db.Queries, pool TxBeginner, settingsStore *settings.Store, m TurnDetectorMetrics) *TurnDetectorWorker {
	return &TurnDetectorWorker{
		Queries:              queries,
		Pool:                 pool,
		Settings:             settingsStore,
		Metrics:              m,
		PollInterval:         defaultTurnPollInterval,
		FinalizedAfter:       defaultTurnFinalizedAfter,
		BatchLimit:           defaultTurnBatchLimit,
		DefaultWindowSeconds: defaultTurnWindowSeconds,
		DefaultDeltaDegMin:   defaultTurnDeltaDegMin,
		DefaultDedupSeconds:  defaultTurnDedupSeconds,
		BackfillLimit:        defaultTurnBackfillLimit,
		BackfillPause:        defaultTurnBackfillPause,
	}
}

// Run drives the detection loop until ctx is cancelled. Per-route errors
// are logged and do not abort the loop; only an unrecoverable list
// failure ends the run. Run also kicks off a one-shot backfill that
// processes the most-recent N routes, separately from the steady-state
// poll, so a fresh deploy doesn't have to wait for new uploads to see
// turn timelines.
func (w *TurnDetectorWorker) Run(ctx context.Context) {
	poll := w.PollInterval
	if poll <= 0 {
		poll = defaultTurnPollInterval
	}

	// One-shot backfill in the background. It runs concurrently with the
	// steady-state loop so a long backfill (200 routes at ~1/s = ~3min)
	// does not delay the first finalized-route detection.
	go w.runBackfill(ctx)

	if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("turn_detector: pass failed: %v", err)
	}

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("turn_detector: pass failed: %v", err)
			}
		}
	}
}

// RunOnce executes a single detection pass. It reuses the same
// "ListRoutesNeedingMetadata"-style trigger as the trip-aggregator: a
// route is eligible when its newest segment is older than FinalizedAfter
// AND the route already has start_time + geometry populated by the
// route-metadata worker.
//
// The pass is idempotent at the route level: each route deletes its
// prior turns and inserts the freshly computed set inside one
// transaction, so reprocessing a route never produces duplicates or
// partial state visible to readers.
func (w *TurnDetectorWorker) RunOnce(ctx context.Context) error {
	limit := w.BatchLimit
	if limit <= 0 {
		limit = defaultTurnBatchLimit
	}
	finalizedAfter := w.FinalizedAfter
	if finalizedAfter <= 0 {
		finalizedAfter = defaultTurnFinalizedAfter
	}

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-finalizedAfter), Valid: true}
	routes, err := w.Queries.ListRoutesForTurnDetection(ctx, db.ListRoutesForTurnDetectionParams{
		FinalizedBefore: cutoff,
		Limit:           limit,
	})
	if err != nil {
		return err
	}

	for _, r := range routes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.processRoute(ctx, r.DongleID, r.RouteName); err != nil {
			log.Printf("turn_detector: route %s/%s: %v", r.DongleID, r.RouteName, err)
		}
	}
	return nil
}

// processRoute computes and persists the turn timeline for one route.
// The "result" label on the runs counter is one of:
//
//	"emitted"  -- detection ran and at least one turn was written
//	"empty"    -- detection ran cleanly but yielded zero turns
//	"skipped"  -- route had insufficient signal (too few vertices or too short)
//	"error"    -- detection or persistence failed
func (w *TurnDetectorWorker) processRoute(ctx context.Context, dongleID, routeName string) error {
	start := time.Now()
	defer func() {
		if w.Metrics != nil {
			w.Metrics.ObserveTurnDetectorRun(time.Since(start))
		}
	}()

	cfg, err := w.loadTunables(ctx)
	if err != nil {
		w.incRun("error")
		return fmt.Errorf("load tunables: %w", err)
	}

	geom, err := w.Queries.GetRouteGeometryAndTimes(ctx, db.GetRouteGeometryWKTParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.incRun("skipped")
			return nil
		}
		w.incRun("error")
		return fmt.Errorf("load geometry: %w", err)
	}
	if !geom.WKT.Valid || len(geom.Times) == 0 {
		w.incRun("skipped")
		return nil
	}

	route, err := w.Queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.incRun("skipped")
			return nil
		}
		w.incRun("error")
		return fmt.Errorf("get route: %w", err)
	}
	if !route.StartTime.Valid {
		// No anchor for absolute timestamps -- skip rather than emit
		// turns whose ts is meaningless.
		w.incRun("skipped")
		return nil
	}

	verts, err := parseLineStringWKT(geom.WKT.String)
	if err != nil {
		w.incRun("error")
		return fmt.Errorf("parse geometry: %w", err)
	}

	if len(verts) != len(geom.Times) {
		// Defensive: the parallel-array invariant should be enforced by
		// the route metadata worker, but a stale geometry_times array
		// would silently misalign every turn timestamp. Skip rather
		// than emit lies.
		log.Printf("turn_detector: %s/%s: vertices=%d times=%d (parallel-array mismatch); skipping",
			dongleID, routeName, len(verts), len(geom.Times))
		w.incRun("skipped")
		return nil
	}

	if len(verts) < turnDetectorMinVertices {
		log.Printf("turn_detector: %s/%s: only %d vertices (< %d), skipping",
			dongleID, routeName, len(verts), turnDetectorMinVertices)
		w.incRun("skipped")
		return nil
	}
	durationSec := float64(geom.Times[len(geom.Times)-1]-geom.Times[0]) / 1000.0
	if durationSec < turnDetectorMinDurationSec {
		log.Printf("turn_detector: %s/%s: only %.1fs of geometry (< %.0fs), skipping",
			dongleID, routeName, durationSec, turnDetectorMinDurationSec)
		w.incRun("skipped")
		return nil
	}

	turns := DetectTurns(verts, geom.Times, cfg)
	if err := w.persistTurns(ctx, dongleID, routeName, route.StartTime.Time, turns); err != nil {
		w.incRun("error")
		return fmt.Errorf("persist turns: %w", err)
	}

	if w.Metrics != nil {
		w.Metrics.AddTurnDetectorTurnsEmitted(len(turns))
	}
	if len(turns) > 0 {
		w.incRun("emitted")
	} else {
		w.incRun("empty")
	}
	return nil
}

func (w *TurnDetectorWorker) incRun(result string) {
	if w.Metrics != nil {
		w.Metrics.IncTurnDetectorRun(result)
	}
}

// persistTurns deletes the route's existing turns and inserts the new
// set inside a single transaction so a reader never sees a half-applied
// recomputation. When the worker is wired without a transactional pool
// (tests), it falls back to non-transactional delete+insert; the unique
// constraint on (dongle_id, route, turn_offset_ms) still prevents
// duplicates.
func (w *TurnDetectorWorker) persistTurns(ctx context.Context, dongleID, routeName string, routeStart time.Time, turns []DetectedTurn) error {
	if w.Pool == nil {
		return w.persistTurnsNoTx(ctx, dongleID, routeName, routeStart, turns)
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := w.Queries.WithTx(tx)
	if err := qtx.DeleteTurnsForRoute(ctx, db.DeleteTurnsForRouteParams{
		DongleID: dongleID,
		Route:    routeName,
	}); err != nil {
		return fmt.Errorf("delete prior turns: %w", err)
	}
	for _, t := range turns {
		if err := qtx.InsertTurn(ctx, turnInsertParams(dongleID, routeName, routeStart, t)); err != nil {
			return fmt.Errorf("insert turn @%dms: %w", t.OffsetMs, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func (w *TurnDetectorWorker) persistTurnsNoTx(ctx context.Context, dongleID, routeName string, routeStart time.Time, turns []DetectedTurn) error {
	if err := w.Queries.DeleteTurnsForRoute(ctx, db.DeleteTurnsForRouteParams{
		DongleID: dongleID,
		Route:    routeName,
	}); err != nil {
		return fmt.Errorf("delete prior turns: %w", err)
	}
	for _, t := range turns {
		if err := w.Queries.InsertTurn(ctx, turnInsertParams(dongleID, routeName, routeStart, t)); err != nil {
			return fmt.Errorf("insert turn @%dms: %w", t.OffsetMs, err)
		}
	}
	return nil
}

func turnInsertParams(dongleID, routeName string, routeStart time.Time, t DetectedTurn) db.InsertTurnParams {
	ts := routeStart.Add(time.Duration(t.OffsetMs) * time.Millisecond).UTC()
	return db.InsertTurnParams{
		DongleID:         dongleID,
		Route:            routeName,
		TurnTs:           pgtype.Timestamptz{Time: ts, Valid: true},
		TurnOffsetMs:     int32(t.OffsetMs),
		BearingBeforeDeg: float32(t.BearingBefore),
		BearingAfterDeg:  float32(t.BearingAfter),
		DeltaDeg:         float32(t.DeltaDeg),
		GpsLat:           pgtype.Float8{Float64: t.Lat, Valid: true},
		GpsLng:           pgtype.Float8{Float64: t.Lng, Valid: true},
	}
}

// runBackfill processes the most-recent N routes that don't yet have
// turn rows. It runs once per process start. Routes are picked oldest
// first within the window so the operator's freshest activity finishes
// last (and is the most likely to receive new updates that will refresh
// the turns anyway). Sleep between routes implements the rate limit so
// the backfill never starves the steady-state worker.
func (w *TurnDetectorWorker) runBackfill(ctx context.Context) {
	limit := w.BackfillLimit
	if limit <= 0 {
		limit = defaultTurnBackfillLimit
	}
	pause := w.BackfillPause
	if pause <= 0 {
		pause = defaultTurnBackfillPause
	}

	rows, err := w.Queries.ListRecentRoutesForTurnBackfill(ctx, int32(limit))
	if err != nil {
		log.Printf("turn_detector: backfill list failed: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.Printf("turn_detector: backfill starting (%d routes)", len(rows))
	processed := 0
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		if err := w.processRoute(ctx, r.DongleID, r.RouteName); err != nil {
			log.Printf("turn_detector: backfill route %s/%s: %v", r.DongleID, r.RouteName, err)
		}
		processed++
		// Sleep AFTER processing so a one-route backfill doesn't pay
		// the rate-limit tax. The select wakes immediately when ctx is
		// cancelled.
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
	log.Printf("turn_detector: backfill done (%d/%d routes)", processed, len(rows))
}

// loadTunables consults the settings table for the per-tick window /
// delta / dedup values, falling back to the worker's struct defaults
// (which themselves came from env vars at startup). The settings store
// is allowed to be nil at construction time, in which case the env
// defaults are returned directly.
func (w *TurnDetectorWorker) loadTunables(ctx context.Context) (TurnConfig, error) {
	cfg := TurnConfig{
		WindowSeconds: w.DefaultWindowSeconds,
		DeltaDegMin:   w.DefaultDeltaDegMin,
		DedupSeconds:  w.DefaultDedupSeconds,
	}
	if w.Settings == nil {
		return cfg.normalized(), nil
	}

	if v, err := w.Settings.GetFloat(ctx, settings.KeyTurnWindowSeconds); err == nil {
		cfg.WindowSeconds = v
	} else if !errors.Is(err, settings.ErrNotFound) {
		return cfg, err
	}
	if v, err := w.Settings.GetFloat(ctx, settings.KeyTurnDeltaDegMin); err == nil {
		cfg.DeltaDegMin = v
	} else if !errors.Is(err, settings.ErrNotFound) {
		return cfg, err
	}
	if v, err := w.Settings.GetFloat(ctx, settings.KeyTurnDedupSeconds); err == nil {
		cfg.DedupSeconds = v
	} else if !errors.Is(err, settings.ErrNotFound) {
		return cfg, err
	}
	return cfg.normalized(), nil
}

// TurnConfig holds the runtime tunables that govern the turn detector
// algorithm. Window and dedup are seconds, delta is in degrees.
type TurnConfig struct {
	WindowSeconds float64
	DeltaDegMin   float64
	DedupSeconds  float64
}

// normalized clamps the tunables to safe lower bounds. Negative or zero
// values would cause divide-by-zero or empty-window pathology in the
// detector; we coerce them to the canonical defaults instead of erroring
// so an operator can recover by setting any positive value.
func (c TurnConfig) normalized() TurnConfig {
	if c.WindowSeconds <= 0 {
		c.WindowSeconds = defaultTurnWindowSeconds
	}
	if c.DeltaDegMin <= 0 {
		c.DeltaDegMin = defaultTurnDeltaDegMin
	}
	if c.DedupSeconds < 0 {
		c.DedupSeconds = defaultTurnDedupSeconds
	}
	return c
}

// DetectedTurn is one row in the per-route turn timeline. Lat/Lng point
// at the geometry vertex closest to the central timestamp; the offset
// is route-relative milliseconds so the wall-clock ts can be derived
// from the route's start_time at write/read time.
type DetectedTurn struct {
	OffsetMs      int64
	BearingBefore float64
	BearingAfter  float64
	DeltaDeg      float64
	Lat           float64
	Lng           float64
}

// LatLng is a single GPS sample. Fields use float64 to match the WKT
// parser and the cereal extractor's GpsPoint resolution.
type LatLng struct {
	Lat float64
	Lng float64
}

// DetectTurns is the pure algorithm: given a parallel (vertices, times)
// pair plus a TurnConfig, return the list of detected turns in
// chronological order. Pure: no I/O, no allocation tied to the database
// driver. This is what the unit tests exercise.
//
// The algorithm:
//
//  1. Slide a window of half-width (WindowSeconds/2) ms around each
//     vertex t = times[i]. Compute the average bearing across the
//     samples that fall in the (t - h, t] window ("before") and the
//     (t, t + h] window ("after").
//  2. delta = wrapTo180(after - before). Wrapping is critical: a bearing
//     change of 358 -> 2 deg is a +4 deg crossing of north, NOT a -356
//     deg sweep. wrapTo180 does the modular arithmetic.
//  3. When |delta| >= DeltaDegMin, emit a turn at the central
//     timestamp.
//  4. Suppress duplicates: skip a candidate if the last emitted turn
//     was within DedupSeconds ms.
//
// "Average bearing across a window" is computed in the unit-vector
// domain (sum of cos/sin, then atan2 of the sum) so the average
// respects the angular metric -- a naive arithmetic mean would say
// (350 + 10) / 2 = 180 instead of 0.
func DetectTurns(verts []LatLng, timesMs []int64, cfg TurnConfig) []DetectedTurn {
	cfg = cfg.normalized()
	if len(verts) < 3 || len(verts) != len(timesMs) {
		return nil
	}

	// Per-edge bearings: bearings[i] is the bearing from verts[i] to
	// verts[i+1], expressed as degrees clockwise from north in [0, 360).
	bearings := make([]float64, len(verts)-1)
	for i := 0; i < len(verts)-1; i++ {
		bearings[i] = bearingDeg(verts[i].Lat, verts[i].Lng, verts[i+1].Lat, verts[i+1].Lng)
	}
	// Edge midpoint times: an edge spans [times[i], times[i+1]], so we
	// associate each edge with its midpoint for window membership.
	edgeMidMs := make([]int64, len(bearings))
	for i := range edgeMidMs {
		edgeMidMs[i] = (timesMs[i] + timesMs[i+1]) / 2
	}

	halfWindowMs := int64(cfg.WindowSeconds * 1000.0 / 2.0)
	dedupMs := int64(cfg.DedupSeconds * 1000.0)
	if halfWindowMs <= 0 {
		halfWindowMs = int64(defaultTurnWindowSeconds * 1000.0 / 2.0)
	}

	var out []DetectedTurn
	// haveEmitted tracks whether the dedup window applies. Using a
	// sentinel like math.MinInt64 would overflow when subtracted from
	// t, so we keep the boolean explicit instead.
	var haveEmitted bool
	var lastEmittedMs int64

	// We anchor the candidate at each interior vertex i, i.e. the
	// window straddles vertex i with edges (i-1) on the "before" side
	// and edge i on the "after" side, plus any neighbouring edges that
	// fall inside the window.
	for i := 1; i < len(verts)-1; i++ {
		t := timesMs[i]

		before, okB := windowedBearing(bearings, edgeMidMs, t-halfWindowMs, t)
		after, okA := windowedBearing(bearings, edgeMidMs, t, t+halfWindowMs)
		if !okB || !okA {
			continue
		}

		delta := wrapTo180(after - before)
		if math.Abs(delta) < cfg.DeltaDegMin {
			continue
		}
		if haveEmitted && t-lastEmittedMs < dedupMs {
			continue
		}

		out = append(out, DetectedTurn{
			OffsetMs:      t - timesMs[0],
			BearingBefore: before,
			BearingAfter:  after,
			DeltaDeg:      delta,
			Lat:           verts[i].Lat,
			Lng:           verts[i].Lng,
		})
		haveEmitted = true
		lastEmittedMs = t
	}
	return out
}

// windowedBearing returns the unit-vector-mean bearing across edges
// whose midpoint timestamp falls in (loMs, hiMs]. The boolean result is
// false when no edges sit inside the window (which happens at the head
// and tail of every route). Edges can be sparser than the window if the
// metadata worker dropped duplicate vertices, so we always scan, never
// assume a fixed sample rate.
func windowedBearing(bearings []float64, edgeMidMs []int64, loMs, hiMs int64) (float64, bool) {
	var sumCos, sumSin float64
	count := 0
	for i, m := range edgeMidMs {
		if m > loMs && m <= hiMs {
			rad := bearings[i] * math.Pi / 180.0
			sumCos += math.Cos(rad)
			sumSin += math.Sin(rad)
			count++
		}
	}
	if count == 0 {
		return 0, false
	}
	deg := math.Atan2(sumSin, sumCos) * 180.0 / math.Pi
	if deg < 0 {
		deg += 360.0
	}
	return deg, true
}

// bearingDeg returns the initial bearing (degrees clockwise from north,
// in [0, 360)) on the great-circle path from (lat1, lng1) to (lat2,
// lng2). The formula is the standard haversine-derived bearing:
//
//	θ = atan2( sin(Δλ) · cos(φ2),
//	           cos(φ1) · sin(φ2) - sin(φ1) · cos(φ2) · cos(Δλ) )
//
// where φ is latitude and λ is longitude, both in radians, and Δλ =
// λ2 - λ1. The atan2 result is in (-π, π]; we wrap it to [0, 2π) and
// convert to degrees so the [0, 360) convention matches the rest of
// the file. WHY this comment: the math is easy to misread (it is NOT
// the rhumb-line bearing, and atan2's argument order is (y, x), not
// (x, y)); copy-paste bugs here would rotate every emitted turn by
// hundreds of metres.
func bearingDeg(lat1, lng1, lat2, lng2 float64) float64 {
	phi1 := lat1 * math.Pi / 180.0
	phi2 := lat2 * math.Pi / 180.0
	dLambda := (lng2 - lng1) * math.Pi / 180.0

	y := math.Sin(dLambda) * math.Cos(phi2)
	x := math.Cos(phi1)*math.Sin(phi2) - math.Sin(phi1)*math.Cos(phi2)*math.Cos(dLambda)
	rad := math.Atan2(y, x)
	deg := rad * 180.0 / math.Pi
	if deg < 0 {
		deg += 360.0
	}
	return deg
}

// wrapTo180 maps an angular delta into (-180, 180] using modular
// arithmetic. The classic trap this avoids: bearing 358 -> 2 is a +4
// deg change crossing north, NOT a -356 deg sweep. The formula
// `mod(delta + 540, 360) - 180` is the textbook wrap-to-180; the +540
// instead of +180 keeps the result in (-180, 180] (rather than [-180,
// 180)) so a U-turn (exactly 180) lands on the positive side.
func wrapTo180(deg float64) float64 {
	return math.Mod(deg+540.0, 360.0) - 180.0
}

// parseLineStringWKT decodes a Postgres WKT LineString of the form
// "LINESTRING(lng lat, lng lat, ...)" into a slice of LatLng. Order is
// (lng, lat) per the WKT spec, even though our LatLng struct (and most
// human-readable APIs) use (lat, lng) -- the swap happens here. The
// route metadata worker writes the WKT we read here, so the format is
// stable; we still validate it defensively because a malformed string
// produces silently wrong turns rather than a parse error.
func parseLineStringWKT(wkt string) ([]LatLng, error) {
	const prefix = "LINESTRING("
	if !strings.HasPrefix(wkt, prefix) || !strings.HasSuffix(wkt, ")") {
		return nil, fmt.Errorf("turn_detector: not a LINESTRING WKT: %q", wkt)
	}
	body := wkt[len(prefix) : len(wkt)-1]
	if body == "" {
		return nil, nil
	}
	parts := strings.Split(body, ",")
	out := make([]LatLng, 0, len(parts))
	for _, p := range parts {
		fields := strings.Fields(strings.TrimSpace(p))
		if len(fields) != 2 {
			return nil, fmt.Errorf("turn_detector: bad WKT vertex %q", p)
		}
		lng, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("turn_detector: parse lng %q: %w", fields[0], err)
		}
		lat, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, fmt.Errorf("turn_detector: parse lat %q: %w", fields[1], err)
		}
		out = append(out, LatLng{Lat: lat, Lng: lng})
	}
	return out, nil
}
