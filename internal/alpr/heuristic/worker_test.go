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

	// fusion-layer state. These are populated only by tests that
	// exercise the fusion path; default empty values produce the
	// "no signatures, no fusion output" path.
	detectionsByPlate          map[string][]db.CountDetectionsBySignatureForPlateRow
	signaturesByID             map[int64]db.VehicleSignature
	platesForSignatureInWindow map[int64][]db.ListPlateHashesForSignatureInWindowRow
	signatureSwapWatchlist     map[int64]*db.PlateWatchlist
	swapUpsertCalls            []db.UpsertWatchlistSignatureSwapParams

	// failure injection
	upsertErr error
	listErr   error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		encountersByHash:           make(map[string][]db.ListEncountersForPlateInWindowWithStartGPSRow),
		watchlist:                  make(map[string]*db.GetWatchlistByHashRow),
		detectionsByPlate:          make(map[string][]db.CountDetectionsBySignatureForPlateRow),
		signaturesByID:             make(map[int64]db.VehicleSignature),
		platesForSignatureInWindow: make(map[int64][]db.ListPlateHashesForSignatureInWindowRow),
		signatureSwapWatchlist:     make(map[int64]*db.PlateWatchlist),
	}
}

// --- fusion-layer fakes ---

func (f *fakeQuerier) CountDetectionsBySignatureForPlate(_ context.Context, plateHash []byte) ([]db.CountDetectionsBySignatureForPlateRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.CountDetectionsBySignatureForPlateRow(nil), f.detectionsByPlate[string(plateHash)]...), nil
}

func (f *fakeQuerier) GetSignature(_ context.Context, id int64) (db.VehicleSignature, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.signaturesByID[id]
	if !ok {
		return db.VehicleSignature{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeQuerier) ListPlatesForSignature(_ context.Context, signatureID pgtype.Int8) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.platesForSignatureInWindow[signatureID.Int64]
	seen := make(map[string]struct{}, len(rows))
	out := make([][]byte, 0, len(rows))
	for _, r := range rows {
		k := string(r.PlateHash)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, append([]byte(nil), r.PlateHash...))
	}
	return out, nil
}

func (f *fakeQuerier) ListPlateHashesForSignatureInWindow(_ context.Context, arg db.ListPlateHashesForSignatureInWindowParams) ([]db.ListPlateHashesForSignatureInWindowRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := f.platesForSignatureInWindow[arg.SignatureID.Int64]
	out := make([]db.ListPlateHashesForSignatureInWindowRow, 0, len(rows))
	for _, r := range rows {
		if r.LastSeenTs.Valid {
			if r.LastSeenTs.Time.Before(arg.WindowStart.Time) {
				continue
			}
			if r.LastSeenTs.Time.After(arg.WindowEnd.Time) {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeQuerier) GetWatchlistSignatureSwap(_ context.Context, signatureID pgtype.Int8) (db.PlateWatchlist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.signatureSwapWatchlist[signatureID.Int64]
	if !ok {
		return db.PlateWatchlist{}, pgx.ErrNoRows
	}
	return *row, nil
}

func (f *fakeQuerier) UpsertWatchlistSignatureSwap(_ context.Context, arg db.UpsertWatchlistSignatureSwapParams) (db.PlateWatchlist, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.swapUpsertCalls = append(f.swapUpsertCalls, arg)
	if existing, ok := f.signatureSwapWatchlist[arg.SignatureID.Int64]; ok {
		// GREATEST() semantics: never demote.
		if !existing.Severity.Valid || arg.Severity.Int16 > existing.Severity.Int16 {
			existing.Severity = arg.Severity
		}
		existing.LastAlertAt = arg.AlertAt
		existing.Kind = "alerted"
		return *existing, nil
	}
	row := &db.PlateWatchlist{
		ID:           int64(len(f.signatureSwapWatchlist) + 1),
		Kind:         "alerted",
		Severity:     arg.Severity,
		FirstAlertAt: arg.AlertAt,
		LastAlertAt:  arg.AlertAt,
		SignatureID:  arg.SignatureID,
	}
	f.signatureSwapWatchlist[arg.SignatureID.Int64] = row
	return *row, nil
}

// seedDetectionCount records (signature_id, count) pairs returned by
// CountDetectionsBySignatureForPlate. sigID == 0 records the
// missing-signature row.
func (f *fakeQuerier) seedDetectionCount(plateHash []byte, sigID int64, count int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := db.CountDetectionsBySignatureForPlateRow{DetectionCount: count}
	if sigID > 0 {
		row.SignatureID = pgtype.Int8{Int64: sigID, Valid: true}
	}
	f.detectionsByPlate[string(plateHash)] = append(f.detectionsByPlate[string(plateHash)], row)
}

// seedSignaturePlateInWindow seeds an encounter row used by the
// fusion plate-swap query. Cell coordinates are integer bucket
// indices; the heuristic does not require they match the configured
// cellLatDeg/cellLngDeg arithmetic for tests.
func (f *fakeQuerier) seedSignaturePlateInWindow(sigID int64, plateHash []byte, cellLat, cellLng int64, gpsLat, gpsLng float64, last time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := db.ListPlateHashesForSignatureInWindowRow{
		PlateHash:   append([]byte(nil), plateHash...),
		CellLat:     cellLat,
		CellLng:     cellLng,
		EncounterID: int64(len(f.platesForSignatureInWindow[sigID]) + 1),
		LastSeenTs:  pgtype.Timestamptz{Time: last, Valid: true},
		GpsLat:      pgtype.Float8{Float64: gpsLat, Valid: true},
		GpsLng:      pgtype.Float8{Float64: gpsLng, Valid: true},
	}
	f.platesForSignatureInWindow[sigID] = append(f.platesForSignatureInWindow[sigID], row)
}

func (f *fakeQuerier) seedSignature(id int64, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signaturesByID[id] = db.VehicleSignature{
		ID:           id,
		SignatureKey: key,
		SampleCount:  10,
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

// --- fusion-layer integration through the worker ---

// TestWorker_FusionPlateSwap_EmitsSignatureKeyedAlert exercises the
// end-to-end fusion path: a plate with a dominant signature that
// shares an area cell with two other plates triggers a signature-
// keyed AlertCreated, with a non-nil SignatureID and the chain of
// plate hashes, and writes a plate_watchlist row keyed on
// signature_id (plate_hash IS NULL).
func TestWorker_FusionPlateSwap_EmitsSignatureKeyedAlert(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x80}
	// Stalking heuristic input: a routine encounter that scores 0,
	// so the plate-keyed AlertCreated path is not in play. The
	// fusion alert must emit independently.
	q.seedEncounter(plate, "r1", baseTime.Add(-1*time.Hour), baseTime.Add(-50*time.Minute), 0, 0, 0, false)

	// Fusion: plate dominantly under signature 9 (10/10 detections).
	q.seedDetectionCount(plate, 9, 10)
	q.seedSignature(9, "honda|civic|black|sedan")

	// Three distinct plate hashes share signature 9 in cell (50,50).
	for _, ph := range [][]byte{{0xa1}, {0xa2}, {0xa3}} {
		q.seedSignaturePlateInWindow(9, ph, 50, 50, 40.0, -73.0, baseTime.Add(-2*time.Hour))
	}

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}

	// AlertCreated must arrive with SignatureID set and PlateHash empty.
	select {
	case ev := <-alerts:
		if ev.SignatureID == nil {
			t.Fatalf("SignatureID should be set on signature-swap alert; got %+v", ev)
		}
		if *ev.SignatureID != 9 {
			t.Fatalf("SignatureID: got=%d want=9", *ev.SignatureID)
		}
		if len(ev.PlateHash) != 0 {
			t.Fatalf("PlateHash should be empty on signature-swap alert; got %x", ev.PlateHash)
		}
		if len(ev.PlateHashes) != 3 {
			t.Fatalf("PlateHashes chain: got=%d want=3", len(ev.PlateHashes))
		}
		if ev.Severity != DefaultPlateSwapSeverity {
			t.Fatalf("severity: got=%d want=%d", ev.Severity, DefaultPlateSwapSeverity)
		}
	default:
		t.Fatal("expected signature-swap AlertCreated")
	}

	// Signature-keyed watchlist row must exist.
	row := q.signatureSwapWatchlist[9]
	if row == nil {
		t.Fatal("expected signature-keyed watchlist row")
	}
	if row.SignatureID.Int64 != 9 {
		t.Fatalf("watchlist signature_id: got=%d want=9", row.SignatureID.Int64)
	}
	if !row.Severity.Valid || row.Severity.Int16 != int16(DefaultPlateSwapSeverity) {
		t.Fatalf("watchlist severity: got=%+v want=%d", row.Severity, DefaultPlateSwapSeverity)
	}
}

// TestWorker_FusionPlateSwap_RefireSilentOnSameSeverity ensures that a
// re-evaluation of a plate that re-discovers the same swap alert at
// the same severity does NOT emit a duplicate AlertCreated. The
// watchlist row's last_alert_at should still refresh.
func TestWorker_FusionPlateSwap_RefireSilentOnSameSeverity(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x81}
	q.seedEncounter(plate, "r1", baseTime.Add(-1*time.Hour), baseTime.Add(-50*time.Minute), 0, 0, 0, false)
	q.seedDetectionCount(plate, 11, 10)
	q.seedSignature(11, "ford|f150|red|truck")
	for _, ph := range [][]byte{{0xb1}, {0xb2}, {0xb3}} {
		q.seedSignaturePlateInWindow(11, ph, 60, 60, 41.0, -74.0, baseTime.Add(-3*time.Hour))
	}

	// Pre-existing signature-swap row at the same severity.
	q.signatureSwapWatchlist[11] = &db.PlateWatchlist{
		ID:           99,
		Kind:         "alerted",
		Severity:     pgtype.Int2{Int16: int16(DefaultPlateSwapSeverity), Valid: true},
		FirstAlertAt: pgtype.Timestamptz{Time: baseTime.Add(-7 * 24 * time.Hour), Valid: true},
		LastAlertAt:  pgtype.Timestamptz{Time: baseTime.Add(-7 * 24 * time.Hour), Valid: true},
		SignatureID:  pgtype.Int8{Int64: 11, Valid: true},
	}

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}

	// No AlertCreated should be emitted on a same-severity re-fire.
	select {
	case ev := <-alerts:
		t.Fatalf("did not expect AlertCreated on swap re-fire, got %+v", ev)
	default:
	}

	// last_alert_at must refresh.
	row := q.signatureSwapWatchlist[11]
	if !row.LastAlertAt.Time.Equal(baseTime) {
		t.Fatalf("last_alert_at: got=%v want=%v", row.LastAlertAt.Time, baseTime)
	}
}

// TestWorker_FusionConsistencyBumpsPlateSeverity exercises the
// plate-confirmation path: the stalking heuristic scores 3.5 (below
// severity 3 at 4.0), the fusion layer adds +0.5 for signature
// consistency, the combined score crosses 4.0 and severity promotes.
func TestWorker_FusionConsistencyBumpsPlateSeverity(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x82}
	// Stalking signal: turns 4 -> 2pt + persistence high 1.5 = 3.5
	// (severity 2). Add 0.5 from fusion -> 4.0, severity 3.
	q.seedEncounter(plate, "r1", baseTime, baseTime.Add(16*time.Minute), 4, 0, 0, false)

	// Fusion: 100% of detections under signature 13, raises +0.5.
	q.seedDetectionCount(plate, 13, 10)
	q.seedSignature(13, "subaru|outback|silver|wagon")

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}
	wl := q.watchlist[string(plate)]
	if wl == nil {
		t.Fatal("expected watchlist row")
	}
	if !wl.Severity.Valid || wl.Severity.Int16 != 3 {
		t.Fatalf("severity should promote to 3 with fusion bump; got=%+v", wl.Severity)
	}

	// Audit row should record the boosted severity AND include the
	// fusion components.
	if len(q.alertEvents) != 1 {
		t.Fatalf("want 1 audit row, got %d", len(q.alertEvents))
	}
	ev := q.alertEvents[0]
	if ev.Severity != 3 {
		t.Fatalf("audit severity: got=%d want=3", ev.Severity)
	}
	if !bytes.Contains(ev.Components, []byte("signature_consistent")) {
		t.Fatalf("expected signature_consistent in components, got %s", ev.Components)
	}
}

// TestWorker_FusionWhitelistedPlateSkipsFusion ensures the worker
// short-circuits the fusion layer when the plate itself is
// whitelisted. The plate-keyed path was already suppressed; the
// fusion layer must not write fusion components or raise swap alerts
// for a whitelisted plate.
func TestWorker_FusionWhitelistedPlateSkipsFusion(t *testing.T) {
	q := newFakeQuerier()
	alerts := make(chan AlertCreated, 4)
	w := NewWorker(nil, q, nil, nil, alerts)
	w.alprEnabledForTest = enabledTrue()
	w.Now = func() time.Time { return baseTime }

	plate := []byte{0x83}
	q.seedWatchlist(plate, "whitelist", 0, time.Time{})
	q.seedEncounter(plate, "r1", baseTime, baseTime.Add(16*time.Minute), 4, 0, 0, false)
	q.seedDetectionCount(plate, 17, 10)
	q.seedSignature(17, "tesla|model3|white|sedan")
	for _, ph := range [][]byte{{0xc1}, {0xc2}, {0xc3}} {
		q.seedSignaturePlateInWindow(17, ph, 70, 70, 42.0, -75.0, baseTime.Add(-2*time.Hour))
	}

	if err := w.EvaluatePlate(context.Background(), plate, "r1", "dongleA"); err != nil {
		t.Fatal(err)
	}

	// No AlertCreated, no signature-swap watchlist row, no fusion
	// components in the audit blob.
	select {
	case ev := <-alerts:
		t.Fatalf("whitelisted plate should not emit alerts, got %+v", ev)
	default:
	}
	if len(q.signatureSwapWatchlist) != 0 {
		t.Fatalf("whitelisted plate should not produce signature-swap rows, got %d", len(q.signatureSwapWatchlist))
	}
	if len(q.alertEvents) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(q.alertEvents))
	}
	if bytes.Contains(q.alertEvents[0].Components, []byte("signature_consistent")) {
		t.Fatalf("did not expect fusion components on whitelisted path: %s", q.alertEvents[0].Components)
	}
	if bytes.Contains(q.alertEvents[0].Components, []byte("plate_swap")) {
		t.Fatalf("did not expect plate_swap on whitelisted path: %s", q.alertEvents[0].Components)
	}
}
