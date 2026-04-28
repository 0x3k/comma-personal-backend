package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/db"
)

// reevalFakeQuerier is the minimal HeuristicQuerier+distinct-plates
// stub the re-evaluate handler needs. The fake mirrors the
// production semantics of UPSERT + GREATEST so live-mode assertions
// can verify that watchlist rows actually move.
type reevalFakeQuerier struct {
	mu sync.Mutex

	encountersByHash map[string][]db.ListEncountersForPlateInWindowWithStartGPSRow
	watchlist        map[string]*db.GetWatchlistByHashRow
	alertEvents      []db.InsertAlertEventParams
	distinctPlates   []db.ListDistinctPlatesEncounteredInWindowRow
}

func newReevalFakeQuerier() *reevalFakeQuerier {
	return &reevalFakeQuerier{
		encountersByHash: make(map[string][]db.ListEncountersForPlateInWindowWithStartGPSRow),
		watchlist:        make(map[string]*db.GetWatchlistByHashRow),
	}
}

func (f *reevalFakeQuerier) ListEncountersForPlateInWindowWithStartGPS(_ context.Context, arg db.ListEncountersForPlateInWindowWithStartGPSParams) ([]db.ListEncountersForPlateInWindowWithStartGPSRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.ListEncountersForPlateInWindowWithStartGPSRow(nil), f.encountersByHash[string(arg.PlateHash)]...), nil
}

func (f *reevalFakeQuerier) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.watchlist[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return *row, nil
}

func (f *reevalFakeQuerier) UpsertWatchlistAlerted(_ context.Context, arg db.UpsertWatchlistAlertedParams) (db.UpsertWatchlistAlertedRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := string(arg.PlateHash)
	if existing, ok := f.watchlist[key]; ok {
		existing.Kind = "alerted"
		existing.Severity = arg.Severity
		existing.LastAlertAt = arg.AlertAt
		existing.AckedAt = pgtype.Timestamptz{}
		return db.UpsertWatchlistAlertedRow{ID: existing.ID, PlateHash: existing.PlateHash, Kind: existing.Kind, Severity: existing.Severity, FirstAlertAt: existing.FirstAlertAt, LastAlertAt: existing.LastAlertAt}, nil
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
	return db.UpsertWatchlistAlertedRow{ID: row.ID, PlateHash: row.PlateHash, Kind: row.Kind, Severity: row.Severity, FirstAlertAt: row.FirstAlertAt, LastAlertAt: row.LastAlertAt}, nil
}

func (f *reevalFakeQuerier) UpsertWatchlistAlertedPreserveAck(_ context.Context, arg db.UpsertWatchlistAlertedPreserveAckParams) (db.UpsertWatchlistAlertedPreserveAckRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := string(arg.PlateHash)
	if existing, ok := f.watchlist[key]; ok {
		existing.Kind = "alerted"
		if !existing.Severity.Valid || arg.Severity.Int16 > existing.Severity.Int16 {
			existing.Severity = arg.Severity
		}
		existing.LastAlertAt = arg.AlertAt
		return db.UpsertWatchlistAlertedPreserveAckRow{ID: existing.ID, PlateHash: existing.PlateHash, Kind: existing.Kind, Severity: existing.Severity, FirstAlertAt: existing.FirstAlertAt, LastAlertAt: existing.LastAlertAt}, nil
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
	return db.UpsertWatchlistAlertedPreserveAckRow{ID: row.ID, PlateHash: row.PlateHash, Kind: row.Kind, Severity: row.Severity, FirstAlertAt: row.FirstAlertAt, LastAlertAt: row.LastAlertAt}, nil
}

func (f *reevalFakeQuerier) InsertAlertEvent(_ context.Context, arg db.InsertAlertEventParams) (db.PlateAlertEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alertEvents = append(f.alertEvents, arg)
	return db.PlateAlertEvent{ID: int64(len(f.alertEvents)), PlateHash: arg.PlateHash, Severity: arg.Severity}, nil
}

func (f *reevalFakeQuerier) ListDistinctPlatesEncounteredInWindow(_ context.Context, _ db.ListDistinctPlatesEncounteredInWindowParams) ([]db.ListDistinctPlatesEncounteredInWindowRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.ListDistinctPlatesEncounteredInWindowRow(nil), f.distinctPlates...), nil
}

// seedEncountersStrong installs an encounter set rich enough to cross
// the default severity-3 threshold (cross-route count + persistence
// + turn count). The plate hash is reused as the dongle id so the
// seeded fake is self-consistent with ListDistinctPlatesEncounteredInWindow.
func (f *reevalFakeQuerier) seedEncountersStrong(plateHash []byte, dongleID string, now time.Time) {
	for i := 0; i < 5; i++ {
		first := now.Add(time.Duration(-i) * 24 * time.Hour)
		row := db.ListEncountersForPlateInWindowWithStartGPSRow{
			ID:          int64(i + 1),
			Route:       "route-" + string(rune('a'+i)),
			DongleID:    dongleID,
			PlateHash:   append([]byte(nil), plateHash...),
			FirstSeenTs: pgtype.Timestamptz{Time: first, Valid: true},
			LastSeenTs:  pgtype.Timestamptz{Time: first.Add(20 * time.Minute), Valid: true},
			TurnCount:   6,
			StartLat:    pgtype.Float8{Float64: 37.7 + float64(i)*0.5, Valid: true},
			StartLng:    pgtype.Float8{Float64: -122.4 + float64(i)*0.5, Valid: true},
		}
		f.encountersByHash[string(plateHash)] = append(f.encountersByHash[string(plateHash)], row)
	}
	f.distinctPlates = append(f.distinctPlates, db.ListDistinctPlatesEncounteredInWindowRow{
		DongleID:  dongleID,
		PlateHash: append([]byte(nil), plateHash...),
	})
}

func newReevalHandler(t *testing.T, q *reevalFakeQuerier) *ALPRReevaluateHandler {
	t.Helper()
	w := &heuristic.Worker{
		Queries:     q,
		Concurrency: 1,
		Now:         func() time.Time { return time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC) },
	}
	return NewALPRReevaluateHandler(q, w)
}

func doPostReevaluate(t *testing.T, h *ALPRReevaluateHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/alpr/heuristic/reevaluate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.Reevaluate(c); err != nil {
		t.Fatalf("Reevaluate error: %v", err)
	}
	return rec
}

func TestReevaluate_DryRunReturnsCounts(t *testing.T) {
	q := newReevalFakeQuerier()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	q.seedEncountersStrong([]byte{0x01, 0x02}, "donglA", now)
	q.seedEncountersStrong([]byte{0x03, 0x04}, "donglB", now)

	h := newReevalHandler(t, q)
	rec := doPostReevaluate(t, h, `{"days_back":30,"dry_run":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp alprReevaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !resp.Accepted {
		t.Errorf("accepted = false, want true")
	}
	if resp.JobsEnqueued != 2 {
		t.Errorf("jobs_enqueued = %d, want 2", resp.JobsEnqueued)
	}
	if !resp.DryRun {
		t.Errorf("dry_run = false, want true")
	}
	if resp.Summary == nil {
		t.Fatalf("summary missing on dry_run response")
	}
	// Dry-run must NOT have written any watchlist rows or alert
	// events.
	if len(q.watchlist) != 0 {
		t.Errorf("watchlist rows = %d, want 0 in dry_run", len(q.watchlist))
	}
	if len(q.alertEvents) != 0 {
		t.Errorf("alertEvents rows = %d, want 0 in dry_run", len(q.alertEvents))
	}
	// At least one severity bucket should be non-zero given the
	// seeded "strong" encounter shape.
	total := 0
	for _, n := range resp.Summary.BySeverityAfter {
		total += n
	}
	if total != 2 {
		t.Errorf("by_severity_after totals = %d, want 2 (one per plate)", total)
	}
}

func TestReevaluate_LiveTriggersHeuristicAndUpdatesWatchlist(t *testing.T) {
	q := newReevalFakeQuerier()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	plate := []byte{0x01, 0x02}
	q.seedEncountersStrong(plate, "donglA", now)

	h := newReevalHandler(t, q)
	rec := doPostReevaluate(t, h, `{"days_back":30,"dry_run":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Live path must have written an alert_events row + a watchlist
	// row at the computed severity.
	if len(q.alertEvents) != 1 {
		t.Fatalf("alertEvents = %d, want 1", len(q.alertEvents))
	}
	wl, ok := q.watchlist[string(plate)]
	if !ok {
		t.Fatalf("watchlist row not created for plate")
	}
	if !wl.Severity.Valid || wl.Severity.Int16 < 2 {
		t.Errorf("watchlist severity = %+v, want >= 2", wl.Severity)
	}
}

func TestReevaluate_RejectsOutOfRangeDaysBack(t *testing.T) {
	q := newReevalFakeQuerier()
	h := newReevalHandler(t, q)
	rec := doPostReevaluate(t, h, `{"days_back":99999}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	rec = doPostReevaluate(t, h, `{"days_back":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for 0 days_back", rec.Code)
	}
}

func TestReevaluate_DefaultDaysBack(t *testing.T) {
	q := newReevalFakeQuerier()
	h := newReevalHandler(t, q)
	rec := doPostReevaluate(t, h, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp alprReevaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.DaysBack != alprReevaluateDefaultDaysBack {
		t.Errorf("days_back = %d, want %d default", resp.DaysBack, alprReevaluateDefaultDaysBack)
	}
}
