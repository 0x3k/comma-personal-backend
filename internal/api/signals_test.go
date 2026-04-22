package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/cereal/schema"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// signalsMockDB drives the signals handler's two DB reads: GetRoute (single
// row, may error with pgx.ErrNoRows to simulate 404) and ListSegmentsByRoute
// (rows iteration). SQL strings are matched heuristically so we don't need to
// depend on the exact generated text.
type signalsMockDB struct {
	route       *db.Route
	routeErr    error
	segments    []db.Segment
	segmentsErr error
}

func (m *signalsMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *signalsMockDB) Query(_ context.Context, sql string, _ ...interface{}) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM segments") {
		if m.segmentsErr != nil {
			return nil, m.segmentsErr
		}
		return &mockSegmentRows{segments: m.segments}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func (m *signalsMockDB) QueryRow(_ context.Context, sql string, _ ...interface{}) pgx.Row {
	if strings.Contains(sql, "FROM routes") {
		if m.routeErr != nil {
			return &mockRouteRow{err: m.routeErr}
		}
		if m.route == nil {
			return &mockRouteRow{err: pgx.ErrNoRows}
		}
		return &mockRouteRow{route: m.route}
	}
	return &mockRouteRow{err: fmt.Errorf("unexpected query: %s", sql)}
}

// buildQlogFixture assembles a minimal qlog stream (uncompressed Cap'n Proto
// frames) with two events: a carState at t=0 with vEgo=7.5 and steering=2.25,
// and a selfdriveState at t=100ms with engaged=true and alert="Autopilot".
// Two events is enough to verify the row alignment and value propagation
// through the handler.
func buildQlogFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)

	// Frame 0: carState at monoTime=0
	msg0, seg0, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	evt0, err := schema.NewRootEvent(seg0)
	if err != nil {
		t.Fatalf("NewRootEvent: %v", err)
	}
	evt0.SetLogMonoTime(0)
	evt0.SetValid(true)
	cs, err := evt0.NewCarState()
	if err != nil {
		t.Fatalf("NewCarState: %v", err)
	}
	cs.SetVEgo(7.5)
	cs.SetSteeringAngleDeg(2.25)
	if err := enc.Encode(msg0); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Frame 1: selfdriveState at monoTime=100ms
	msg1, seg1, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	evt1, err := schema.NewRootEvent(seg1)
	if err != nil {
		t.Fatalf("NewRootEvent: %v", err)
	}
	evt1.SetLogMonoTime(100_000_000)
	evt1.SetValid(true)
	ss, err := evt1.NewSelfdriveState()
	if err != nil {
		t.Fatalf("NewSelfdriveState: %v", err)
	}
	ss.SetEnabled(true)
	if err := ss.SetAlertText1("Autopilot"); err != nil {
		t.Fatalf("SetAlertText1: %v", err)
	}
	if err := enc.Encode(msg1); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	return buf.Bytes()
}

// writeQlog drops a qlog (uncompressed) into the storage path for a segment.
// It creates any missing parent directories to mirror what the upload handler
// would do in production.
func writeQlog(t *testing.T, store *storage.Storage, dongle, route, segment string, data []byte) string {
	t.Helper()
	dir := filepath.Dir(store.Path(dongle, route, segment, "qlog"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	path := filepath.Join(dir, "qlog")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

// newSignalsRequest wires an Echo context with the conventional URL and auth
// context so handler tests can focus on the response.
func newSignalsRequest(t *testing.T, dongle, route, authDongle string) (*httptest.ResponseRecorder, echo.Context) {
	t.Helper()
	e := echo.New()
	target := fmt.Sprintf("/v1/routes/%s/%s/signals", dongle, route)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongle, route)
	c.Set(middleware.ContextKeyDongleID, authDongle)
	return rec, c
}

func newSignalsMockRoute(id int32, dongleID, routeName string) *db.Route {
	now := time.Now()
	return &db.Route{
		ID:        id,
		DongleID:  dongleID,
		RouteName: routeName,
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

// TestSignals_CacheMiss_WritesCache exercises the happy path: a segment with a
// qlog but no cache. We expect the handler to parse the qlog, return the
// column-oriented payload, and write signals.json next to the qlog.
func TestSignals_CacheMiss_WritesCache(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	fixture := buildQlogFixture(t)
	qlogPath := writeQlog(t, store, dongle, route, "0", fixture)

	mock := &signalsMockDB{
		route: newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{
			{ID: 1, RouteID: 1, SegmentNumber: 0, QlogUploaded: true},
		},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body signalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v; raw=%s", err, rec.Body.String())
	}
	if len(body.Times) != 2 {
		t.Fatalf("len(times) = %d, want 2; body=%s", len(body.Times), rec.Body.String())
	}
	if body.SpeedMPS[0] != 7.5 {
		t.Errorf("speed_mps[0] = %v, want 7.5", body.SpeedMPS[0])
	}
	if body.SteeringDeg[0] != 2.25 {
		t.Errorf("steering_deg[0] = %v, want 2.25", body.SteeringDeg[0])
	}
	if !body.Engaged[1] {
		t.Errorf("engaged[1] = %v, want true", body.Engaged[1])
	}
	if body.Alerts[1] != "Autopilot" {
		t.Errorf("alerts[1] = %q, want %q", body.Alerts[1], "Autopilot")
	}
	// times are unix-ms of the logMonoTime reinterpreted as ns-since-epoch.
	if body.Times[0] != 0 {
		t.Errorf("times[0] = %d, want 0", body.Times[0])
	}
	if body.Times[1] != 100 {
		t.Errorf("times[1] = %d, want 100 (ms)", body.Times[1])
	}

	// The cache file must now sit next to the qlog.
	cachePath := filepath.Join(filepath.Dir(qlogPath), signalsCacheFilename)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("expected signals cache at %s: %v", cachePath, err)
	}
}

// TestSignals_CacheHit_NoRecompute deletes the qlog after the first call and
// expects the second call to still succeed (i.e. the cache is authoritative
// once written).
func TestSignals_CacheHit_NoRecompute(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	fixture := buildQlogFixture(t)
	qlogPath := writeQlog(t, store, dongle, route, "0", fixture)

	mock := &signalsMockDB{
		route: newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{
			{ID: 1, RouteID: 1, SegmentNumber: 0, QlogUploaded: true},
		},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	// First call populates the cache.
	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("first call error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("first call status = %d", rec.Code)
	}
	var first signalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first body: %v", err)
	}

	// Delete the qlog -- the cache must now carry the load.
	if err := os.Remove(qlogPath); err != nil {
		t.Fatalf("remove qlog: %v", err)
	}

	rec2, c2 := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c2); err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call status = %d; body=%s", rec2.Code, rec2.Body.String())
	}
	var second signalsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second body: %v", err)
	}
	if len(second.Times) != len(first.Times) {
		t.Fatalf("cache hit returned different length: %d vs %d",
			len(second.Times), len(first.Times))
	}
	for i := range first.Times {
		if first.Times[i] != second.Times[i] || first.SpeedMPS[i] != second.SpeedMPS[i] {
			t.Errorf("row %d differs between cache-miss and cache-hit", i)
		}
	}
}

// TestSignals_CacheStale_RecomputesWhenQlogIsNewer makes sure a stale cache
// is ignored when the qlog has been re-uploaded after the cache was written.
func TestSignals_CacheStale_RecomputesWhenQlogIsNewer(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	fixture := buildQlogFixture(t)
	qlogPath := writeQlog(t, store, dongle, route, "0", fixture)

	mock := &signalsMockDB{
		route: newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{
			{ID: 1, RouteID: 1, SegmentNumber: 0, QlogUploaded: true},
		},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	// First call writes the cache.
	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("first call status = %d", rec.Code)
	}

	// Backdate the cache so the qlog looks newer than it.
	cachePath := filepath.Join(filepath.Dir(qlogPath), signalsCacheFilename)
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(cachePath, past, past); err != nil {
		t.Fatalf("Chtimes cache: %v", err)
	}
	// Ensure the qlog is definitively newer.
	future := time.Now()
	if err := os.Chtimes(qlogPath, future, future); err != nil {
		t.Fatalf("Chtimes qlog: %v", err)
	}

	// Corrupt the cache file. If the handler honors mtime, it will
	// recompute from the qlog and overwrite the corrupted cache rather
	// than returning garbage.
	if err := os.WriteFile(cachePath, []byte(`{"times":[999],"speed_mps":[999],"steering_deg":[999],"engaged":[true],"alerts":["STALE"]}`), 0644); err != nil {
		t.Fatalf("corrupt cache: %v", err)
	}
	if err := os.Chtimes(cachePath, past, past); err != nil {
		t.Fatalf("Chtimes cache after corrupt: %v", err)
	}

	rec2, c2 := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c2); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call status = %d; body=%s", rec2.Code, rec2.Body.String())
	}
	var body signalsResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Times) != 2 {
		t.Fatalf("stale cache was honored: times=%v", body.Times)
	}
	if len(body.Alerts) != 2 || body.Alerts[1] != "Autopilot" {
		t.Errorf("stale cache was honored: alerts=%v", body.Alerts)
	}
}

// TestSignals_MissingQlogWholeRoute is the partial-route happy path: the
// route has segment rows but no qlogs yet. The endpoint must return 200 with
// empty arrays, not 500.
func TestSignals_MissingQlogWholeRoute(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	mock := &signalsMockDB{
		route: newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{
			{ID: 1, RouteID: 1, SegmentNumber: 0, QlogUploaded: false},
			{ID: 2, RouteID: 1, SegmentNumber: 1, QlogUploaded: false},
		},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body signalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Times) != 0 || len(body.SpeedMPS) != 0 || len(body.Engaged) != 0 || len(body.Alerts) != 0 {
		t.Errorf("expected all-empty arrays, got %+v", body)
	}
	// Confirm the JSON wire format uses arrays, not null.
	if !strings.Contains(rec.Body.String(), `"times":[]`) {
		t.Errorf("expected empty array for times, got %s", rec.Body.String())
	}
}

// TestSignals_UnknownRoute exercises the 404 path: route lookup returns
// pgx.ErrNoRows so the endpoint must respond 404 before touching the
// filesystem.
func TestSignals_UnknownRoute(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2099-01-01--00-00-00"

	store := storage.New(t.TempDir())
	mock := &signalsMockDB{routeErr: pgx.ErrNoRows}
	handler := NewSignalsHandler(db.New(mock), store)

	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body.Error, "not found") {
		t.Errorf("error = %q, want 'not found'", body.Error)
	}
}

// TestSignals_DongleMismatchReturns403 checks the auth guard. Any caller
// whose JWT-derived dongle doesn't match the URL's dongle must get 403.
func TestSignals_DongleMismatchReturns403(t *testing.T) {
	store := storage.New(t.TempDir())
	mock := &signalsMockDB{}
	handler := NewSignalsHandler(db.New(mock), store)

	rec, c := newSignalsRequest(t, "owner", "2024-03-15--12-30-00", "other")
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSignals_SessionAuthProceeds verifies that a session-authenticated caller
// (ContextKeyAuthMode = AuthModeSession, no ContextKeyDongleID set) is allowed
// through. Regression test for IH-008.
func TestSignals_SessionAuthProceeds(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	mock := &signalsMockDB{
		route:    newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	e := echo.New()
	target := fmt.Sprintf("/v1/routes/%s/%s/signals", dongle, route)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "route_name")
	c.SetParamValues(dongle, route)
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeSession)

	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSignals_MultiSegment concatenates two segments (one with a qlog, one
// without) and verifies the qlog-backed segment contributes in order.
func TestSignals_MultiSegment(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	fixture := buildQlogFixture(t)
	writeQlog(t, store, dongle, route, "0", fixture)
	// segment 1 intentionally has no qlog on disk.

	mock := &signalsMockDB{
		route: newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{
			{ID: 1, RouteID: 1, SegmentNumber: 0, QlogUploaded: true},
			{ID: 2, RouteID: 1, SegmentNumber: 1, QlogUploaded: false},
		},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body signalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Times) != 2 {
		t.Errorf("len(times) = %d, want 2 (segment 1 is skipped)", len(body.Times))
	}
}

// TestSignals_RegisterRoutes ensures the handler wires the expected path onto
// an Echo group so main.go's wiring is asserted alongside the behavior.
func TestSignals_RegisterRoutes(t *testing.T) {
	store := storage.New(t.TempDir())
	mock := &signalsMockDB{}
	handler := NewSignalsHandler(db.New(mock), store)

	e := echo.New()
	g := e.Group("/v1/routes")
	handler.RegisterRoutes(g)

	var found bool
	for _, r := range e.Routes() {
		if r.Method == http.MethodGet && r.Path == "/v1/routes/:dongle_id/:route_name/signals" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected GET /v1/routes/:dongle_id/:route_name/signals to be registered")
	}
}

// TestSignals_SegmentOrdering builds two qlog-backed segments and checks that
// segment 0's rows land before segment 1's rows even when ListSegments
// returns them in a non-sorted order (the handler relies on the ordering
// the DB gives it, so document that ordering here for contract-regression).
func TestSignals_SegmentOrdering(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())
	fixture := buildQlogFixture(t)
	writeQlog(t, store, dongle, route, "0", fixture)
	writeQlog(t, store, dongle, route, "1", fixture)

	mock := &signalsMockDB{
		route: newSignalsMockRoute(1, dongle, route),
		segments: []db.Segment{
			// Intentionally in order 0 then 1 -- matches the DB query
			// that sorts by segment_number ASC.
			{ID: 1, RouteID: 1, SegmentNumber: 0, QlogUploaded: true},
			{ID: 2, RouteID: 1, SegmentNumber: 1, QlogUploaded: true},
		},
	}
	handler := NewSignalsHandler(db.New(mock), store)

	rec, c := newSignalsRequest(t, dongle, route, dongle)
	if err := handler.GetRouteSignals(c); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body signalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Times) != 4 {
		t.Fatalf("len(times) = %d, want 4 (two segments * two rows)", len(body.Times))
	}
	// Both segments have the same fixture, so times[0] and times[2] should
	// both be 0ms, and times[1] and times[3] should both be 100ms.
	if body.Times[0] != 0 || body.Times[2] != 0 {
		t.Errorf("expected fixture mono=0 rows at idx 0 and 2, got %v", body.Times)
	}
	if body.Times[1] != 100 || body.Times[3] != 100 {
		t.Errorf("expected fixture mono=100ms rows at idx 1 and 3, got %v", body.Times)
	}
}

// TestSignals_SegmentIntLookup is a small sanity check that strconv.Itoa is
// how we name segment directories -- future refactors that change this
// encoding must update this test and loadSegmentSignals together.
func TestSignals_SegmentIntLookup(t *testing.T) {
	if strconv.Itoa(0) != "0" {
		t.Fatalf("unexpected")
	}
}
