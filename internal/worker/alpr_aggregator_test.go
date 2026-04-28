package worker

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// fakeAggregatorQuerier is a minimal in-memory ALPRAggregatorQuerier.
// The same instance is returned from WithTxQuerier; we don't model
// rollback because every test asserts on the *committed* end state.
type fakeAggregatorQuerier struct {
	mu sync.Mutex

	// Per-route inputs the test seeds before running.
	detectionsByRoute map[string][]db.ListDetectionsForRouteRow
	turnsInWindow     []turnWindowEntry

	// Recorded writes (committed state).
	encounters    map[string][]db.UpsertEncounterParams
	deletedRoutes []db.DeleteEncountersForRouteParams

	// Failure injection.
	listErr     error
	turnsErr    error
	deleteErr   error
	upsertErr   error
	upsertCalls int
}

// turnWindowEntry models one "there are this many turns in this
// window" row the test wants the fake to return. The fake matches a
// CountTurnsInWindow call by dongle/route and the inclusive
// [WindowStart, WindowEnd] range overlap.
type turnWindowEntry struct {
	DongleID    string
	Route       string
	WindowStart time.Time
	WindowEnd   time.Time
	Count       int64
}

func newFakeAggregatorQuerier() *fakeAggregatorQuerier {
	return &fakeAggregatorQuerier{
		detectionsByRoute: make(map[string][]db.ListDetectionsForRouteRow),
		encounters:        make(map[string][]db.UpsertEncounterParams),
	}
}

func aggKey(dongle, route string) string {
	return dongle + "|" + route
}

func (f *fakeAggregatorQuerier) ListDetectionsForRoute(_ context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	rows := f.detectionsByRoute[aggKey(arg.DongleID, arg.Route)]
	out := make([]db.ListDetectionsForRouteRow, len(rows))
	copy(out, rows)
	return out, nil
}

func (f *fakeAggregatorQuerier) CountTurnsInWindow(_ context.Context, arg db.CountTurnsInWindowParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.turnsErr != nil {
		return 0, f.turnsErr
	}
	var total int64
	for _, e := range f.turnsInWindow {
		if e.DongleID != arg.DongleID || e.Route != arg.Route {
			continue
		}
		// A turn at e.WindowStart counts when it falls within the
		// query's inclusive [WindowStart, WindowEnd] range.
		ws := arg.WindowStart.Time
		we := arg.WindowEnd.Time
		if (e.WindowStart.Equal(ws) || e.WindowStart.After(ws)) &&
			(e.WindowStart.Equal(we) || e.WindowStart.Before(we)) {
			total += e.Count
		}
	}
	return total, nil
}

func (f *fakeAggregatorQuerier) DeleteEncountersForRoute(_ context.Context, arg db.DeleteEncountersForRouteParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	f.deletedRoutes = append(f.deletedRoutes, arg)
	k := aggKey(arg.DongleID, arg.Route)
	deleted := int64(len(f.encounters[k]))
	delete(f.encounters, k)
	return deleted, nil
}

func (f *fakeAggregatorQuerier) UpsertEncounter(_ context.Context, arg db.UpsertEncounterParams) (db.PlateEncounter, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return db.PlateEncounter{}, f.upsertErr
	}
	f.upsertCalls++
	k := aggKey(arg.DongleID, arg.Route)
	// Replace any existing entry with the same first_seen_ts to mimic
	// the ON CONFLICT DO UPDATE semantics. (The aggregator runs
	// DELETE-then-INSERT, so this branch is mostly defensive.)
	replaced := false
	for i, prior := range f.encounters[k] {
		if bytes.Equal(prior.PlateHash, arg.PlateHash) && prior.FirstSeenTs.Time.Equal(arg.FirstSeenTs.Time) {
			f.encounters[k][i] = arg
			replaced = true
			break
		}
	}
	if !replaced {
		f.encounters[k] = append(f.encounters[k], arg)
	}
	id := int64(f.upsertCalls)
	return db.PlateEncounter{
		ID:                    id,
		DongleID:              arg.DongleID,
		Route:                 arg.Route,
		PlateHash:             arg.PlateHash,
		FirstSeenTs:           arg.FirstSeenTs,
		LastSeenTs:            arg.LastSeenTs,
		DetectionCount:        arg.DetectionCount,
		TurnCount:             arg.TurnCount,
		MaxInternalGapSeconds: arg.MaxInternalGapSeconds,
		SignatureID:           arg.SignatureID,
		Status:                arg.Status,
		BboxFirst:             arg.BboxFirst,
		BboxLast:              arg.BboxLast,
	}, nil
}

func (f *fakeAggregatorQuerier) WithTxQuerier(_ pgx.Tx) ALPRAggregatorQuerier {
	return f
}

// fakeAggregatorPool is a minimal ALPRAggregatorTxBeginner that returns
// a fresh fakeTx (defined in alpr_detector_test.go) per Begin.
type fakeAggregatorPool struct {
	mu       sync.Mutex
	beginErr error
	calls    int
	txs      []*fakeTx
}

func (p *fakeAggregatorPool) Begin(_ context.Context) (pgx.Tx, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	tx := &fakeTx{}
	p.txs = append(p.txs, tx)
	return tx, nil
}

// detectionRowAt builds a db.ListDetectionsForRouteRow with the bbox
// the caller supplies. This is the single test fixture builder used
// throughout this file; the explicit bbox lets tests assert that
// bbox_first / bbox_last carry the right rows.
func detectionRowAt(id int64, plateHash string, frameTs time.Time, bbox []byte, sigID int64, sigValid bool) db.ListDetectionsForRouteRow {
	return db.ListDetectionsForRouteRow{
		ID:          id,
		DongleID:    "dongle1",
		Route:       "route1",
		PlateHash:   []byte(plateHash),
		Bbox:        bbox,
		FrameTs:     pgtype.Timestamptz{Time: frameTs, Valid: true},
		SignatureID: pgtype.Int8{Int64: sigID, Valid: sigValid},
	}
}

// TestComputeEncounters_SingleEncounter verifies that a tight burst of
// detections with no gap exceeding the threshold collapses into one
// encounter. detection_count tracks the row count, max_internal_gap
// reflects the largest in-encounter gap, and bbox_first/bbox_last
// point at the first and last rows respectively.
func TestComputeEncounters_SingleEncounter(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, []byte(`{"x":1}`), 0, false),
		detectionRowAt(2, "plateA", t0.Add(2*time.Second), []byte(`{"x":2}`), 0, false),
		detectionRowAt(3, "plateA", t0.Add(5*time.Second), []byte(`{"x":3}`), 0, false),
		detectionRowAt(4, "plateA", t0.Add(20*time.Second), []byte(`{"x":4}`), 0, false),
	}
	got := computeEncounters(rows, 60.0)
	if len(got) != 1 {
		t.Fatalf("want 1 encounter, got %d", len(got))
	}
	enc := got[0]
	if enc.DetectionCount != 4 {
		t.Errorf("detection_count: want 4, got %d", enc.DetectionCount)
	}
	if !enc.FirstSeenTs.Time.Equal(t0) {
		t.Errorf("first_seen_ts: want %v, got %v", t0, enc.FirstSeenTs.Time)
	}
	if !enc.LastSeenTs.Time.Equal(t0.Add(20 * time.Second)) {
		t.Errorf("last_seen_ts: want %v, got %v", t0.Add(20*time.Second), enc.LastSeenTs.Time)
	}
	// Max internal gap: t0 -> +2s = 2, +2 -> +5 = 3, +5 -> +20 = 15.
	if enc.MaxInternalGapSeconds != 15 {
		t.Errorf("max_internal_gap_seconds: want 15, got %d", enc.MaxInternalGapSeconds)
	}
	if !bytes.Equal(enc.BboxFirst, []byte(`{"x":1}`)) {
		t.Errorf("bbox_first: want first row's bbox, got %s", string(enc.BboxFirst))
	}
	if !bytes.Equal(enc.BboxLast, []byte(`{"x":4}`)) {
		t.Errorf("bbox_last: want last row's bbox, got %s", string(enc.BboxLast))
	}
	if enc.SignatureID.Valid {
		t.Errorf("signature_id: want null (no row had one), got %d", enc.SignatureID.Int64)
	}
}

// TestComputeEncounters_TwoEncountersSplitByGap verifies that a >60s
// gap (90s here) starts a fresh encounter. detection_count and
// max_internal_gap are computed within each encounter independently.
func TestComputeEncounters_TwoEncountersSplitByGap(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, []byte(`{"x":1}`), 0, false),
		detectionRowAt(2, "plateA", t0.Add(5*time.Second), []byte(`{"x":2}`), 0, false),
		detectionRowAt(3, "plateA", t0.Add(10*time.Second), []byte(`{"x":3}`), 0, false),
		// 90s gap kicks off encounter 2.
		detectionRowAt(4, "plateA", t0.Add(100*time.Second), []byte(`{"x":4}`), 0, false),
		detectionRowAt(5, "plateA", t0.Add(105*time.Second), []byte(`{"x":5}`), 0, false),
	}
	got := computeEncounters(rows, 60.0)
	if len(got) != 2 {
		t.Fatalf("want 2 encounters, got %d", len(got))
	}
	first := got[0]
	if first.DetectionCount != 3 {
		t.Errorf("encounter[0].detection_count: want 3, got %d", first.DetectionCount)
	}
	if !first.LastSeenTs.Time.Equal(t0.Add(10 * time.Second)) {
		t.Errorf("encounter[0].last_seen_ts: want +10s, got %v", first.LastSeenTs.Time)
	}
	if first.MaxInternalGapSeconds != 5 {
		t.Errorf("encounter[0].max_internal_gap_seconds: want 5, got %d", first.MaxInternalGapSeconds)
	}
	second := got[1]
	if second.DetectionCount != 2 {
		t.Errorf("encounter[1].detection_count: want 2, got %d", second.DetectionCount)
	}
	if !second.FirstSeenTs.Time.Equal(t0.Add(100 * time.Second)) {
		t.Errorf("encounter[1].first_seen_ts: want +100s, got %v", second.FirstSeenTs.Time)
	}
	if second.MaxInternalGapSeconds != 5 {
		t.Errorf("encounter[1].max_internal_gap_seconds: want 5, got %d", second.MaxInternalGapSeconds)
	}
	if !bytes.Equal(second.BboxFirst, []byte(`{"x":4}`)) {
		t.Errorf("encounter[1].bbox_first: want row 4's bbox, got %s", string(second.BboxFirst))
	}
}

// TestComputeEncounters_SignatureModeTieBreaker verifies the
// deterministic-tie-break rule: when two signature_ids both occur the
// same number of times, the lowest id wins. Also verifies that null
// rows are ignored when computing the mode.
func TestComputeEncounters_SignatureModeTieBreaker(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 7, true),
		detectionRowAt(2, "plateA", t0.Add(time.Second), nil, 3, true),
		// Null signature: must be ignored by the mode.
		detectionRowAt(3, "plateA", t0.Add(2*time.Second), nil, 0, false),
		detectionRowAt(4, "plateA", t0.Add(3*time.Second), nil, 7, true),
		detectionRowAt(5, "plateA", t0.Add(4*time.Second), nil, 3, true),
	}
	got := computeEncounters(rows, 60.0)
	if len(got) != 1 {
		t.Fatalf("want 1 encounter, got %d", len(got))
	}
	enc := got[0]
	if !enc.SignatureID.Valid {
		t.Fatalf("signature_id: want a value, got null")
	}
	// Tie at count=2 between sig_id 3 and 7; deterministic pick is 3.
	if enc.SignatureID.Int64 != 3 {
		t.Errorf("signature_id (tie break): want 3, got %d", enc.SignatureID.Int64)
	}
}

// TestComputeEncounters_AllNullSignatures verifies that an encounter
// where no row has a signature ends up with signature_id=null.
func TestComputeEncounters_AllNullSignatures(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 0, false),
		detectionRowAt(2, "plateA", t0.Add(time.Second), nil, 0, false),
	}
	got := computeEncounters(rows, 60.0)
	if len(got) != 1 {
		t.Fatalf("want 1 encounter, got %d", len(got))
	}
	if got[0].SignatureID.Valid {
		t.Errorf("signature_id: want null, got %d", got[0].SignatureID.Int64)
	}
}

// TestComputeEncounters_DistinctPlatesProduceSeparateEncounters
// verifies that two plates seen in the same route window produce two
// separate encounters; their groups are independent.
func TestComputeEncounters_DistinctPlatesProduceSeparateEncounters(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 0, false),
		detectionRowAt(2, "plateB", t0.Add(time.Second), nil, 0, false),
		detectionRowAt(3, "plateA", t0.Add(2*time.Second), nil, 0, false),
		detectionRowAt(4, "plateB", t0.Add(3*time.Second), nil, 0, false),
	}
	got := computeEncounters(rows, 60.0)
	if len(got) != 2 {
		t.Fatalf("want 2 encounters, got %d", len(got))
	}
	byHash := map[string]encounter{}
	for _, e := range got {
		byHash[string(e.PlateHash)] = e
	}
	if byHash["plateA"].DetectionCount != 2 {
		t.Errorf("plateA detection_count: want 2, got %d", byHash["plateA"].DetectionCount)
	}
	if byHash["plateB"].DetectionCount != 2 {
		t.Errorf("plateB detection_count: want 2, got %d", byHash["plateB"].DetectionCount)
	}
}

// TestProcessRoute_TurnCountAggregates verifies the worker-level
// turn_count aggregation: a plate seen across many turns reports the
// turn count for its [first_seen, last_seen] window via the existing
// CountTurnsInWindow query.
func TestProcessRoute_TurnCountAggregates(t *testing.T) {
	q := newFakeAggregatorQuerier()
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q.detectionsByRoute[aggKey("dongle1", "route1")] = []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 0, false),
		detectionRowAt(2, "plateA", t0.Add(10*time.Second), nil, 0, false),
		detectionRowAt(3, "plateA", t0.Add(30*time.Second), nil, 0, false),
	}
	// Three turns inside the window, one outside.
	q.turnsInWindow = []turnWindowEntry{
		{DongleID: "dongle1", Route: "route1", WindowStart: t0.Add(2 * time.Second), Count: 1},
		{DongleID: "dongle1", Route: "route1", WindowStart: t0.Add(15 * time.Second), Count: 1},
		{DongleID: "dongle1", Route: "route1", WindowStart: t0.Add(25 * time.Second), Count: 1},
		// This one falls AFTER last_seen and should not be counted.
		{DongleID: "dongle1", Route: "route1", WindowStart: t0.Add(60 * time.Second), Count: 1},
	}

	w := NewALPRAggregator(nil, q, &fakeAggregatorPool{}, nil, nil, nil)
	w.alprEnabledForTest = trueP()

	if err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	}); err != nil {
		t.Fatalf("processRoute: %v", err)
	}

	upserts := q.encounters[aggKey("dongle1", "route1")]
	if len(upserts) != 1 {
		t.Fatalf("want 1 encounter, got %d", len(upserts))
	}
	if upserts[0].TurnCount != 3 {
		t.Errorf("turn_count: want 3, got %d", upserts[0].TurnCount)
	}
}

// TestProcessRoute_TurnCountFallsBackToZero verifies the documented
// race fallback: when route_turns hasn't been populated, the worker
// records turn_count=0 and proceeds rather than failing the
// aggregation.
func TestProcessRoute_TurnCountFallsBackToZero(t *testing.T) {
	q := newFakeAggregatorQuerier()
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q.detectionsByRoute[aggKey("dongle1", "route1")] = []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 0, false),
		detectionRowAt(2, "plateA", t0.Add(time.Second), nil, 0, false),
	}
	// Empty turnsInWindow -> CountTurnsInWindow returns 0.

	w := NewALPRAggregator(nil, q, &fakeAggregatorPool{}, nil, nil, nil)
	w.alprEnabledForTest = trueP()

	if err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	}); err != nil {
		t.Fatalf("processRoute: %v", err)
	}

	upserts := q.encounters[aggKey("dongle1", "route1")]
	if len(upserts) != 1 {
		t.Fatalf("want 1 encounter, got %d", len(upserts))
	}
	if upserts[0].TurnCount != 0 {
		t.Errorf("turn_count fallback: want 0, got %d", upserts[0].TurnCount)
	}
}

// TestProcessRoute_IdempotentReRun verifies that running the
// aggregator twice over the same route produces identical state. The
// DELETE-then-INSERT sequence inside one transaction should be a
// no-op result-wise on the second run.
func TestProcessRoute_IdempotentReRun(t *testing.T) {
	q := newFakeAggregatorQuerier()
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q.detectionsByRoute[aggKey("dongle1", "route1")] = []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 5, true),
		detectionRowAt(2, "plateA", t0.Add(2*time.Second), nil, 5, true),
		detectionRowAt(3, "plateB", t0.Add(3*time.Second), nil, 0, false),
		// 90s gap creates a second plateA encounter.
		detectionRowAt(4, "plateA", t0.Add(95*time.Second), nil, 5, true),
	}

	pool := &fakeAggregatorPool{}
	w := NewALPRAggregator(nil, q, pool, nil, nil, nil)
	w.alprEnabledForTest = trueP()

	first := []db.UpsertEncounterParams{}
	if err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	}); err != nil {
		t.Fatalf("first processRoute: %v", err)
	}
	first = append(first, q.encounters[aggKey("dongle1", "route1")]...)

	if err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	}); err != nil {
		t.Fatalf("second processRoute: %v", err)
	}
	second := q.encounters[aggKey("dongle1", "route1")]

	if !sameEncounterSet(first, second) {
		t.Errorf("idempotent re-run produced different encounter set\nfirst:  %+v\nsecond: %+v", first, second)
	}
	// DELETE called once per pass.
	if len(q.deletedRoutes) != 2 {
		t.Errorf("DeleteEncountersForRoute calls: want 2, got %d", len(q.deletedRoutes))
	}
	// Two transactions.
	if pool.calls != 2 {
		t.Errorf("Pool.Begin calls: want 2, got %d", pool.calls)
	}
	// Second tx committed (we test commit indirectly by observing the
	// fakePool's last tx state).
	if len(pool.txs) < 2 || !pool.txs[1].committed {
		t.Errorf("second tx should be committed, got %+v", pool.txs)
	}
}

// sameEncounterSet returns true when two slices of UpsertEncounterParams
// are equal as a set keyed on (plate_hash, first_seen_ts) plus the
// invariant fields. Used by the idempotency test.
func sameEncounterSet(a, b []db.UpsertEncounterParams) bool {
	if len(a) != len(b) {
		return false
	}
	keyOf := func(p db.UpsertEncounterParams) string {
		return string(p.PlateHash) + "|" + p.FirstSeenTs.Time.UTC().Format(time.RFC3339Nano)
	}
	sort.Slice(a, func(i, j int) bool { return keyOf(a[i]) < keyOf(a[j]) })
	sort.Slice(b, func(i, j int) bool { return keyOf(b[i]) < keyOf(b[j]) })
	for i := range a {
		ai, bi := a[i], b[i]
		if !bytes.Equal(ai.PlateHash, bi.PlateHash) ||
			!ai.FirstSeenTs.Time.Equal(bi.FirstSeenTs.Time) ||
			!ai.LastSeenTs.Time.Equal(bi.LastSeenTs.Time) ||
			ai.DetectionCount != bi.DetectionCount ||
			ai.TurnCount != bi.TurnCount ||
			ai.MaxInternalGapSeconds != bi.MaxInternalGapSeconds ||
			ai.SignatureID.Valid != bi.SignatureID.Valid ||
			(ai.SignatureID.Valid && ai.SignatureID.Int64 != bi.SignatureID.Int64) ||
			ai.Status != bi.Status ||
			!bytes.Equal(ai.BboxFirst, bi.BboxFirst) ||
			!bytes.Equal(ai.BboxLast, bi.BboxLast) {
			return false
		}
	}
	return true
}

// TestProcessRoute_EmitsEncountersUpdated verifies the post-write
// signal: after persistence the worker emits an EncountersUpdated
// event whose PlateHashesAffected lists exactly the plates with
// encounter rows.
func TestProcessRoute_EmitsEncountersUpdated(t *testing.T) {
	q := newFakeAggregatorQuerier()
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q.detectionsByRoute[aggKey("dongle1", "route1")] = []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 0, false),
		detectionRowAt(2, "plateB", t0.Add(time.Second), nil, 0, false),
	}

	updates := make(chan EncountersUpdated, 4)
	w := NewALPRAggregator(nil, q, &fakeAggregatorPool{}, nil, nil, updates)
	w.alprEnabledForTest = trueP()

	if err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	}); err != nil {
		t.Fatalf("processRoute: %v", err)
	}

	select {
	case ev := <-updates:
		if ev.DongleID != "dongle1" || ev.Route != "route1" {
			t.Errorf("event identity: got %+v", ev)
		}
		if len(ev.PlateHashesAffected) != 2 {
			t.Fatalf("PlateHashesAffected: want 2, got %d", len(ev.PlateHashesAffected))
		}
		// uniquePlateHashes sorts lexicographically.
		if !bytes.Equal(ev.PlateHashesAffected[0], []byte("plateA")) ||
			!bytes.Equal(ev.PlateHashesAffected[1], []byte("plateB")) {
			t.Errorf("PlateHashesAffected: want [plateA, plateB], got %v", ev.PlateHashesAffected)
		}
	default:
		t.Fatalf("no EncountersUpdated event was emitted")
	}
}

// TestRun_DropsEventsWhenALPRDisabled verifies that events arriving
// while the gate is off are silently discarded -- we never aggregate
// stale data after the operator turned ALPR off.
func TestRun_DropsEventsWhenALPRDisabled(t *testing.T) {
	q := newFakeAggregatorQuerier()
	q.detectionsByRoute[aggKey("dongle1", "route1")] = []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", time.Now(), nil, 0, false),
	}

	completions := make(chan RouteAlprDetectionsComplete, 1)
	updates := make(chan EncountersUpdated, 1)
	w := NewALPRAggregator(completions, q, &fakeAggregatorPool{}, nil, nil, updates)
	// Explicitly disabled.
	disabled := false
	w.alprEnabledForTest = &disabled

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	completions <- RouteAlprDetectionsComplete{DongleID: "dongle1", Route: "route1"}
	// Give the worker a chance to process (or not).
	time.Sleep(20 * time.Millisecond)

	cancel()
	<-done

	if got := len(q.encounters[aggKey("dongle1", "route1")]); got != 0 {
		t.Errorf("encounters written while disabled: want 0, got %d", got)
	}
	select {
	case ev := <-updates:
		t.Errorf("EncountersUpdated emitted while disabled: %+v", ev)
	default:
	}
}

// TestRun_ProcessesEventsViaChannel verifies the end-to-end Run loop:
// an event sent on the completions channel is picked up, the route is
// aggregated, and an EncountersUpdated event is emitted.
func TestRun_ProcessesEventsViaChannel(t *testing.T) {
	q := newFakeAggregatorQuerier()
	t0 := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q.detectionsByRoute[aggKey("dongle1", "route1")] = []db.ListDetectionsForRouteRow{
		detectionRowAt(1, "plateA", t0, nil, 0, false),
		detectionRowAt(2, "plateA", t0.Add(time.Second), nil, 0, false),
	}

	completions := make(chan RouteAlprDetectionsComplete, 1)
	updates := make(chan EncountersUpdated, 1)
	w := NewALPRAggregator(completions, q, &fakeAggregatorPool{}, nil, nil, updates)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	completions <- RouteAlprDetectionsComplete{DongleID: "dongle1", Route: "route1"}

	select {
	case ev := <-updates:
		if ev.DongleID != "dongle1" || ev.Route != "route1" {
			t.Errorf("event identity: got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for EncountersUpdated")
	}

	cancel()
	<-done

	if got := len(q.encounters[aggKey("dongle1", "route1")]); got != 1 {
		t.Errorf("want 1 encounter persisted, got %d", got)
	}
}

// TestProcessRoute_NoDetectionsStillRunsCleanly verifies that a route
// with zero detections is a no-op: DELETE runs (clears any prior
// state) and an empty EncountersUpdated event is emitted. The
// histograms still observe a per-route value (0).
func TestProcessRoute_NoDetectionsStillRunsCleanly(t *testing.T) {
	q := newFakeAggregatorQuerier()
	updates := make(chan EncountersUpdated, 1)
	w := NewALPRAggregator(nil, q, &fakeAggregatorPool{}, nil, nil, updates)
	w.alprEnabledForTest = trueP()

	if err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	}); err != nil {
		t.Fatalf("processRoute: %v", err)
	}
	if got := len(q.encounters[aggKey("dongle1", "route1")]); got != 0 {
		t.Errorf("want 0 encounters, got %d", got)
	}
	if len(q.deletedRoutes) != 1 {
		t.Errorf("DeleteEncountersForRoute should still run; calls: %d", len(q.deletedRoutes))
	}
	select {
	case ev := <-updates:
		if len(ev.PlateHashesAffected) != 0 {
			t.Errorf("PlateHashesAffected: want empty, got %v", ev.PlateHashesAffected)
		}
	default:
		t.Errorf("EncountersUpdated event should still be emitted on empty routes")
	}
}

// TestProcessRoute_PropagatesListError verifies that an unrecoverable
// list-detections error surfaces as a returned error so the caller can
// log it. Persistence is NOT attempted.
func TestProcessRoute_PropagatesListError(t *testing.T) {
	q := newFakeAggregatorQuerier()
	q.listErr = errors.New("boom")
	w := NewALPRAggregator(nil, q, &fakeAggregatorPool{}, nil, nil, nil)
	w.alprEnabledForTest = trueP()

	err := w.processRoute(context.Background(), RouteAlprDetectionsComplete{
		DongleID: "dongle1",
		Route:    "route1",
	})
	if err == nil {
		t.Fatalf("want error from processRoute, got nil")
	}
	if !errors.Is(err, q.listErr) {
		t.Errorf("error chain: want listErr in chain, got %v", err)
	}
	if len(q.deletedRoutes) != 0 {
		t.Errorf("DeleteEncountersForRoute should not have been called: %d", len(q.deletedRoutes))
	}
}

// TestEncounterGap_ClampsBelowMinimum verifies that a misconfigured
// (zero or negative) settings value falls back to the worker default
// rather than collapsing every detection into its own encounter.
func TestEncounterGap_ClampsBelowMinimum(t *testing.T) {
	w := NewALPRAggregator(nil, nil, nil, nil, nil, nil)
	// No Settings store; encounterGap should return the struct
	// default unchanged.
	w.DefaultEncounterGapSeconds = 60.0
	got := w.encounterGap(context.Background())
	if got != 60.0 {
		t.Errorf("encounterGap: want default 60, got %g", got)
	}

	// A misconfigured struct default falls back to the package
	// default constant.
	w.DefaultEncounterGapSeconds = 0
	got = w.encounterGap(context.Background())
	if got != DefaultALPREncounterGapSeconds {
		t.Errorf("encounterGap with bad default: want %g, got %g", DefaultALPREncounterGapSeconds, got)
	}
}
