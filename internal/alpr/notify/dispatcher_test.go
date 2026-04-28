package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/db"
)

// fakeQuerier is the in-memory notifyQuerier used by every dispatcher
// test. The mutex guards both maps so the parallel-dispatch test can
// safely read after wg.Wait without a race detector flag.
type fakeQuerier struct {
	mu        sync.Mutex
	watchlist map[string]db.GetWatchlistByHashRow
	sent      map[string]time.Time
	getErr    error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		watchlist: map[string]db.GetWatchlistByHashRow{},
		sent:      map[string]time.Time{},
	}
}

func (f *fakeQuerier) GetWatchlistByHash(_ context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return db.GetWatchlistByHashRow{}, f.getErr
	}
	row, ok := f.watchlist[string(plateHash)]
	if !ok {
		return db.GetWatchlistByHashRow{}, pgx.ErrNoRows
	}
	return row, nil
}

func (f *fakeQuerier) GetLastNotificationSent(_ context.Context, plateHash []byte) (db.AlprNotificationsSent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.sent[string(plateHash)]
	if !ok {
		return db.AlprNotificationsSent{}, pgx.ErrNoRows
	}
	return db.AlprNotificationsSent{
		PlateHash:  plateHash,
		LastSentAt: pgtype.Timestamptz{Time: t, Valid: true},
	}, nil
}

func (f *fakeQuerier) UpsertNotificationSent(_ context.Context, arg db.UpsertNotificationSentParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[string(arg.PlateHash)] = arg.LastSentAt.Time
	return nil
}

// recordingSender is a Sender impl that records every call and returns
// a configurable error. Safe for concurrent use.
type recordingSender struct {
	name  string
	mu    sync.Mutex
	calls []AlertPayload
	err   error
	hold  chan struct{} // when non-nil, Send blocks until closed
}

func (s *recordingSender) Name() string { return s.name }

func (s *recordingSender) Send(ctx context.Context, alert AlertPayload) error {
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, alert)
	return s.err
}

func (s *recordingSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func basePayload() (AlertPayload, []byte) {
	plateHash := make([]byte, 32)
	for i := range plateHash {
		plateHash[i] = byte(i)
	}
	return AlertPayload{
		Severity:     5,
		Plate:        "ABC-123",
		PlateHashB64: EncodePlateHashB64(plateHash),
		Evidence: []heuristic.Component{
			{Name: heuristic.ComponentCrossRouteCount, Points: 2.0},
		},
		Route:    "abc1234567890abc|2026-04-27--10-00-00",
		DongleID: "abc1234567890abc",
	}, plateHash
}

// passThroughEnricher returns the input unchanged. The dispatcher's
// production enricher fills in plate text + vehicle from the DB; the
// tests already populate those fields so a no-op enricher is correct.
var passThroughEnricher = PayloadEnricherFunc(func(_ context.Context, p AlertPayload) (AlertPayload, error) {
	return p, nil
})

func TestDispatcher_NoSenders_NoOp(t *testing.T) {
	q := newFakeQuerier()
	d := New(q, nil, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	// No upsert because no senders ran.
	if len(q.sent) != 0 {
		t.Fatalf("expected no dedup ledger writes, got %d", len(q.sent))
	}
}

func TestDispatcher_SeverityBelowThreshold_Dropped(t *testing.T) {
	email := &recordingSender{name: "email"}
	q := newFakeQuerier()
	d := New(q, []Sender{email}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	payload.Severity = 3
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if email.callCount() != 0 {
		t.Fatalf("expected sender not invoked below threshold, got %d calls", email.callCount())
	}
	if len(q.sent) != 0 {
		t.Fatalf("expected no dedup ledger write below threshold, got %d", len(q.sent))
	}
}

func TestDispatcher_SeverityAtThreshold_Sent(t *testing.T) {
	email := &recordingSender{name: "email"}
	q := newFakeQuerier()
	d := New(q, []Sender{email}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	payload.Severity = 4
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if email.callCount() != 1 {
		t.Fatalf("expected 1 sender call, got %d", email.callCount())
	}
	if len(q.sent) != 1 {
		t.Fatalf("expected dedup ledger to advance, got %d entries", len(q.sent))
	}
}

func TestDispatcher_DedupWindow_SuppressesSecondAlert(t *testing.T) {
	email := &recordingSender{name: "email"}
	q := newFakeQuerier()
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	d := New(q, []Sender{email}, passThroughEnricher, Config{
		MinSeverity: 4,
		DedupHours:  12,
		Now:         func() time.Time { return now },
	})
	payload, hash := basePayload()

	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("first Dispatch error: %v", err)
	}
	if email.callCount() != 1 {
		t.Fatalf("first Dispatch: expected 1 call, got %d", email.callCount())
	}

	// Same plate, 1h later -- inside the 12h window. Should be suppressed.
	now = now.Add(1 * time.Hour)
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("second Dispatch error: %v", err)
	}
	if email.callCount() != 1 {
		t.Fatalf("dedup window: expected still 1 call, got %d", email.callCount())
	}

	// 13h after the original send -- outside the window. Should fire.
	now = now.Add(12 * time.Hour)
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("third Dispatch error: %v", err)
	}
	if email.callCount() != 2 {
		t.Fatalf("post-window: expected 2 calls, got %d", email.callCount())
	}
}

func TestDispatcher_ParallelDispatch_BothSendersCalled(t *testing.T) {
	email := &recordingSender{name: "email"}
	webhook := &recordingSender{name: "webhook"}
	q := newFakeQuerier()
	d := New(q, []Sender{email, webhook}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if email.callCount() != 1 || webhook.callCount() != 1 {
		t.Fatalf("expected both senders called once, got email=%d webhook=%d",
			email.callCount(), webhook.callCount())
	}
}

func TestDispatcher_PartialFailure_OtherSenderStillRuns(t *testing.T) {
	email := &recordingSender{name: "email", err: errors.New("smtp 451")}
	webhook := &recordingSender{name: "webhook"}
	q := newFakeQuerier()
	d := New(q, []Sender{email, webhook}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if email.callCount() != 1 {
		t.Fatalf("email: expected 1 call, got %d", email.callCount())
	}
	if webhook.callCount() != 1 {
		t.Fatalf("webhook: expected 1 call, got %d", webhook.callCount())
	}
	// Dedup ledger should advance because at least one sender succeeded.
	if len(q.sent) != 1 {
		t.Fatalf("expected dedup ledger advance on partial success, got %d", len(q.sent))
	}
}

func TestDispatcher_AllSendersFail_DedupLedgerNotAdvanced(t *testing.T) {
	email := &recordingSender{name: "email", err: errors.New("smtp 451")}
	webhook := &recordingSender{name: "webhook", err: errors.New("503 service unavailable")}
	q := newFakeQuerier()
	d := New(q, []Sender{email, webhook}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if len(q.sent) != 0 {
		t.Fatalf("dedup ledger advanced despite all senders failing: %d entries", len(q.sent))
	}
}

func TestDispatcher_WhitelistedPlate_Suppressed(t *testing.T) {
	email := &recordingSender{name: "email"}
	q := newFakeQuerier()
	payload, hash := basePayload()
	q.watchlist[string(hash)] = db.GetWatchlistByHashRow{
		Kind: "whitelist",
	}
	d := New(q, []Sender{email}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if email.callCount() != 0 {
		t.Fatalf("expected whitelist to suppress dispatch, got %d calls", email.callCount())
	}
}

func TestDispatcher_AlertedPlate_NotSuppressed(t *testing.T) {
	email := &recordingSender{name: "email"}
	q := newFakeQuerier()
	payload, hash := basePayload()
	q.watchlist[string(hash)] = db.GetWatchlistByHashRow{
		Kind: "alerted",
	}
	d := New(q, []Sender{email}, passThroughEnricher, Config{MinSeverity: 4, DedupHours: 12})
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if email.callCount() != 1 {
		t.Fatalf("expected alerted-kind to be dispatched, got %d calls", email.callCount())
	}
}

func TestDispatcher_EmptyPlateAfterEnrich_Skipped(t *testing.T) {
	email := &recordingSender{name: "email"}
	q := newFakeQuerier()
	enricher := PayloadEnricherFunc(func(_ context.Context, p AlertPayload) (AlertPayload, error) {
		p.Plate = ""
		return p, nil
	})
	d := New(q, []Sender{email}, enricher, Config{MinSeverity: 4, DedupHours: 12})
	payload, hash := basePayload()
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if email.callCount() != 0 {
		t.Fatalf("empty plate after enrich should skip dispatch, got %d calls", email.callCount())
	}
}

func TestDispatcher_Test_BypassesDedupAndCallsAllSenders(t *testing.T) {
	email := &recordingSender{name: "email"}
	webhook := &recordingSender{name: "webhook"}
	q := newFakeQuerier()
	hash := make([]byte, 32)
	q.sent[string(hash)] = time.Now() // pretend we just sent
	d := New(q, []Sender{email, webhook}, passThroughEnricher, Config{
		MinSeverity:  4,
		DedupHours:   12,
		DashboardURL: "https://comma.example.com",
	})
	payload := AlertPayload{
		Severity:     5,
		Plate:        "TEST-123",
		PlateHashB64: EncodePlateHashB64(hash),
	}
	results := d.Test(context.Background(), payload)
	if len(results) != 2 {
		t.Fatalf("expected 2 result rows, got %d", len(results))
	}
	if email.callCount() != 1 || webhook.callCount() != 1 {
		t.Fatalf("expected both senders called once, got email=%d webhook=%d",
			email.callCount(), webhook.callCount())
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("sender %q reported not OK: %s", r.Sender, r.Error)
		}
	}
	// Dashboard URL should have been built and passed through to senders.
	if got := email.calls[0].DashboardURL; !strings.HasPrefix(got, "https://comma.example.com/alpr/plates/") {
		t.Errorf("email payload missing dashboard URL: %q", got)
	}
}

func TestDispatcher_Test_NoSenders_ReturnsNil(t *testing.T) {
	q := newFakeQuerier()
	d := New(q, nil, passThroughEnricher, Config{MinSeverity: 4})
	results := d.Test(context.Background(), AlertPayload{Severity: 5, Plate: "TEST-123"})
	if results != nil {
		t.Fatalf("expected nil results when no senders configured, got %v", results)
	}
}

// TestDispatcher_FanOut_PerSenderTimeout proves that a stuck sender does
// not steal the budget of its sibling. We give the dispatcher a 50ms
// SendTimeout and a sender that holds Send forever; the second sender
// should still complete promptly.
func TestDispatcher_FanOut_PerSenderTimeout(t *testing.T) {
	stuck := &recordingSender{name: "stuck", hold: make(chan struct{})}
	defer close(stuck.hold)
	fast := &recordingSender{name: "fast"}
	q := newFakeQuerier()
	d := New(q, []Sender{stuck, fast}, passThroughEnricher, Config{
		MinSeverity: 4,
		SendTimeout: 50 * time.Millisecond,
	})
	payload, hash := basePayload()
	start := time.Now()
	if err := d.Dispatch(context.Background(), payload, hash); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Dispatch blocked too long: %s", elapsed)
	}
	if fast.callCount() != 1 {
		t.Fatalf("fast sender should have completed, got %d calls", fast.callCount())
	}
}

func TestDispatcher_SenderNames_PreservesOrder(t *testing.T) {
	a := &recordingSender{name: "email"}
	b := &recordingSender{name: "webhook"}
	d := New(newFakeQuerier(), []Sender{a, b}, passThroughEnricher, Config{})
	got := d.SenderNames()
	want := []string{"email", "webhook"}
	if len(got) != len(want) {
		t.Fatalf("SenderNames() length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SenderNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
