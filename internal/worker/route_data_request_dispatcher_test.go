package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"comma-personal-backend/internal/db"
)

// fakePendingQueries implements PendingDispatcherQueries with an in-memory
// row store so we don't need a Postgres pool to exercise the worker loop.
type fakePendingQueries struct {
	mu sync.Mutex

	rows []db.ListPendingRouteDataRequestsRow
	// updates records every UpdateRouteDataRequestStatus call so tests can
	// assert how the worker transitioned each row.
	updates []db.UpdateRouteDataRequestStatusParams

	listErr   error
	updateErr error
}

func (f *fakePendingQueries) ListPendingRouteDataRequests(_ context.Context, limit int32) ([]db.ListPendingRouteDataRequestsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]db.ListPendingRouteDataRequestsRow, 0, len(f.rows))
	for i, r := range f.rows {
		if int32(i) >= limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakePendingQueries) UpdateRouteDataRequestStatus(_ context.Context, arg db.UpdateRouteDataRequestStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates = append(f.updates, arg)
	// Mutate the in-memory row to reflect the new status, so the next
	// list call wouldn't keep returning rows the worker already handled
	// in real Postgres semantics. Safe even when the row is no longer in
	// the slice.
	for i := range f.rows {
		if f.rows[i].RequestID == arg.ID {
			f.rows[i].Status = arg.Status
		}
	}
	return nil
}

// fakePendingRowDispatcher records every call and returns scripted (online,
// err) tuples in order. After the script is exhausted the last entry is
// reused so the test does not have to specify every retry.
type fakePendingRowDispatcher struct {
	mu     sync.Mutex
	script []dispatchScriptEntry
	calls  []db.ListPendingRouteDataRequestsRow

	totalCalls atomic.Int32
}

type dispatchScriptEntry struct {
	online bool
	err    error
}

func (d *fakePendingRowDispatcher) DispatchPending(_ context.Context, row db.ListPendingRouteDataRequestsRow) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, row)
	d.totalCalls.Add(1)
	if len(d.script) == 0 {
		return false, nil
	}
	idx := len(d.calls) - 1
	if idx >= len(d.script) {
		idx = len(d.script) - 1
	}
	e := d.script[idx]
	return e.online, e.err
}

func newDispatcherForTest(q *fakePendingQueries, d *fakePendingRowDispatcher) *RouteDataRequestDispatcher {
	w := NewRouteDataRequestDispatcher(q, d)
	w.PollInterval = 10 * time.Millisecond
	w.MaxAttempts = 3
	return w
}

func pendingRow(id int32, dongle string) db.ListPendingRouteDataRequestsRow {
	return db.ListPendingRouteDataRequestsRow{
		RequestID: id,
		RouteID:   id + 100,
		DongleID:  dongle,
		RouteName: "2024-03-15--12-30-00",
		Kind:      "all",
		Status:    "pending",
	}
}

func TestDispatcher_RetryThenSucceed(t *testing.T) {
	q := &fakePendingQueries{
		rows: []db.ListPendingRouteDataRequestsRow{pendingRow(1, "abc123")},
	}
	d := &fakePendingRowDispatcher{
		// First sweep: device offline. Second sweep: online + success.
		script: []dispatchScriptEntry{
			{online: false},
			{online: true, err: nil},
		},
	}
	w := newDispatcherForTest(q, d)

	ctx := context.Background()

	// Sweep 1: device offline -> row stays pending, no update.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if len(q.updates) != 0 {
		t.Errorf("first sweep updates = %d, want 0", len(q.updates))
	}

	// Sweep 2: online + success -> update to dispatched.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if len(q.updates) != 1 {
		t.Fatalf("second sweep updates = %d, want 1", len(q.updates))
	}
	if q.updates[0].Status != "dispatched" {
		t.Errorf("update status = %q, want dispatched", q.updates[0].Status)
	}
	if !q.updates[0].DispatchedAt.Valid {
		t.Errorf("dispatched_at not stamped on update")
	}
}

func TestDispatcher_BoundedRetryThenFailOffline(t *testing.T) {
	q := &fakePendingQueries{
		rows: []db.ListPendingRouteDataRequestsRow{pendingRow(7, "abc123")},
	}
	d := &fakePendingRowDispatcher{
		// Always offline.
		script: []dispatchScriptEntry{{online: false}},
	}
	w := newDispatcherForTest(q, d)
	w.MaxAttempts = 3

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}

	// After MaxAttempts visits the row should be marked failed.
	var failedUpdates []db.UpdateRouteDataRequestStatusParams
	for _, u := range q.updates {
		if u.Status == "failed" {
			failedUpdates = append(failedUpdates, u)
		}
	}
	if len(failedUpdates) != 1 {
		t.Fatalf("failed updates = %d, want 1; all updates: %+v", len(failedUpdates), q.updates)
	}
	if !failedUpdates[0].Error.Valid {
		t.Errorf("failed update missing error text: %+v", failedUpdates[0])
	}
}

func TestDispatcher_BoundedRetryThenFailRPCError(t *testing.T) {
	q := &fakePendingQueries{
		rows: []db.ListPendingRouteDataRequestsRow{pendingRow(11, "abc123")},
	}
	d := &fakePendingRowDispatcher{
		// Always online + RPC error.
		script: []dispatchScriptEntry{{online: true, err: errors.New("boom")}},
	}
	w := newDispatcherForTest(q, d)
	w.MaxAttempts = 2

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
	}

	var failed *db.UpdateRouteDataRequestStatusParams
	for i := range q.updates {
		if q.updates[i].Status == "failed" {
			failed = &q.updates[i]
			break
		}
	}
	if failed == nil {
		t.Fatalf("no failed update recorded; all updates: %+v", q.updates)
	}
	if !failed.Error.Valid || failed.Error.String == "" {
		t.Errorf("failed update has empty error: %+v", failed)
	}
	wantContains := "dispatch failed"
	if got := failed.Error.String; len(got) < len(wantContains) || got[:len(wantContains)] != wantContains {
		t.Errorf("failed error = %q, want prefix %q", got, wantContains)
	}
}

func TestDispatcher_ListErrorPropagates(t *testing.T) {
	q := &fakePendingQueries{listErr: errors.New("db down")}
	d := &fakePendingRowDispatcher{}
	w := newDispatcherForTest(q, d)

	if err := w.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce returned nil, want list error")
	}
	if got := d.totalCalls.Load(); got != 0 {
		t.Errorf("dispatcher called %d times, want 0 when list fails", got)
	}
}

func TestHubBackedDispatcher_NilHubReportsOffline(t *testing.T) {
	d := &HubBackedDispatcher{}
	online, err := d.DispatchPending(context.Background(), pendingRow(1, "abc"))
	if online {
		t.Errorf("online = true, want false on nil hub")
	}
	if err != nil {
		t.Errorf("err = %v, want nil on nil hub (offline path)", err)
	}
}
