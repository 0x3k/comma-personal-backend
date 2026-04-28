package heuristic

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// fakeQuerier is a minimal in-memory HeuristicQuerier. It models the
// invariants we care about for the worker layer:
//
//   - GetWatchlistByHash returns pgx.ErrNoRows when no row exists (real
//     pgx behaviour) and the stored row otherwise.
//   - UpsertWatchlistAlerted and UpsertWatchlistAlertedPreserveAck both
//     UPSERT keyed on plate_hash; the first uses GREATEST-style severity
//     and clears acked_at on update, the second preserves acked_at.
//   - InsertAlertEvent always succeeds (it's an audit append).
type fakeQuerier struct {
	mu sync.Mutex

	encountersByHash map[string][]db.ListEncountersForPlateInWindowWithStartGPSRow
	watchlist        map[string]*db.GetWatchlistByHashRow
	alertEvents      []db.InsertAlertEventParams

	// failure injection
	upsertErr error
	listErr   error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		encountersByHash: make(map[string][]db.ListEncountersForPlateInWindowWithStartGPSRow),
		watchlist:        make(map[string]*db.GetWatchlistByHashRow),
	}
}

func (f *fakeQuerier) ListEncountersForPlateInWindowWithStartGPS(_ context.Context, arg db.ListEncountersForPlateInWindowWithStartGPSParams) ([]db.ListEncountersForPlateInWindowWithStartGPSRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	rows := f.encountersByHash[string(arg.PlateHash)]
	out := make([]db.ListEncountersForPlateInWindowWithStartGPSRow, 0, len(rows))
	for _, r := range rows {
		// Match the real query's window filter (last_seen_ts >=
		// arg.LastSeenTs AND first_seen_ts <= arg.FirstSeenTs).
		if !r.LastSeenTs.Valid || !r.FirstSeenTs.Valid {
			continue
		}
		if r.LastSeenTs.Time.Before(arg.LastSeenTs.Time) {
			continue
		}
		if r.FirstSeenTs.Time.After(arg.FirstSeenTs.Time) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeQuerier) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.watchlist[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return *row, nil
}

func (f *fakeQuerier) UpsertWatchlistAlerted(_ context.Context, arg db.UpsertWatchlistAlertedParams) (db.UpsertWatchlistAlertedRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return db.UpsertWatchlistAlertedRow{}, f.upsertErr
	}
	key := string(arg.PlateHash)
	if existing, ok := f.watchlist[key]; ok {
		existing.Kind = "alerted"
		existing.Severity = arg.Severity
		existing.LastAlertAt = arg.AlertAt
		// Clear ack on upgrade (matches the existing query).
		existing.AckedAt = pgtype.Timestamptz{}
		return db.UpsertWatchlistAlertedRow{
			ID: existing.ID, PlateHash: existing.PlateHash, Kind: existing.Kind,
			Severity: existing.Severity, FirstAlertAt: existing.FirstAlertAt,
			LastAlertAt: existing.LastAlertAt, AckedAt: existing.AckedAt,
		}, nil
	}
	row := &db.GetWatchlistByHashRow{
		ID:           int64(len(f.watchlist) + 1),
		PlateHash:    append([]byte(nil), arg.PlateHash...),
		Kind:         "alerted",
		Severity:     arg.Severity,
		FirstAlertAt: arg.AlertAt,
		LastAlertAt:  arg.AlertAt,
	}
	f.watchlist[key] = row
	return db.UpsertWatchlistAlertedRow{
		ID: row.ID, PlateHash: row.PlateHash, Kind: row.Kind, Severity: row.Severity,
		FirstAlertAt: row.FirstAlertAt, LastAlertAt: row.LastAlertAt,
	}, nil
}

func (f *fakeQuerier) UpsertWatchlistAlertedPreserveAck(_ context.Context, arg db.UpsertWatchlistAlertedPreserveAckParams) (db.UpsertWatchlistAlertedPreserveAckRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return db.UpsertWatchlistAlertedPreserveAckRow{}, f.upsertErr
	}
	key := string(arg.PlateHash)
	if existing, ok := f.watchlist[key]; ok {
		existing.Kind = "alerted"
		// GREATEST() semantics: never demote.
		if !existing.Severity.Valid || arg.Severity.Int16 > existing.Severity.Int16 {
			existing.Severity = arg.Severity
		}
		existing.LastAlertAt = arg.AlertAt
		// acked_at preserved (no-op).
		return db.UpsertWatchlistAlertedPreserveAckRow{
			ID: existing.ID, PlateHash: existing.PlateHash, Kind: existing.Kind,
			Severity: existing.Severity, FirstAlertAt: existing.FirstAlertAt,
			LastAlertAt: existing.LastAlertAt, AckedAt: existing.AckedAt,
		}, nil
	}
	row := &db.GetWatchlistByHashRow{
		ID:           int64(len(f.watchlist) + 1),
		PlateHash:    append([]byte(nil), arg.PlateHash...),
		Kind:         "alerted",
		Severity:     arg.Severity,
		FirstAlertAt: arg.AlertAt,
		LastAlertAt:  arg.AlertAt,
	}
	f.watchlist[key] = row
	return db.UpsertWatchlistAlertedPreserveAckRow{
		ID: row.ID, PlateHash: row.PlateHash, Kind: row.Kind, Severity: row.Severity,
		FirstAlertAt: row.FirstAlertAt, LastAlertAt: row.LastAlertAt,
	}, nil
}

func (f *fakeQuerier) InsertAlertEvent(_ context.Context, arg db.InsertAlertEventParams) (db.PlateAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alertEvents = append(f.alertEvents, arg)
	return db.PlateAlertEvent{
		ID: int64(len(f.alertEvents)), PlateHash: arg.PlateHash, Severity: arg.Severity,
		Components: arg.Components, HeuristicVersion: arg.HeuristicVersion,
	}, nil
}

// seedEncounter adds an encounter row the fake will return from
// ListEncountersForPlateInWindowWithStartGPS.
func (f *fakeQuerier) seedEncounter(plateHash []byte, route string, first, last time.Time, turns int, lat, lng float64, hasGPS bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := db.ListEncountersForPlateInWindowWithStartGPSRow{
		ID:          int64(len(f.encountersByHash[string(plateHash)]) + 1),
		Route:       route,
		PlateHash:   append([]byte(nil), plateHash...),
		FirstSeenTs: pgtype.Timestamptz{Time: first, Valid: true},
		LastSeenTs:  pgtype.Timestamptz{Time: last, Valid: true},
		TurnCount:   int32(turns),
	}
	if hasGPS {
		row.StartLat = pgtype.Float8{Float64: lat, Valid: true}
		row.StartLng = pgtype.Float8{Float64: lng, Valid: true}
	}
	f.encountersByHash[string(plateHash)] = append(f.encountersByHash[string(plateHash)], row)
}

// seedWatchlist installs a watchlist row before the test runs.
func (f *fakeQuerier) seedWatchlist(plateHash []byte, kind string, severity int16, ackedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := &db.GetWatchlistByHashRow{
		ID:        int64(len(f.watchlist) + 1),
		PlateHash: append([]byte(nil), plateHash...),
		Kind:      kind,
		Severity:  pgtype.Int2{Int16: severity, Valid: severity > 0},
	}
	if !ackedAt.IsZero() {
		row.AckedAt = pgtype.Timestamptz{Time: ackedAt, Valid: true}
	}
	f.watchlist[string(plateHash)] = row
}

// fakeMetrics captures the calls so a test can assert metrics
// observability without spinning up a Prometheus registry.
type fakeMetrics struct {
	mu     sync.Mutex
	evals  int
	alerts map[int]int
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{alerts: make(map[int]int)}
}

func (m *fakeMetrics) ObserveALPRHeuristicEval(time.Duration) {
	m.mu.Lock()
	m.evals++
	m.mu.Unlock()
}

func (m *fakeMetrics) IncALPRHeuristicAlerts(severity int) {
	m.mu.Lock()
	m.alerts[severity]++
	m.mu.Unlock()
}

// --- tests ---

func enabledTrue() *bool { v := true; return &v }

func TestWorker_NoEncountersWritesAuditOnly(t *testing.T) {
	q := newFakeQuerier()
	w := NewWorker(nil, q, nil, nil, nil)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x01, 0x02}
	if err := w.EvaluatePlate(context.Background(), plate, "r", "dongleA"); err != nil {
		t.Fatal(err)
	}
	// One audit row, severity 0, no watchlist row.
	if len(q.alertEvents) != 1 {
		t.Fatalf("want 1 alert event, got %d", len(q.alertEvents))
	}
	if got := q.alertEvents[0].Severity; got != 0 {
		t.Fatalf("severity: got=%d want=0", got)
	}
	if got := q.alertEvents[0].HeuristicVersion; got != HeuristicVersion {
		t.Fatalf("heuristic_version: got=%q want=%q", got, HeuristicVersion)
	}
	if len(q.watchlist) != 0 {
		t.Fatal("severity 0 should not write a watchlist row")
	}
}

func TestWorker_NewAlertEmitsAlertCreated(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x10}
	// Score 4.0 -> severity 3 (turns 5 -> 3pt; persistence 9m -> 1.0).
	q.seedEncounter(plate, "r1", baseTime.Add(-30*time.Minute), baseTime.Add(-21*time.Minute), 5, 0, 0, false)

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}
	wl := q.watchlist[string(plate)]
	if wl == nil {
		t.Fatal("expected watchlist row")
	}
	if !wl.Severity.Valid || wl.Severity.Int16 != 3 {
		t.Fatalf("severity: got=%+v want=3", wl.Severity)
	}
	select {
	case ev := <-alerts:
		if ev.Severity != 3 {
			t.Fatalf("alert severity: got=%d want=3", ev.Severity)
		}
		if !bytes.Equal(ev.PlateHash, plate) {
			t.Fatalf("alert plate_hash: got=%x want=%x", ev.PlateHash, plate)
		}
	default:
		t.Fatal("expected AlertCreated event")
	}
}

func TestWorker_AckPreservedOnReEvaluation(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0xab}
	// Existing row: severity 3, acked an hour ago.
	ackTime := baseTime.Add(-1 * time.Hour)
	q.seedWatchlist(plate, "alerted", 3, ackTime)

	// Re-evaluation produces same severity 3.
	q.seedEncounter(plate, "r1", baseTime.Add(-30*time.Minute), baseTime.Add(-21*time.Minute), 5, 0, 0, false)

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}
	wl := q.watchlist[string(plate)]
	if !wl.AckedAt.Valid {
		t.Fatal("ack should be preserved on re-evaluation at same severity")
	}
	if !wl.AckedAt.Time.Equal(ackTime) {
		t.Fatalf("ack time changed: got=%v want=%v", wl.AckedAt.Time, ackTime)
	}
	// No AlertCreated emitted because severity didn't increase.
	select {
	case ev := <-alerts:
		t.Fatalf("did not expect AlertCreated on re-evaluation, got %+v", ev)
	default:
	}
}

func TestWorker_SeverityUpgradeClearsAck(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0xcd}
	// Existing row at severity 2, acked.
	ackTime := baseTime.Add(-1 * time.Hour)
	q.seedWatchlist(plate, "alerted", 2, ackTime)

	// Upgrade scenario: 5 routes (mid+high stack 2.5), persistence high
	// (1.5), turns 5 -> 3, geo spread 2.0 = 9.0 -> severity 5.
	for i, r := range []string{"r1", "r2", "r3", "r4", "r5"} {
		off := time.Duration(-30+5*i) * time.Hour
		q.seedEncounter(plate, r, baseTime.Add(off), baseTime.Add(off+20*time.Minute), 5,
			40.0+float64(i)*0.5, -73.0, true)
	}

	if err := w.EvaluatePlate(context.Background(), plate, "r5", "dongleA"); err != nil {
		t.Fatal(err)
	}
	wl := q.watchlist[string(plate)]
	if !wl.Severity.Valid || wl.Severity.Int16 < 3 {
		t.Fatalf("severity should have upgraded: got=%+v", wl.Severity)
	}
	if wl.AckedAt.Valid {
		t.Fatalf("ack should be cleared on strict upgrade; got=%+v", wl.AckedAt)
	}
	// AlertCreated emitted for the upgrade.
	select {
	case ev := <-alerts:
		if ev.Severity < 3 {
			t.Fatalf("upgrade alert severity: got=%d want>=3", ev.Severity)
		}
	default:
		t.Fatal("expected AlertCreated on upgrade")
	}
}

func TestWorker_NoEmitOnConfirmAtSameSeverity(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0xef}
	q.seedWatchlist(plate, "alerted", 3, time.Time{})
	q.seedEncounter(plate, "r1", baseTime.Add(-30*time.Minute), baseTime.Add(-21*time.Minute), 5, 0, 0, false)

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-alerts:
		t.Fatalf("did not expect AlertCreated on re-confirmation, got %+v", ev)
	default:
	}
}

func TestWorker_NeverDemotes(t *testing.T) {
	q := newFakeQuerier()
	w := NewWorker(nil, q, nil, nil, nil)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x11}
	q.seedWatchlist(plate, "alerted", 5, time.Time{})
	// Score will land at severity 3 (turns 5 -> 3pt + persistence 9m
	// -> 1.0 = 4.0).
	q.seedEncounter(plate, "r1", baseTime.Add(-30*time.Minute), baseTime.Add(-21*time.Minute), 5, 0, 0, false)

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}
	wl := q.watchlist[string(plate)]
	if !wl.Severity.Valid || wl.Severity.Int16 != 5 {
		t.Fatalf("severity should not demote: got=%+v want=5", wl.Severity)
	}
}

func TestWorker_WhitelistSuppression(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x22}
	q.seedWatchlist(plate, "whitelist", 0, time.Time{})
	// Strong signals -- would otherwise score severity 5.
	for i, r := range []string{"r1", "r2", "r3", "r4", "r5"} {
		off := time.Duration(-30+5*i) * time.Hour
		q.seedEncounter(plate, r, baseTime.Add(off), baseTime.Add(off+20*time.Minute), 5,
			40.0+float64(i)*0.5, -73.0, true)
	}

	if err := w.EvaluatePlate(context.Background(), plate, "r5", "dongleA"); err != nil {
		t.Fatal(err)
	}
	if len(q.alertEvents) != 1 {
		t.Fatalf("should still write audit row, got %d events", len(q.alertEvents))
	}
	if got := q.alertEvents[0].Severity; got != 0 {
		t.Fatalf("audit severity should be 0 (suppressed), got %d", got)
	}
	wl := q.watchlist[string(plate)]
	if wl.Kind != "whitelist" {
		t.Fatalf("kind should remain whitelist, got %q", wl.Kind)
	}
	if wl.Severity.Valid && wl.Severity.Int16 != 0 {
		t.Fatalf("whitelist severity should be NULL/0, got %+v", wl.Severity)
	}
	select {
	case ev := <-alerts:
		t.Fatalf("whitelisted plate should not emit AlertCreated, got %+v", ev)
	default:
	}
}

func TestWorker_DroppedWhenALPRDisabled(t *testing.T) {
	q := newFakeQuerier()
	in := make(chan EncountersUpdatedEvent, 4)
	w := NewWorker(in, q, nil, nil, nil)
	dis := false
	w.alprEnabledForTest = &dis
	w.Now = func() time.Time { return baseTime }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	plate := []byte{0x33}
	q.seedEncounter(plate, "r1", baseTime.Add(-30*time.Minute), baseTime.Add(-21*time.Minute), 5, 0, 0, false)
	in <- EncountersUpdatedEvent{DongleID: "d", Route: "r1", PlateHashesAffected: [][]byte{plate}}

	// Give the worker a tick to drain the event.
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done

	if len(q.alertEvents) != 0 {
		t.Fatalf("expected no audit rows when alpr disabled, got %d", len(q.alertEvents))
	}
}

func TestWorker_ListErrorPropagatesPerPlate(t *testing.T) {
	q := newFakeQuerier()
	q.listErr = errors.New("boom")
	w := NewWorker(nil, q, nil, nil, nil)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x44}
	err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA")
	if err == nil {
		t.Fatal("expected error when list fails")
	}
}

func TestWorker_RunProcessesAllAffectedPlates(t *testing.T) {
	q := newFakeQuerier()
	in := make(chan EncountersUpdatedEvent, 4)
	w := NewWorker(in, q, nil, nil, nil)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plates := [][]byte{{0x55}, {0x66}, {0x77}}
	for _, p := range plates {
		q.seedEncounter(p, "r1", baseTime.Add(-1*time.Hour), baseTime.Add(-50*time.Minute), 0, 0, 0, false)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	in <- EncountersUpdatedEvent{DongleID: "d", Route: "r1", PlateHashesAffected: plates}

	// Wait for all events to be processed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		n := len(q.alertEvents)
		q.mu.Unlock()
		if n == len(plates) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if got := len(q.alertEvents); got != len(plates) {
		t.Fatalf("alert events: got=%d want=%d", got, len(plates))
	}
}

func TestWorker_MetricsCalled(t *testing.T) {
	q := newFakeQuerier()
	m := newFakeMetrics()
	w := NewWorker(nil, q, nil, m, nil)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x99}
	q.seedEncounter(plate, "r1", baseTime.Add(-30*time.Minute), baseTime.Add(-21*time.Minute), 5, 0, 0, false)

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}
	if m.evals != 1 {
		t.Fatalf("evals: got=%d want=1", m.evals)
	}
	if m.alerts[3] != 1 {
		t.Fatalf("alerts[3]: got=%d want=1", m.alerts[3])
	}
}
