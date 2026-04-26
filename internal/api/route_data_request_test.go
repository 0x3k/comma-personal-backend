package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/ws"
)

// fakeRouteDataQueries is an in-memory stub of RouteDataRequestQueries.
// It supports the few queries the handler touches so unit tests can run
// without standing up a real Postgres pool.
type fakeRouteDataQueries struct {
	mu sync.Mutex

	// Fixed routes keyed by (dongleID, routeName). The tests seed one or
	// two of these and expect the handler to look them up.
	routes map[string]db.Route
	// Segments keyed by route_id. Order matters: the handler expects the
	// segments in segment_number order.
	segments map[int32][]db.Segment
	// Existing requests keyed by id, plus a ledger of inserted rows so we
	// can assert what the handler created.
	requests    map[int32]db.RouteDataRequest
	nextReqID   int32
	createCalls int
	updateCalls int

	// Optional error injection.
	getRouteErr     error
	listSegmentsErr error
	createErr       error
	updateErr       error
	getLatestErr    error
	getByIDErr      error
}

func newFakeRouteDataQueries() *fakeRouteDataQueries {
	return &fakeRouteDataQueries{
		routes:    make(map[string]db.Route),
		segments:  make(map[int32][]db.Segment),
		requests:  make(map[int32]db.RouteDataRequest),
		nextReqID: 1,
	}
}

func routeKey(dongleID, routeName string) string { return dongleID + "|" + routeName }

func (f *fakeRouteDataQueries) seedRoute(r db.Route) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[routeKey(r.DongleID, r.RouteName)] = r
}

func (f *fakeRouteDataQueries) seedSegments(routeID int32, segs []db.Segment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.segments[routeID] = segs
}

func (f *fakeRouteDataQueries) seedRequest(r db.RouteDataRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ID == 0 {
		r.ID = f.nextReqID
		f.nextReqID++
	}
	f.requests[r.ID] = r
}

func (f *fakeRouteDataQueries) GetRoute(_ context.Context, arg db.GetRouteParams) (db.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getRouteErr != nil {
		return db.Route{}, f.getRouteErr
	}
	r, ok := f.routes[routeKey(arg.DongleID, arg.RouteName)]
	if !ok {
		return db.Route{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeRouteDataQueries) ListSegmentsByRoute(_ context.Context, routeID int32) ([]db.Segment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listSegmentsErr != nil {
		return nil, f.listSegmentsErr
	}
	segs, ok := f.segments[routeID]
	if !ok {
		return nil, nil
	}
	out := make([]db.Segment, len(segs))
	copy(out, segs)
	return out, nil
}

func (f *fakeRouteDataQueries) CreateRouteDataRequest(_ context.Context, arg db.CreateRouteDataRequestParams) (db.RouteDataRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return db.RouteDataRequest{}, f.createErr
	}
	row := db.RouteDataRequest{
		ID:             f.nextReqID,
		RouteID:        arg.RouteID,
		RequestedBy:    arg.RequestedBy,
		RequestedAt:    pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		Kind:           arg.Kind,
		Status:         arg.Status,
		FilesRequested: arg.FilesRequested,
		DispatchedAt:   arg.DispatchedAt,
		Error:          arg.Error,
	}
	f.nextReqID++
	f.requests[row.ID] = row
	return row, nil
}

func (f *fakeRouteDataQueries) GetRouteDataRequestByID(_ context.Context, id int32) (db.RouteDataRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getByIDErr != nil {
		return db.RouteDataRequest{}, f.getByIDErr
	}
	r, ok := f.requests[id]
	if !ok {
		return db.RouteDataRequest{}, pgx.ErrNoRows
	}
	return r, nil
}

func (f *fakeRouteDataQueries) GetLatestRouteDataRequestByRoute(_ context.Context, arg db.GetLatestRouteDataRequestByRouteParams) (db.RouteDataRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getLatestErr != nil {
		return db.RouteDataRequest{}, f.getLatestErr
	}
	var latest db.RouteDataRequest
	var found bool
	for _, r := range f.requests {
		if r.RouteID != arg.RouteID || r.Kind != arg.Kind {
			continue
		}
		if !found || r.RequestedAt.Time.After(latest.RequestedAt.Time) {
			latest = r
			found = true
		}
	}
	if !found {
		return db.RouteDataRequest{}, pgx.ErrNoRows
	}
	return latest, nil
}

func (f *fakeRouteDataQueries) UpdateRouteDataRequestStatus(_ context.Context, arg db.UpdateRouteDataRequestStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	if f.updateErr != nil {
		return f.updateErr
	}
	r, ok := f.requests[arg.ID]
	if !ok {
		return pgx.ErrNoRows
	}
	r.Status = arg.Status
	if arg.DispatchedAt.Valid {
		r.DispatchedAt = arg.DispatchedAt
	}
	if arg.CompletedAt.Valid {
		r.CompletedAt = arg.CompletedAt
	}
	if arg.Error.Valid {
		r.Error = arg.Error
	}
	f.requests[arg.ID] = r
	return nil
}

// fakeDispatcher records the dispatch calls for assertion.
type fakeDispatcher struct {
	online bool
	err    error

	mu    sync.Mutex
	calls []dispatchCall
}

type dispatchCall struct {
	dongleID string
	items    []ws.UploadFileToUrlParams
}

func (f *fakeDispatcher) Dispatch(_ context.Context, dongleID string, items []ws.UploadFileToUrlParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dispatchCall{dongleID: dongleID, items: items})
	return f.online, f.err
}

func (f *fakeDispatcher) lastCall() (dispatchCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return dispatchCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// seedHappyPath sets up a route with N segments and no uploaded full-res
// files. Returns the (queries, route id) pair.
func seedHappyPath(t *testing.T, dongleID, routeName string, numSegments int) (*fakeRouteDataQueries, int32) {
	t.Helper()
	q := newFakeRouteDataQueries()
	const routeID int32 = 42
	q.seedRoute(db.Route{
		ID:        routeID,
		DongleID:  dongleID,
		RouteName: routeName,
	})
	segs := make([]db.Segment, 0, numSegments)
	for i := 0; i < numSegments; i++ {
		segs = append(segs, db.Segment{
			ID:            int32(i + 1),
			RouteID:       routeID,
			SegmentNumber: int32(i),
			QlogUploaded:  true, // already uploaded (auto)
		})
	}
	q.seedSegments(routeID, segs)
	return q, routeID
}

func newPostContext(t *testing.T, e *echo.Echo, dongleID, routeName, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	target := fmt.Sprintf("/v1/route/%s/%s/request_full_data", dongleID, routeName)
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongleID, routeName)
	return c, rec
}

func newGetContext(t *testing.T, e *echo.Echo, dongleID, routeName string, requestID int32) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	target := fmt.Sprintf("/v1/route/%s/%s/request_full_data/%d", dongleID, routeName, requestID)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name", "request_id")
	c.SetParamValues(dongleID, routeName, fmt.Sprintf("%d", requestID))
	return c, rec
}

// withDeviceJWTAuth flags the context as device JWT authenticated for the
// given dongle id, satisfying checkDongleAccess.
func withDeviceJWTAuth(c echo.Context, dongleID string) {
	// No auth_mode set -> falls through to dongle-id check, which is what
	// a JWT-authenticated device hits. Set the dongle id explicitly.
	c.Set("dongle_id", dongleID)
}

func TestRequestFullData_HappyPathDispatchesItems(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 3)

	dispatcher := &fakeDispatcher{online: true}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"all"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	call, ok := dispatcher.lastCall()
	if !ok {
		t.Fatal("expected dispatcher to be called, was not")
	}
	// 3 segments * 4 files (fcamera/ecamera/dcamera + rlog.zst) = 12.
	if len(call.items) != 12 {
		t.Fatalf("dispatched items = %d, want 12", len(call.items))
	}
	// Spot-check the fn for the first item: should be "<route>--0/fcamera.hevc".
	wantFn := routeName + "--0/fcamera.hevc"
	if call.items[0].Path != wantFn {
		t.Errorf("first item fn = %q, want %q", call.items[0].Path, wantFn)
	}
	if !strings.HasSuffix(call.items[0].URL, fmt.Sprintf("/upload/%s/%s/0/fcamera.hevc", dongle, routeName)) {
		t.Errorf("first item url = %q, want suffix /upload/%s/%s/0/fcamera.hevc", call.items[0].URL, dongle, routeName)
	}

	// Persistence: row was created with status=dispatched and the right
	// files_requested count.
	if q.createCalls != 1 {
		t.Errorf("create calls = %d, want 1", q.createCalls)
	}
	for _, row := range q.requests {
		if row.RouteID != routeID {
			continue
		}
		if row.Status != requestStatusDispatched {
			t.Errorf("row status = %q, want %q", row.Status, requestStatusDispatched)
		}
		if row.FilesRequested != 12 {
			t.Errorf("files_requested = %d, want 12", row.FilesRequested)
		}
		if !row.DispatchedAt.Valid {
			t.Error("dispatched_at not stamped")
		}
	}
}

func TestRequestFullData_FullLogsKindOnlyRequestsRlog(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, _ := seedHappyPath(t, dongle, routeName, 2)

	dispatcher := &fakeDispatcher{online: true}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"full_logs"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	call, _ := dispatcher.lastCall()
	if len(call.items) != 2 { // 2 segments * 1 file (rlog.zst)
		t.Fatalf("items = %d, want 2", len(call.items))
	}
	for _, it := range call.items {
		if !strings.HasSuffix(it.Path, "/rlog.zst") {
			t.Errorf("item fn %q does not end with /rlog.zst", it.Path)
		}
	}
}

func TestRequestFullData_SkipsAlreadyUploadedFiles(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 2)
	// Mark fcamera as already uploaded on segment 0.
	segs, _ := q.ListSegmentsByRoute(context.Background(), routeID)
	segs[0].FcameraUploaded = true
	q.seedSegments(routeID, segs)

	dispatcher := &fakeDispatcher{online: true}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, _ := newPostContext(t, e, dongle, routeName, `{"kind":"full_video"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	call, _ := dispatcher.lastCall()
	// 2 segments * 3 video files = 6, minus the 1 already uploaded = 5.
	if len(call.items) != 5 {
		t.Errorf("items = %d, want 5", len(call.items))
	}
}

func TestRequestFullData_DeviceOfflineLeavesRowPending(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, _ := seedHappyPath(t, dongle, routeName, 1)

	dispatcher := &fakeDispatcher{online: false}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"all"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if got := len(dispatcher.calls); got != 1 {
		t.Errorf("dispatcher called %d times, want exactly 1 (offline reply)", got)
	}
	for _, row := range q.requests {
		if row.Status != requestStatusPending {
			t.Errorf("row status = %q, want %q", row.Status, requestStatusPending)
		}
		if row.DispatchedAt.Valid {
			t.Errorf("dispatched_at unexpectedly set: %+v", row.DispatchedAt)
		}
	}
}

func TestRequestFullData_RPCFailureMarksFailed(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, _ := seedHappyPath(t, dongle, routeName, 1)

	dispatcher := &fakeDispatcher{online: true, err: errors.New("rpc boom")}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"all"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	for _, row := range q.requests {
		if row.Status != requestStatusFailed {
			t.Errorf("row status = %q, want %q", row.Status, requestStatusFailed)
		}
		if !row.Error.Valid || !strings.Contains(row.Error.String, "rpc boom") {
			t.Errorf("row error = %+v, want text containing 'rpc boom'", row.Error)
		}
	}
}

func TestRequestFullData_IdempotentWithinWindow(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 1)

	// Pre-existing row, dispatched 5 minutes ago. Same kind. Should be
	// returned as-is (200 OK) without dispatching.
	q.seedRequest(db.RouteDataRequest{
		RouteID:        routeID,
		Kind:           requestKindAll,
		Status:         requestStatusDispatched,
		RequestedAt:    pgtype.Timestamptz{Time: time.Now().Add(-5 * time.Minute), Valid: true},
		FilesRequested: 4,
	})

	dispatcher := &fakeDispatcher{online: true}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"all"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (idempotent re-request); body=%s", rec.Code, rec.Body.String())
	}
	if len(dispatcher.calls) != 0 {
		t.Errorf("dispatcher called %d times, want 0 (idempotent)", len(dispatcher.calls))
	}
	if q.createCalls != 0 {
		t.Errorf("create calls = %d, want 0 (idempotent)", q.createCalls)
	}
}

func TestRequestFullData_FailedRequestDoesNotShortCircuit(t *testing.T) {
	// A failed prior request is NOT idempotent: we want to give the user
	// a clean retry.
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 1)

	q.seedRequest(db.RouteDataRequest{
		RouteID:     routeID,
		Kind:        requestKindAll,
		Status:      requestStatusFailed,
		RequestedAt: pgtype.Timestamptz{Time: time.Now().Add(-5 * time.Minute), Valid: true},
		Error:       pgtype.Text{String: "previous failure", Valid: true},
	})

	dispatcher := &fakeDispatcher{online: true}
	h := NewRouteDataRequestHandler(q, dispatcher, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"all"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (failed not idempotent); body=%s", rec.Code, rec.Body.String())
	}
	if q.createCalls != 1 {
		t.Errorf("create calls = %d, want 1", q.createCalls)
	}
}

func TestRequestFullData_BadKindRejected(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, _ := seedHappyPath(t, dongle, routeName, 1)

	h := NewRouteDataRequestHandler(q, &fakeDispatcher{online: true}, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"junk"}`)
	withDeviceJWTAuth(c, dongle)

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRequestFullData_DongleAccessEnforced(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, _ := seedHappyPath(t, dongle, routeName, 1)
	h := NewRouteDataRequestHandler(q, &fakeDispatcher{online: true}, nil)

	e := echo.New()
	c, rec := newPostContext(t, e, dongle, routeName, `{"kind":"all"}`)
	// Mismatched dongle.
	c.Set("dongle_id", "someone-else")

	if err := h.RequestFullData(c); err != nil {
		t.Fatalf("RequestFullData returned error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestGetFullDataRequest_AutoCompletesWhenFullyUploaded(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 2)
	// Mark every full_video file uploaded on every segment.
	segs, _ := q.ListSegmentsByRoute(context.Background(), routeID)
	for i := range segs {
		segs[i].FcameraUploaded = true
		segs[i].EcameraUploaded = true
		segs[i].DcameraUploaded = true
	}
	q.seedSegments(routeID, segs)

	q.seedRequest(db.RouteDataRequest{
		RouteID:        routeID,
		Kind:           requestKindFullVideo,
		Status:         requestStatusDispatched,
		RequestedAt:    pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		DispatchedAt:   pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		FilesRequested: 6, // 2 segments * 3 video files
	})
	// The seeded row got id=1.
	const reqID int32 = 1

	h := NewRouteDataRequestHandler(q, &fakeDispatcher{online: true}, nil)

	e := echo.New()
	c, rec := newGetContext(t, e, dongle, routeName, reqID)
	withDeviceJWTAuth(c, dongle)

	if err := h.GetFullDataRequest(c); err != nil {
		t.Fatalf("GetFullDataRequest returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp requestStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Request.Status != requestStatusComplete {
		t.Errorf("response status = %q, want %q", resp.Request.Status, requestStatusComplete)
	}
	if resp.Progress.FilesUploaded != 6 || resp.Progress.FilesRequested != 6 || resp.Progress.Percent != 100 {
		t.Errorf("progress = %+v, want 6/6 100%%", resp.Progress)
	}
	if len(resp.Segments) != 2 {
		t.Errorf("segments len = %d, want 2", len(resp.Segments))
	}
	if !resp.Segments[0].FcameraUploaded {
		t.Errorf("segment 0 fcamera flag not set in response")
	}
	// Persisted side-effect: the row now has status=complete.
	row, _ := q.GetRouteDataRequestByID(context.Background(), reqID)
	if row.Status != requestStatusComplete {
		t.Errorf("persisted status = %q, want %q", row.Status, requestStatusComplete)
	}
	if !row.CompletedAt.Valid {
		t.Errorf("persisted completed_at not stamped")
	}
}

func TestGetFullDataRequest_PartialProgressReportsPercent(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 4)
	// 2 of 4 segments fully uploaded.
	segs, _ := q.ListSegmentsByRoute(context.Background(), routeID)
	for i := 0; i < 2; i++ {
		segs[i].RlogUploaded = true
	}
	q.seedSegments(routeID, segs)

	q.seedRequest(db.RouteDataRequest{
		RouteID:        routeID,
		Kind:           requestKindFullLogs,
		Status:         requestStatusDispatched,
		RequestedAt:    pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		FilesRequested: 4, // 4 segments * 1 file (rlog.zst)
	})
	const reqID int32 = 1

	h := NewRouteDataRequestHandler(q, &fakeDispatcher{online: true}, nil)
	e := echo.New()
	c, rec := newGetContext(t, e, dongle, routeName, reqID)
	withDeviceJWTAuth(c, dongle)

	if err := h.GetFullDataRequest(c); err != nil {
		t.Fatalf("GetFullDataRequest returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp requestStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Progress.FilesUploaded != 2 || resp.Progress.Percent != 50 {
		t.Errorf("progress = %+v, want 2/4 (50%%)", resp.Progress)
	}
	if resp.Request.Status == requestStatusComplete {
		t.Errorf("status auto-completed too early: %q", resp.Request.Status)
	}
}

func TestGetFullDataRequest_MismatchedRoute404(t *testing.T) {
	const dongle = "abc123"
	const routeName = "2024-03-15--12-30-00"
	q, routeID := seedHappyPath(t, dongle, routeName, 1)
	q.seedRequest(db.RouteDataRequest{
		RouteID:     routeID,
		Kind:        requestKindAll,
		Status:      requestStatusDispatched,
		RequestedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	})
	// Seed a second route on the same dongle so the GetRoute call doesn't
	// 404 -- we want the row id to belong to a different route.
	q.seedRoute(db.Route{ID: 99, DongleID: dongle, RouteName: "other-route"})

	h := NewRouteDataRequestHandler(q, &fakeDispatcher{online: true}, nil)
	e := echo.New()
	c, rec := newGetContext(t, e, dongle, "other-route", 1)
	withDeviceJWTAuth(c, dongle)

	if err := h.GetFullDataRequest(c); err != nil {
		t.Fatalf("GetFullDataRequest returned error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
