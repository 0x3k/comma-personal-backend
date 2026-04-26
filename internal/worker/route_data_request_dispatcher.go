// Package worker -- route_data_request_dispatcher.
//
// When a POST /v1/route/.../request_full_data arrives while the target
// device is offline, the handler persists the row with status='pending'
// and returns 202 Accepted instead of immediately failing. This worker
// wakes on a timer, lists every pending row, and retries the dispatch for
// any whose route's dongle is now online on the WS hub.
//
// Failure handling: each retry attempt increments an in-memory counter
// keyed by request id. After MaxAttempts retries the row is marked
// 'failed' with a clear error, so the polling UI can stop spinning.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/ws"
)

// Dispatcher defaults. Tuned conservatively: a 30s poll keeps the worker
// quiet on idle backends while still being responsive when a device
// reconnects, and 10 attempts is enough to cover ~5 minutes of intermittent
// connectivity at the default poll without flapping rows to 'failed'.
const (
	defaultDispatcherPollInterval = 30 * time.Second
	defaultDispatcherBatchLimit   = 100
	defaultDispatcherMaxAttempts  = 10
)

// PendingDispatcherQueries is the subset of db.Queries the worker needs.
// Narrow interface so tests can substitute a fake without standing up a
// real Postgres pool.
type PendingDispatcherQueries interface {
	ListPendingRouteDataRequests(ctx context.Context, limit int32) ([]db.ListPendingRouteDataRequestsRow, error)
	UpdateRouteDataRequestStatus(ctx context.Context, arg db.UpdateRouteDataRequestStatusParams) error
}

// HubBackedDispatcher is the production PendingRowDispatcher: it consults
// the shared WebSocket hub for the device's live client and forwards a
// freshly rebuilt batch through the shared RPC caller.
//
// We do NOT persist the original UploadFileToUrlParams batch on the row --
// regenerating from the segments table on retry means a file that finally
// uploaded through the regular auto-upload path between the original POST
// and the retry won't be redundantly enqueued. The trade-off is that the
// retry needs to know the per-segment file list; that knowledge lives in
// the api package, so we accept a builder callback.
type HubBackedDispatcher struct {
	// Hub and RPC are the shared websocket primitives. A nil hub or rpc
	// collapses DispatchPending to "device offline".
	Hub *ws.Hub
	RPC *ws.RPCCaller

	// BuildItems regenerates the per-row upload batch from live data
	// (segments table + base URL). It is the api package's responsibility
	// because it owns the upload URL shape and the kind/file mapping.
	BuildItems func(ctx context.Context, row db.ListPendingRouteDataRequestsRow) ([]ws.UploadFileToUrlParams, error)
}

// DispatchPending implements PendingRowDispatcher.
func (d *HubBackedDispatcher) DispatchPending(ctx context.Context, row db.ListPendingRouteDataRequestsRow) (bool, error) {
	if d.Hub == nil || d.RPC == nil {
		return false, nil
	}
	client, ok := d.Hub.GetClient(row.DongleID)
	if !ok || client == nil {
		return false, nil
	}
	if d.BuildItems == nil {
		return true, fmt.Errorf("hub dispatcher missing item builder")
	}
	items, err := d.BuildItems(ctx, row)
	if err != nil {
		return true, fmt.Errorf("rebuild batch: %w", err)
	}
	if len(items) == 0 {
		// Nothing left to upload (everything finished while we were
		// offline). Treat as success so the row promotes to dispatched.
		return true, nil
	}
	if _, err := ws.CallUploadFilesToUrls(d.RPC, client, items); err != nil {
		return true, err
	}
	return true, nil
}

// RouteDataRequestDispatcher is the worker entry point. It loops on
// PollInterval and retries pending requests via Dispatcher.
type RouteDataRequestDispatcher struct {
	// Queries is the sqlc handle. Required.
	Queries PendingDispatcherQueries

	// Dispatcher is the per-row dispatch primitive. Required.
	Dispatcher PendingRowDispatcher

	// PollInterval is the sleep between sweeps. Defaults to 30s.
	PollInterval time.Duration

	// BatchLimit caps the number of pending rows touched per pass.
	// Defaults to 100.
	BatchLimit int32

	// MaxAttempts is the number of retries (including the first) before a
	// row is failed. Defaults to 10. The counter is in-memory so a server
	// restart resets it -- intentional, because the next sweep will pick
	// up the same rows and the retry budget should be measured in
	// "attempts since the row last had a chance".
	MaxAttempts int

	// now lets tests freeze time so dispatched_at and similar timestamps
	// are deterministic. Defaults to time.Now.
	now func() time.Time

	// attempts tracks the per-request retry count across sweeps. Reset on
	// transition to a terminal state.
	mu       sync.Mutex
	attempts map[int32]int
}

// PendingRowDispatcher is the per-row dispatcher used by the worker. The
// worker passes the full pending row so the implementation can rebuild the
// upload batch from the live segments table without being coupled to the
// row shape via a closure variable.
type PendingRowDispatcher interface {
	DispatchPending(ctx context.Context, row db.ListPendingRouteDataRequestsRow) (online bool, err error)
}

// NewRouteDataRequestDispatcher builds a worker with sane defaults.
func NewRouteDataRequestDispatcher(queries PendingDispatcherQueries, dispatcher PendingRowDispatcher) *RouteDataRequestDispatcher {
	return &RouteDataRequestDispatcher{
		Queries:      queries,
		Dispatcher:   dispatcher,
		PollInterval: defaultDispatcherPollInterval,
		BatchLimit:   defaultDispatcherBatchLimit,
		MaxAttempts:  defaultDispatcherMaxAttempts,
		now:          time.Now,
		attempts:     make(map[int32]int),
	}
}

// Run loops until ctx is cancelled. Per-row failures are logged and do not
// abort the loop; only a context cancellation or a list failure ends the
// run (the list failure is logged and the loop continues).
func (w *RouteDataRequestDispatcher) Run(ctx context.Context) {
	poll := w.PollInterval
	if poll <= 0 {
		poll = defaultDispatcherPollInterval
	}

	// Run one pass immediately so a server restart does not have to wait a
	// full poll for the first sweep. Same pattern as the trip aggregator.
	if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("route_data_dispatcher: pass failed: %v", err)
	}

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("route_data_dispatcher: pass failed: %v", err)
			}
		}
	}
}

// RunOnce executes a single sweep. Per-row errors are logged and do not
// abort the pass; only a list failure is returned.
func (w *RouteDataRequestDispatcher) RunOnce(ctx context.Context) error {
	limit := w.BatchLimit
	if limit <= 0 {
		limit = defaultDispatcherBatchLimit
	}

	rows, err := w.Queries.ListPendingRouteDataRequests(ctx, limit)
	if err != nil {
		return err
	}

	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.processRow(ctx, row)
	}
	return nil
}

// processRow attempts a single dispatch and records the outcome. The retry
// counter is bumped on every visit; once it exceeds MaxAttempts the row is
// marked 'failed' so the polling UI can stop spinning.
func (w *RouteDataRequestDispatcher) processRow(ctx context.Context, row db.ListPendingRouteDataRequestsRow) {
	online, err := w.Dispatcher.DispatchPending(ctx, row)

	switch {
	case online && err == nil:
		// Success. Promote the row to dispatched and clear our retry
		// counter so a future re-pending (a new POST) starts fresh.
		updateErr := w.Queries.UpdateRouteDataRequestStatus(ctx, db.UpdateRouteDataRequestStatusParams{
			ID:           row.RequestID,
			Status:       "dispatched",
			DispatchedAt: pgtype.Timestamptz{Time: w.now().UTC(), Valid: true},
		})
		if updateErr != nil {
			log.Printf("route_data_dispatcher: request=%d: update dispatched: %v", row.RequestID, updateErr)
			return
		}
		w.clearAttempts(row.RequestID)
	case online && err != nil:
		// RPC error. Bump the counter; fail the row when it tips over the
		// budget. Otherwise leave it pending and retry on the next sweep.
		attempts := w.bumpAttempts(row.RequestID)
		if attempts >= w.maxAttempts() {
			updateErr := w.Queries.UpdateRouteDataRequestStatus(ctx, db.UpdateRouteDataRequestStatusParams{
				ID:     row.RequestID,
				Status: "failed",
				Error:  pgtype.Text{String: fmt.Sprintf("dispatch failed after %d attempts: %v", attempts, err), Valid: true},
			})
			if updateErr != nil {
				log.Printf("route_data_dispatcher: request=%d: update failed: %v", row.RequestID, updateErr)
			}
			w.clearAttempts(row.RequestID)
		} else {
			log.Printf("route_data_dispatcher: request=%d: rpc error attempt %d/%d: %v",
				row.RequestID, attempts, w.maxAttempts(), err)
		}
	default:
		// Device still offline. Bump the counter so the row eventually
		// gets failed if the device never comes back.
		attempts := w.bumpAttempts(row.RequestID)
		if attempts >= w.maxAttempts() {
			updateErr := w.Queries.UpdateRouteDataRequestStatus(ctx, db.UpdateRouteDataRequestStatusParams{
				ID:     row.RequestID,
				Status: "failed",
				Error:  pgtype.Text{String: fmt.Sprintf("device %s did not reconnect after %d attempts", row.DongleID, attempts), Valid: true},
			})
			if updateErr != nil {
				log.Printf("route_data_dispatcher: request=%d: update offline-fail: %v", row.RequestID, updateErr)
			}
			w.clearAttempts(row.RequestID)
		}
	}
}

func (w *RouteDataRequestDispatcher) maxAttempts() int {
	if w.MaxAttempts <= 0 {
		return defaultDispatcherMaxAttempts
	}
	return w.MaxAttempts
}

func (w *RouteDataRequestDispatcher) bumpAttempts(id int32) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.attempts == nil {
		w.attempts = make(map[int32]int)
	}
	w.attempts[id]++
	return w.attempts[id]
}

func (w *RouteDataRequestDispatcher) clearAttempts(id int32) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.attempts != nil {
		delete(w.attempts, id)
	}
}
