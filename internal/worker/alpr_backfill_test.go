package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// fakeBackfillQuerier is the worker's analog of api.fakeBackfillQuerier:
// an in-memory implementation of ALPRBackfillQuerier that exposes hooks
// the tests need to drive the singleton's state machine deterministically.
type fakeBackfillQuerier struct {
	mu sync.Mutex

	jobs   map[int64]db.AlprBackfillJob
	nextID int64

	// routesAsc is the route set returned by ListBackfillRoutesAsc.
	// Tests typically populate this once and let the worker iterate.
	routesAsc []db.Route

	// processed records calls to MarkExtractorProcessed for assertion.
	processed map[string]int
	// alreadyProcessed lets a test pre-mark segments so the
	// idempotency branch (skip-when-already-processed) can be
	// exercised without manually inserting alpr_segment_progress
	// rows.
	alreadyProcessed map[string]bool
}

func newFakeBackfillQuerier() *fakeBackfillQuerier {
	return &fakeBackfillQuerier{
		jobs:             make(map[int64]db.AlprBackfillJob),
		processed:        make(map[string]int),
		alreadyProcessed: make(map[string]bool),
	}
}

func segKey(d, r string, s int32) string { return d + "|" + r + "|" + string(rune('0'+s)) }

func (f *fakeBackfillQuerier) GetBackfillJob(_ context.Context, id int64) (db.AlprBackfillJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return db.AlprBackfillJob{}, pgx.ErrNoRows
	}
	return j, nil
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

func (f *fakeBackfillQuerier) IncrementBackfillJobProgress(_ context.Context, arg db.IncrementBackfillJobProgressParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[arg.ID]
	if !ok {
		return errors.New("not found")
	}
	job.ProcessedRoutes++
	job.LastProcessedRoute = arg.LastProcessedRoute
	f.jobs[arg.ID] = job
	return nil
}

func (f *fakeBackfillQuerier) ListBackfillRoutesAsc(_ context.Context, arg db.ListBackfillRoutesAscParams) ([]db.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Route
	for _, r := range f.routesAsc {
		if arg.AfterRoute.Valid && r.RouteName <= arg.AfterRoute.String {
			continue
		}
		out = append(out, r)
		if int32(len(out)) >= arg.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeBackfillQuerier) ListBackfillRoutesDesc(_ context.Context, arg db.ListBackfillRoutesDescParams) ([]db.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Route
	for i := len(f.routesAsc) - 1; i >= 0; i-- {
		r := f.routesAsc[i]
		if arg.AfterRoute.Valid && r.RouteName >= arg.AfterRoute.String {
			continue
		}
		out = append(out, r)
		if int32(len(out)) >= arg.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeBackfillQuerier) IsExtractorProcessed(_ context.Context, arg db.IsExtractorProcessedParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alreadyProcessed[segKey(arg.DongleID, arg.Route, arg.Segment)], nil
}

func (f *fakeBackfillQuerier) MarkExtractorProcessed(_ context.Context, arg db.MarkExtractorProcessedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := segKey(arg.DongleID, arg.Route, arg.Segment)
	f.processed[k]++
	f.alreadyProcessed[k] = true
	return nil
}

func (f *fakeBackfillQuerier) CountRouteDetectorProgress(_ context.Context, _ db.CountRouteDetectorProgressParams) (db.CountRouteDetectorProgressRow, error) {
	return db.CountRouteDetectorProgressRow{}, nil
}

// stubFrameSink is an ALPRBackfillFrameSink used in tests that don't
// run ffmpeg. It records pushes for assertion (none of the route-loop
// tests below reach Push because they short-circuit before
// processSegment runs ffmpeg, but the type is needed to construct the
// worker).
type stubFrameSink struct {
	mu     sync.Mutex
	frames []ExtractedFrame
	depth  func() int
}

func (s *stubFrameSink) Push(_ context.Context, f ExtractedFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, f)
	return nil
}

func (s *stubFrameSink) QueueDepth() int {
	if s.depth != nil {
		return s.depth()
	}
	return 0
}

func (s *stubFrameSink) Frames() []ExtractedFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ExtractedFrame, len(s.frames))
	copy(out, s.frames)
	return out
}

func TestBackfill_StateTransition_RunningToDone(t *testing.T) {
	q := newFakeBackfillQuerier()
	// One job, no routes -> immediately transitions to 'done' on the
	// first iteration of the route loop because fetchRouteBatch
	// returns an empty slice.
	q.jobs[1] = db.AlprBackfillJob{
		ID:          1,
		State:       "running",
		StartedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FiltersJson: jsonMarshal(t, alprBackfillFilters{}),
	}
	q.nextID = 1

	en := true
	prev := alprEnabledForTest
	alprEnabledForTest = &en
	t.Cleanup(func() { alprEnabledForTest = prev })

	w := &ALPRBackfill{
		Queries:           q,
		Sink:              &stubFrameSink{},
		FPSBudget:         1000,
		LivePriorityDelay: time.Millisecond,
		IdlePoll:          time.Hour,
		RouteBatch:        10,
	}
	// Storage is nil here; the worker shouldn't dereference it
	// because the route batch is empty. Set a non-nil placeholder
	// satisfying the entry guard.
	w.Storage = nopStorage()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w.runOnce(ctx)

	got, _ := q.GetBackfillJob(ctx, 1)
	if got.State != "done" {
		t.Fatalf("expected state=done, got %q", got.State)
	}
}

func TestBackfill_PauseDuringLoop(t *testing.T) {
	q := newFakeBackfillQuerier()
	q.jobs[1] = db.AlprBackfillJob{
		ID:          1,
		State:       "paused",
		StartedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FiltersJson: jsonMarshal(t, alprBackfillFilters{}),
	}

	en := true
	prev := alprEnabledForTest
	alprEnabledForTest = &en
	t.Cleanup(func() { alprEnabledForTest = prev })

	w := &ALPRBackfill{
		Queries:    q,
		Storage:    nopStorage(),
		Sink:       &stubFrameSink{},
		FPSBudget:  1000,
		IdlePoll:   time.Hour,
		RouteBatch: 10,
	}

	// runOnce() finds no running job -> returns silently. State
	// stays paused.
	w.runOnce(context.Background())
	got, _ := q.GetBackfillJob(context.Background(), 1)
	if got.State != "paused" {
		t.Fatalf("expected state=paused, got %q", got.State)
	}
}

func TestBackfill_AlprDisabled_PausesJob(t *testing.T) {
	q := newFakeBackfillQuerier()
	q.jobs[1] = db.AlprBackfillJob{
		ID:          1,
		State:       "running",
		StartedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FiltersJson: jsonMarshal(t, alprBackfillFilters{}),
	}

	en := false
	prev := alprEnabledForTest
	alprEnabledForTest = &en
	t.Cleanup(func() { alprEnabledForTest = prev })

	w := &ALPRBackfill{
		Queries: q, Storage: nopStorage(), Sink: &stubFrameSink{},
		FPSBudget: 1000, IdlePoll: time.Hour, RouteBatch: 10,
	}
	w.runOnce(context.Background())

	got, _ := q.GetBackfillJob(context.Background(), 1)
	if got.State != "paused" {
		t.Fatalf("expected paused on alpr_enabled=false, got %q", got.State)
	}
}

func TestBackfill_ResumeFromLastProcessed(t *testing.T) {
	q := newFakeBackfillQuerier()
	q.routesAsc = []db.Route{
		{ID: 1, DongleID: "d", RouteName: "r-001"},
		{ID: 2, DongleID: "d", RouteName: "r-002"},
		{ID: 3, DongleID: "d", RouteName: "r-003"},
	}
	q.jobs[1] = db.AlprBackfillJob{
		ID:                 1,
		State:              "running",
		StartedAt:          pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FiltersJson:        jsonMarshal(t, alprBackfillFilters{}),
		LastProcessedRoute: pgtype.Text{String: "r-001", Valid: true},
		ProcessedRoutes:    1,
	}

	en := true
	prev := alprEnabledForTest
	alprEnabledForTest = &en
	t.Cleanup(func() { alprEnabledForTest = prev })

	w := &ALPRBackfill{
		Queries: q, Storage: nopStorage(), Sink: &stubFrameSink{},
		FPSBudget: 1000, LivePriorityDelay: time.Millisecond,
		IdlePoll: time.Hour, RouteBatch: 10,
	}
	w.runOnce(context.Background())

	got, _ := q.GetBackfillJob(context.Background(), 1)
	if got.State != "done" {
		t.Fatalf("expected done after iterating remaining routes, got %q", got.State)
	}
	// Started at processed=1, advanced past r-002 and r-003 -> 3.
	if got.ProcessedRoutes != 3 {
		t.Fatalf("expected processed_routes=3, got %d", got.ProcessedRoutes)
	}
	if !got.LastProcessedRoute.Valid || got.LastProcessedRoute.String != "r-003" {
		t.Fatalf("expected last_processed_route=r-003, got %+v", got.LastProcessedRoute)
	}
}

func TestBackfill_MaxRoutesCap(t *testing.T) {
	q := newFakeBackfillQuerier()
	q.routesAsc = []db.Route{
		{ID: 1, DongleID: "d", RouteName: "r-001"},
		{ID: 2, DongleID: "d", RouteName: "r-002"},
		{ID: 3, DongleID: "d", RouteName: "r-003"},
	}
	q.jobs[1] = db.AlprBackfillJob{
		ID:          1,
		State:       "running",
		StartedAt:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FiltersJson: jsonMarshal(t, alprBackfillFilters{MaxRoutes: 2}),
	}

	en := true
	prev := alprEnabledForTest
	alprEnabledForTest = &en
	t.Cleanup(func() { alprEnabledForTest = prev })

	w := &ALPRBackfill{
		Queries: q, Storage: nopStorage(), Sink: &stubFrameSink{},
		FPSBudget: 1000, LivePriorityDelay: time.Millisecond,
		IdlePoll: time.Hour, RouteBatch: 10,
	}
	w.runOnce(context.Background())

	got, _ := q.GetBackfillJob(context.Background(), 1)
	if got.State != "done" {
		t.Fatalf("expected done at max_routes cap, got %q", got.State)
	}
	if got.ProcessedRoutes != 2 {
		t.Fatalf("expected processed_routes=2 (capped), got %d", got.ProcessedRoutes)
	}
}

func TestBackfill_DecodeFilters(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := jsonMarshal(t, alprBackfillFilters{FromDate: &from, DongleID: "abc", MaxRoutes: 5, NewestFirst: true})
	got, err := decodeFilters(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FromDate == nil || !got.FromDate.Equal(from) {
		t.Fatalf("expected from_date roundtrip, got %v", got.FromDate)
	}
	if got.DongleID != "abc" {
		t.Fatalf("expected dongle_id=abc, got %q", got.DongleID)
	}
	if got.MaxRoutes != 5 {
		t.Fatalf("expected max_routes=5, got %d", got.MaxRoutes)
	}
	if !got.NewestFirst {
		t.Fatalf("expected newest_first=true")
	}
}

func TestBackfill_DecodeFilters_EmptyOK(t *testing.T) {
	got, err := decodeFilters(nil)
	if err != nil {
		t.Fatalf("expected nil err for empty filters, got %v", err)
	}
	if got.FromDate != nil || got.DongleID != "" || got.MaxRoutes != 0 || got.NewestFirst {
		t.Fatalf("expected zero-value filters, got %+v", got)
	}
}

// TestBackfill_TokenBucketWait verifies that the throttle keeps the
// engine queue under budget. We construct a worker with a low fps
// budget (10 fps -> 100ms between frames) and measure how many ticks
// of the bucket fit in a fixed window.
func TestBackfill_TokenBucketWait(t *testing.T) {
	w := &ALPRBackfill{FPSBudget: 10}
	got := w.tokenBucketWait()
	want := 100 * time.Millisecond
	if got != want {
		t.Fatalf("expected %s wait at 10 fps, got %s", want, got)
	}

	w.FPSBudget = 0.5
	got = w.tokenBucketWait()
	want = 2 * time.Second
	if got != want {
		t.Fatalf("expected %s wait at 0.5 fps, got %s", want, got)
	}

	w.FPSBudget = 0
	got = w.tokenBucketWait()
	// Default 0.5 fps -> 2s
	if got != 2*time.Second {
		t.Fatalf("expected default 2s wait at fps=0, got %s", got)
	}
}

// TestBackfill_PushFrame_RespectsLivePriority verifies that the
// worker stalls while the live extractor is producing frames (sink
// queue depth > 0) and resumes once the queue drains. We use a sink
// whose depth returns >0 for the first N polls and 0 afterwards;
// the worker should sleep and not push until the queue drains.
func TestBackfill_PushFrame_RespectsLivePriority(t *testing.T) {
	var depth atomic.Int32
	depth.Store(2)
	sink := &stubFrameSink{depth: func() int { return int(depth.Load()) }}

	w := &ALPRBackfill{
		Sink:              sink,
		FPSBudget:         1000, // negligible token-bucket wait
		LivePriorityDelay: 5 * time.Millisecond,
	}

	// Drain the live queue from a separate goroutine after a short
	// delay; pushFrame should observe the change and proceed.
	go func() {
		time.Sleep(20 * time.Millisecond)
		depth.Store(0)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	frame := ExtractedFrame{DongleID: "d", Route: "r", Segment: 0}
	t0 := time.Now()
	if err := w.pushFrame(ctx, frame); err != nil {
		t.Fatalf("pushFrame: %v", err)
	}
	elapsed := time.Since(t0)
	if elapsed < 15*time.Millisecond {
		t.Fatalf("expected pushFrame to stall ~20ms while live queue non-empty, got %s", elapsed)
	}
	if got := sink.Frames(); len(got) != 1 {
		t.Fatalf("expected exactly one frame pushed, got %d", len(got))
	}
}

// TestBackfill_PushFrame_TokenBucketLimitsRate verifies the
// frames-per-second cap is observed even with an empty live queue.
func TestBackfill_PushFrame_TokenBucketLimitsRate(t *testing.T) {
	sink := &stubFrameSink{}
	w := &ALPRBackfill{
		Sink:              sink,
		FPSBudget:         50, // 20ms between frames
		LivePriorityDelay: time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	t0 := time.Now()
	for i := 0; i < 5; i++ {
		if err := w.pushFrame(ctx, ExtractedFrame{Route: "r", Segment: i}); err != nil {
			t.Fatalf("pushFrame: %v", err)
		}
	}
	elapsed := time.Since(t0)
	// 5 frames * 20ms = 100ms minimum
	if elapsed < 90*time.Millisecond {
		t.Fatalf("expected throttle ~100ms for 5 frames at 50fps, got %s", elapsed)
	}
}

// TestBackfill_Wake_NonblockingCollapse verifies that double-Wake
// calls do not deadlock and the channel is buffered to 1.
func TestBackfill_Wake_NonblockingCollapse(t *testing.T) {
	w := &ALPRBackfill{}
	for i := 0; i < 10; i++ {
		w.Wake()
	}
	// Channel should have exactly one pending value.
	select {
	case <-w.wakeCh:
	default:
		t.Fatalf("expected one pending wake")
	}
	select {
	case <-w.wakeCh:
		t.Fatalf("expected only one pending wake (collapsed)")
	default:
	}
}

func TestBackfill_RestartResumesRunning(t *testing.T) {
	// Start with a 'running' row and last_processed_route set.
	// runOnce should pick it up via GetRunningBackfillJob and resume.
	q := newFakeBackfillQuerier()
	q.routesAsc = []db.Route{
		{ID: 1, DongleID: "d", RouteName: "r-001"},
		{ID: 2, DongleID: "d", RouteName: "r-002"},
	}
	q.jobs[42] = db.AlprBackfillJob{
		ID:                 42,
		State:              "running",
		StartedAt:          pgtype.Timestamptz{Time: time.Now(), Valid: true},
		FiltersJson:        jsonMarshal(t, alprBackfillFilters{}),
		LastProcessedRoute: pgtype.Text{String: "r-001", Valid: true},
		ProcessedRoutes:    1,
	}

	en := true
	prev := alprEnabledForTest
	alprEnabledForTest = &en
	t.Cleanup(func() { alprEnabledForTest = prev })

	w := &ALPRBackfill{
		Queries: q, Storage: nopStorage(), Sink: &stubFrameSink{},
		FPSBudget: 1000, LivePriorityDelay: time.Millisecond,
		IdlePoll: time.Hour, RouteBatch: 10,
	}
	w.runOnce(context.Background())

	got, _ := q.GetBackfillJob(context.Background(), 42)
	if got.State != "done" {
		t.Fatalf("expected done after resume, got %q", got.State)
	}
	if !got.LastProcessedRoute.Valid || got.LastProcessedRoute.String != "r-002" {
		t.Fatalf("expected last_processed_route=r-002, got %+v", got.LastProcessedRoute)
	}
}

// helper: marshal a value to JSON, failing the test on error.
func jsonMarshal(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// nopStorage returns a Storage rooted at a path that does not exist.
// ListSegments on it returns an error, which processRoute handles
// gracefully by logging and continuing -- exactly the behaviour the
// route-loop tests want (the worker should still mark routes
// processed and bump the counter even when the on-disk segments are
// missing).
func nopStorage() *storage.Storage {
	return storage.New("/nonexistent-backfill-test-path-" + time.Now().Format("20060102150405"))
}
