package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// fakeBackfillQuerier is an in-memory implementation of
// alprBackfillQuerier. Each test gets its own instance; the mutex
// guards concurrent access from a Wake-driven goroutine the worker
// might spin up alongside the test goroutine.
type fakeBackfillQuerier struct {
	mu              sync.Mutex
	jobs            map[int64]db.AlprBackfillJob
	nextID          int64
	totalRoutes     int64
	insertErr       error
	uniqueViolation bool
}

func newFakeBackfillQuerier() *fakeBackfillQuerier {
	return &fakeBackfillQuerier{
		jobs:        make(map[int64]db.AlprBackfillJob),
		totalRoutes: 100,
	}
}

func (f *fakeBackfillQuerier) InsertBackfillJob(_ context.Context, arg db.InsertBackfillJobParams) (db.AlprBackfillJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return db.AlprBackfillJob{}, f.insertErr
	}
	if f.uniqueViolation && arg.State == "running" {
		// Synthesize a 23505 unique-violation the same way Postgres
		// would so the handler's errors.As branch is exercised.
		return db.AlprBackfillJob{}, &pgconn.PgError{Code: "23505"}
	}
	if arg.State == "running" {
		// Enforce singleton in the fake so tests that start two jobs
		// observe a unique violation on the second.
		for _, j := range f.jobs {
			if j.State == "running" {
				return db.AlprBackfillJob{}, &pgconn.PgError{Code: "23505"}
			}
		}
	}
	f.nextID++
	job := db.AlprBackfillJob{
		ID:          f.nextID,
		StartedAt:   pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		State:       arg.State,
		FiltersJson: arg.FiltersJson,
		TotalRoutes: arg.TotalRoutes,
		StartedBy:   arg.StartedBy,
	}
	f.jobs[job.ID] = job
	return job, nil
}

func (f *fakeBackfillQuerier) GetLatestBackfillJob(_ context.Context) (db.AlprBackfillJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.jobs) == 0 {
		return db.AlprBackfillJob{}, pgx.ErrNoRows
	}
	var latest db.AlprBackfillJob
	for _, j := range f.jobs {
		if j.ID > latest.ID {
			latest = j
		}
	}
	return latest, nil
}

func (f *fakeBackfillQuerier) GetRunningBackfillJob(_ context.Context) (db.AlprBackfillJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.State == "running" {
			return j, nil
		}
	}
	return db.AlprBackfillJob{}, pgx.ErrNoRows
}

func (f *fakeBackfillQuerier) UpdateBackfillJobState(_ context.Context, arg db.UpdateBackfillJobStateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[arg.ID]
	if !ok {
		return errors.New("not found")
	}
	if arg.State == "running" {
		for _, j := range f.jobs {
			if j.ID != arg.ID && j.State == "running" {
				return &pgconn.PgError{Code: "23505"}
			}
		}
	}
	job.State = arg.State
	if arg.FinishedAt.Valid {
		job.FinishedAt = arg.FinishedAt
	}
	if arg.ErrorText.Valid {
		job.Error = arg.ErrorText
	}
	f.jobs[arg.ID] = job
	return nil
}

func (f *fakeBackfillQuerier) CountBackfillRoutes(_ context.Context, _ db.CountBackfillRoutesParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totalRoutes, nil
}

// fakeTrigger is a minimal ALPRBackfillTrigger that just records call
// count so tests can assert Wake() was invoked.
type fakeTrigger struct {
	mu    sync.Mutex
	wakes int
}

func (f *fakeTrigger) Wake() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakes++
}

func (f *fakeTrigger) WakeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakes
}

// newBackfillTestEnv wires up an Echo instance with the handler routes
// mounted, returning everything tests need to issue requests.
func newBackfillTestEnv(t *testing.T) (*echo.Echo, *fakeBackfillQuerier, *fakeTrigger, *ALPRBackfillHandler) {
	t.Helper()
	e := echo.New()
	q := newFakeBackfillQuerier()
	tr := &fakeTrigger{}
	h := NewALPRBackfillHandler(q, tr)
	g := e.Group("/v1")
	h.RegisterRoutes(g)
	return e, q, tr, h
}

func doRequest(e *echo.Echo, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestBackfill_Start_OK(t *testing.T) {
	e, q, tr, _ := newBackfillTestEnv(t)
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ALPRBackfillStartResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.JobID == 0 {
		t.Fatalf("expected non-zero job id")
	}
	if tr.WakeCount() != 1 {
		t.Fatalf("expected one wake, got %d", tr.WakeCount())
	}
	got, err := q.GetRunningBackfillJob(context.Background())
	if err != nil {
		t.Fatalf("expected running job, got err=%v", err)
	}
	if got.State != "running" {
		t.Fatalf("expected state=running, got %q", got.State)
	}
	if !got.TotalRoutes.Valid || got.TotalRoutes.Int32 != 100 {
		t.Fatalf("expected total_routes=100, got %+v", got.TotalRoutes)
	}
}

func TestBackfill_Start_409WhenAlreadyRunning(t *testing.T) {
	e, _, _, _ := newBackfillTestEnv(t)
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	if rec.Code != http.StatusOK {
		t.Fatalf("first start: expected 200, got %d", rec.Code)
	}
	rec = doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second start: expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBackfill_Start_RaceTo409ViaUniqueViolation(t *testing.T) {
	e, q, _, _ := newBackfillTestEnv(t)
	// Force the handler past its fast-path GetRunningBackfillJob check
	// (no row -> ErrNoRows), then trip the synthesized unique
	// violation on InsertBackfillJob to exercise the error-path 409.
	q.uniqueViolation = true
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 from unique violation, got %d", rec.Code)
	}
}

func TestBackfill_Start_BadDates(t *testing.T) {
	e, _, _, _ := newBackfillTestEnv(t)
	to := time.Now().Add(-time.Hour)
	from := time.Now()
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{
		FromDate: &from,
		ToDate:   &to,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for to_date<from_date, got %d", rec.Code)
	}
}

func TestBackfill_Start_NegativeMaxRoutes(t *testing.T) {
	e, _, _, _ := newBackfillTestEnv(t)
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{MaxRoutes: -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative max_routes, got %d", rec.Code)
	}
}

func TestBackfill_Start_MaxRoutesCap(t *testing.T) {
	e, q, _, _ := newBackfillTestEnv(t)
	q.totalRoutes = 1000
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{MaxRoutes: 25})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got, _ := q.GetRunningBackfillJob(context.Background())
	if got.TotalRoutes.Int32 != 25 {
		t.Fatalf("expected total_routes capped at 25, got %d", got.TotalRoutes.Int32)
	}
}

func TestBackfill_Status_NoJob(t *testing.T) {
	e, _, _, _ := newBackfillTestEnv(t)
	rec := doRequest(e, http.MethodGet, "/v1/alpr/backfill/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"state":"none"`) {
		t.Fatalf("expected state=none for empty status, got %s", rec.Body.String())
	}
}

func TestBackfill_Status_Running(t *testing.T) {
	e, _, _, _ := newBackfillTestEnv(t)
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	if rec.Code != http.StatusOK {
		t.Fatalf("start: expected 200, got %d", rec.Code)
	}
	rec = doRequest(e, http.MethodGet, "/v1/alpr/backfill/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp ALPRBackfillStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.State != "running" {
		t.Fatalf("expected state=running, got %q", resp.State)
	}
	if resp.TotalRoutes == nil || *resp.TotalRoutes != 100 {
		t.Fatalf("expected total_routes=100, got %+v", resp.TotalRoutes)
	}
	// processed_routes=0 -> ETA is null (throughput not yet measurable).
	if resp.ETASeconds != nil {
		t.Fatalf("expected nil ETA at processed=0, got %v", *resp.ETASeconds)
	}
}

func TestBackfill_PauseResumeCancel(t *testing.T) {
	e, q, _, _ := newBackfillTestEnv(t)
	startRec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	if startRec.Code != http.StatusOK {
		t.Fatalf("start: %d", startRec.Code)
	}

	// Pause
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/pause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: expected 200, got %d", rec.Code)
	}
	job, _ := q.GetLatestBackfillJob(context.Background())
	if job.State != "paused" {
		t.Fatalf("expected state=paused, got %q", job.State)
	}

	// Resume
	rec = doRequest(e, http.MethodPost, "/v1/alpr/backfill/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	job, _ = q.GetLatestBackfillJob(context.Background())
	if job.State != "running" {
		t.Fatalf("expected state=running, got %q", job.State)
	}

	// Cancel
	rec = doRequest(e, http.MethodPost, "/v1/alpr/backfill/cancel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d", rec.Code)
	}
	job, _ = q.GetLatestBackfillJob(context.Background())
	if job.State != "failed" {
		t.Fatalf("expected state=failed after cancel, got %q", job.State)
	}
	if !job.Error.Valid || job.Error.String != "cancelled by user" {
		t.Fatalf("expected error=cancelled by user, got %+v", job.Error)
	}
}

func TestBackfill_Resume_409WhenNotPaused(t *testing.T) {
	e, _, _, _ := newBackfillTestEnv(t)
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/resume", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestBackfill_Cancel_IdempotentAfterTerminal(t *testing.T) {
	e, q, _, _ := newBackfillTestEnv(t)
	doRequest(e, http.MethodPost, "/v1/alpr/backfill/start", ALPRBackfillFilters{})
	doRequest(e, http.MethodPost, "/v1/alpr/backfill/cancel", nil)

	// Second cancel should be a no-op returning state=failed unchanged.
	rec := doRequest(e, http.MethodPost, "/v1/alpr/backfill/cancel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	job, _ := q.GetLatestBackfillJob(context.Background())
	if job.State != "failed" {
		t.Fatalf("expected state=failed unchanged, got %q", job.State)
	}
}

func TestComputeETASeconds(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-2 * time.Minute)

	// 10 routes processed in 2 minutes -> 5 routes/minute. 90 left
	// -> 18 minutes -> 1080 seconds.
	job := db.AlprBackfillJob{
		StartedAt:       pgtype.Timestamptz{Time: startedAt, Valid: true},
		State:           "running",
		ProcessedRoutes: 10,
		TotalRoutes:     pgtype.Int4{Int32: 100, Valid: true},
	}
	got := computeETASeconds(job, now)
	if got == nil {
		t.Fatalf("expected non-nil ETA")
	}
	if *got < 1075 || *got > 1085 {
		t.Fatalf("expected ~1080 seconds, got %d", *got)
	}

	// Zero processed -> nil
	job.ProcessedRoutes = 0
	if computeETASeconds(job, now) != nil {
		t.Fatalf("expected nil ETA at processed=0")
	}

	// Done -> 0
	job.ProcessedRoutes = 100
	got = computeETASeconds(job, now)
	if got == nil || *got != 0 {
		t.Fatalf("expected 0 ETA when processed==total, got %v", got)
	}

	// Capped on absurdly slow throughput
	job = db.AlprBackfillJob{
		StartedAt:       pgtype.Timestamptz{Time: now.Add(-1 * time.Minute), Valid: true},
		State:           "running",
		ProcessedRoutes: 1,
		TotalRoutes:     pgtype.Int4{Int32: 1_000_000_000, Valid: true},
	}
	got = computeETASeconds(job, now)
	if got == nil || *got != alprBackfillETACap {
		t.Fatalf("expected ETA capped at %d, got %v", alprBackfillETACap, got)
	}
}

func TestStatusFromJob_TerminalStateOmitsETA(t *testing.T) {
	now := time.Now().UTC()
	job := db.AlprBackfillJob{
		ID:              7,
		StartedAt:       pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		FinishedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		State:           "done",
		ProcessedRoutes: 50,
		TotalRoutes:     pgtype.Int4{Int32: 50, Valid: true},
	}
	resp := statusFromJob(job, now)
	if resp.ETASeconds != nil {
		t.Fatalf("expected ETA=nil for terminal state, got %v", *resp.ETASeconds)
	}
	if resp.FinishedAt == nil {
		t.Fatalf("expected non-nil finished_at")
	}
}
