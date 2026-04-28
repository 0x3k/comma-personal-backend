package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/alpr"
	alprcrypto "comma-personal-backend/internal/alpr/crypto"
	"comma-personal-backend/internal/db"
)

// freshKeyring builds a real keyring from a 32-byte random root so the
// crypto path is exercised end-to-end. The HKDF + AES-GCM round-trip
// is fast enough (~tens of microseconds) that doing this per test is
// no measurable overhead, and using the real implementation catches
// any drift between the worker and the crypto package.
func freshKeyring(t *testing.T) *alprcrypto.Keyring {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	k, err := alprcrypto.LoadKeyring(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	return k
}

// fakeDetector is a Detector stub. detect is the per-call hook so a
// test can return canned detections (or an engine error) for each
// frame in turn. A nil hook returns no detections.
type fakeDetector struct {
	mu     sync.Mutex
	calls  int
	detect func(call int, frame []byte) ([]alpr.Detection, error)
}

func (d *fakeDetector) Detect(_ context.Context, frame []byte) ([]alpr.Detection, error) {
	d.mu.Lock()
	call := d.calls
	d.calls++
	d.mu.Unlock()
	if d.detect == nil {
		return nil, nil
	}
	return d.detect(call, frame)
}

// fakeQuerier is a minimal in-memory ALPRDetectorQuerier. The same
// instance is returned from WithTxQuerier; we don't model rollback
// because the worker's transaction commits or fails as a unit, and
// every test asserts on the *committed* end state. A test that wants
// to model a partial-write rollback can wire it via the txCommit hook
// in fakePool below.
type fakeQuerier struct {
	mu sync.Mutex

	// Per-route configuration the test seeds before running.
	routeStartTime time.Time
	geometryTimes  []int64
	geometryWKT    string
	segmentsTotal  int

	// Pre-set per-segment progress so the test can simulate "the
	// extractor has processed segments 0..N already". The detection
	// pipeline marks each segment as it processes it.
	extractorProcessed map[string]bool // key: dongle|route|seg
	detectorProcessed  map[string]bool

	// Recorded writes.
	signatures        []db.UpsertSignatureByKeyParams
	signatureCounts   map[string]int   // signature_key -> sample_count
	signatureIDs      map[string]int64 // signature_key -> id
	nextSignatureID   int64
	detections        []db.InsertDetectionParams
	detectionRows     []db.InsertDetectionRow
	detectionUpdates  []db.UpdateDetectionSignatureParams
	markDetectorCalls int

	// Failure injection.
	insertErr   error
	upsertErr   error
	getRouteErr error
	getGeomErr  error
	markErr     error
	progressErr error
}

func newFakeQuerier() *fakeQuerier {
	return &fakeQuerier{
		extractorProcessed: make(map[string]bool),
		detectorProcessed:  make(map[string]bool),
		signatureCounts:    make(map[string]int),
		signatureIDs:       make(map[string]int64),
	}
}

func progressKey(d, r string, s int32) string {
	return fmt.Sprintf("%s|%s|%d", d, r, s)
}

func (f *fakeQuerier) GetRoute(_ context.Context, arg db.GetRouteParams) (db.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getRouteErr != nil {
		return db.Route{}, f.getRouteErr
	}
	return db.Route{
		DongleID:  arg.DongleID,
		RouteName: arg.RouteName,
		StartTime: pgtype.Timestamptz{Time: f.routeStartTime, Valid: true},
	}, nil
}

func (f *fakeQuerier) GetRouteGeometryAndTimes(_ context.Context, _ db.GetRouteGeometryWKTParams) (db.RouteGeometryAndTimes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getGeomErr != nil {
		return db.RouteGeometryAndTimes{}, f.getGeomErr
	}
	return db.RouteGeometryAndTimes{
		WKT:   pgtype.Text{String: f.geometryWKT, Valid: f.geometryWKT != ""},
		Times: append([]int64(nil), f.geometryTimes...),
	}, nil
}

func (f *fakeQuerier) UpsertSignatureByKey(_ context.Context, arg db.UpsertSignatureByKeyParams) (db.VehicleSignature, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return db.VehicleSignature{}, f.upsertErr
	}
	f.signatures = append(f.signatures, arg)
	id, ok := f.signatureIDs[arg.SignatureKey]
	if !ok {
		f.nextSignatureID++
		id = f.nextSignatureID
		f.signatureIDs[arg.SignatureKey] = id
	}
	f.signatureCounts[arg.SignatureKey]++
	return db.VehicleSignature{
		ID:           id,
		SignatureKey: arg.SignatureKey,
		Make:         arg.Make,
		Model:        arg.Model,
		Color:        arg.Color,
		BodyType:     arg.BodyType,
		Confidence:   arg.Confidence,
		SampleCount:  int32(f.signatureCounts[arg.SignatureKey]),
	}, nil
}

func (f *fakeQuerier) InsertDetection(_ context.Context, arg db.InsertDetectionParams) (db.InsertDetectionRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return db.InsertDetectionRow{}, f.insertErr
	}
	id := int64(len(f.detections) + 1)
	f.detections = append(f.detections, arg)
	row := db.InsertDetectionRow{
		ID:              id,
		DongleID:        arg.DongleID,
		Route:           arg.Route,
		Segment:         arg.Segment,
		FrameOffsetMs:   arg.FrameOffsetMs,
		PlateCiphertext: arg.PlateCiphertext,
		PlateHash:       arg.PlateHash,
		Bbox:            arg.Bbox,
		Confidence:      arg.Confidence,
		OcrCorrected:    arg.OcrCorrected,
		GpsLat:          arg.GpsLat,
		GpsLng:          arg.GpsLng,
		GpsHeadingDeg:   arg.GpsHeadingDeg,
		FrameTs:         arg.FrameTs,
		ThumbPath:       arg.ThumbPath,
	}
	f.detectionRows = append(f.detectionRows, row)
	return row, nil
}

func (f *fakeQuerier) UpdateDetectionSignature(_ context.Context, arg db.UpdateDetectionSignatureParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detectionUpdates = append(f.detectionUpdates, arg)
	return nil
}

func (f *fakeQuerier) MarkDetectorProcessed(_ context.Context, arg db.MarkDetectorProcessedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.detectorProcessed[progressKey(arg.DongleID, arg.Route, arg.Segment)] = true
	// Auto-mark extractor too if the test seeded it as required.
	// (Production sets extractor_processed first; the fake keeps
	// the two maps in sync when the test pre-seeded extractor for
	// the same segment.)
	f.markDetectorCalls++
	return nil
}

func (f *fakeQuerier) CountRouteDetectorProgress(_ context.Context, arg db.CountRouteDetectorProgressParams) (db.CountRouteDetectorProgressRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.progressErr != nil {
		return db.CountRouteDetectorProgressRow{}, f.progressErr
	}
	prefix := arg.DongleID + "|" + arg.Route + "|"
	var ext, det int64
	for k := range f.extractorProcessed {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			ext++
		}
	}
	for k := range f.detectorProcessed {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			det++
		}
	}
	return db.CountRouteDetectorProgressRow{
		ExtractorProcessed: ext,
		DetectorProcessed:  det,
		SegmentsTotal:      int64(f.segmentsTotal),
	}, nil
}

func (f *fakeQuerier) ListDetectionsForRoute(_ context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.ListDetectionsForRouteRow, 0, len(f.detectionRows))
	for _, r := range f.detectionRows {
		if r.DongleID == arg.DongleID && r.Route == arg.Route {
			out = append(out, db.ListDetectionsForRouteRow{
				ID:              r.ID,
				DongleID:        r.DongleID,
				Route:           r.Route,
				Segment:         r.Segment,
				FrameOffsetMs:   r.FrameOffsetMs,
				PlateCiphertext: r.PlateCiphertext,
				PlateHash:       r.PlateHash,
				Bbox:            r.Bbox,
				Confidence:      r.Confidence,
				OcrCorrected:    r.OcrCorrected,
				GpsLat:          r.GpsLat,
				GpsLng:          r.GpsLng,
				GpsHeadingDeg:   r.GpsHeadingDeg,
				FrameTs:         r.FrameTs,
				ThumbPath:       r.ThumbPath,
			})
		}
	}
	return out, nil
}

// WithTxQuerier returns the fake itself: in-memory state already
// participates in whatever the test is asserting. The fakePool's
// Commit/Rollback hooks are the place to model partial-failure
// behaviour, not the querier.
func (f *fakeQuerier) WithTxQuerier(_ pgx.Tx) ALPRDetectorQuerier {
	return f
}

// fakePool is a minimal ALPRDetectorTxBeginner. Each Begin returns a
// fresh fakeTx that records whether Commit/Rollback was called.
type fakePool struct {
	mu        sync.Mutex
	beginErr  error
	txs       []*fakeTx
	beginHook func(call int) error
	calls     int
}

func (p *fakePool) Begin(_ context.Context) (pgx.Tx, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.mu.Unlock()
	if p.beginHook != nil {
		if err := p.beginHook(call); err != nil {
			return nil, err
		}
	}
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	tx := &fakeTx{}
	p.mu.Lock()
	p.txs = append(p.txs, tx)
	p.mu.Unlock()
	return tx, nil
}

// fakeTx implements the entirety of pgx.Tx as no-ops except Commit and
// Rollback, which record their invocation. The worker only ever calls
// Commit / Rollback on the tx (the actual SQL goes through the
// querier interface), so the no-op stubs are fine.
type fakeTx struct {
	committed   bool
	rolledBack  bool
	commitErr   error
	rollbackErr error
}

func (t *fakeTx) Begin(_ context.Context) (pgx.Tx, error) { return nil, errors.New("not implemented") }
func (t *fakeTx) Commit(_ context.Context) error {
	t.committed = true
	return t.commitErr
}
func (t *fakeTx) Rollback(_ context.Context) error {
	t.rolledBack = true
	return t.rollbackErr
}
func (t *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	return 0, errors.New("not implemented")
}
func (t *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	return nil
}
func (t *fakeTx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }
func (t *fakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	return nil, errors.New("not implemented")
}
func (t *fakeTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("not implemented")
}
func (t *fakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("not implemented")
}
func (t *fakeTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return nil }
func (t *fakeTx) Conn() *pgx.Conn                                        { return nil }

// trueP returns a pointer to true; used to flip the alpr_enabled
// test-only override. Pulled into a tiny helper so test setup reads
// declaratively.
func trueP() *bool {
	v := true
	return &v
}

// makeStraightWKT returns a LINESTRING WKT for n vertices walking due
// east at 1ms per metre at the equator. Combined with a parallel times
// array the test gets a deterministic vertex-to-time mapping.
func makeStraightWKT(n int) (string, []int64) {
	const startLat = 37.0
	const startLng = -122.0
	const stepLng = 0.0001 // ~10m east at this latitude
	wkt := "LINESTRING("
	times := make([]int64, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			wkt += ", "
		}
		wkt += fmt.Sprintf("%g %g", startLng+float64(i)*stepLng, startLat)
		times[i] = int64(i) * 1000 // 1s per vertex
	}
	wkt += ")"
	return wkt, times
}

// drainEvents collects everything currently sitting on the completion
// channel and returns it. Used at the end of each test to assert on
// emitted events without racing with a still-running worker.
func drainEvents(ch <-chan RouteAlprDetectionsComplete) []RouteAlprDetectionsComplete {
	var out []RouteAlprDetectionsComplete
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestALPRDetector_BasicWriteAndComplete is the happy-path test. Two
// frames in two segments produce two detections each, and after both
// segments are marked the worker emits a RouteAlprDetectionsComplete.
func TestALPRDetector_BasicWriteAndComplete(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	q.geometryWKT, q.geometryTimes = makeStraightWKT(180) // 3 minutes of GPS
	q.segmentsTotal = 2
	// Pretend the extractor has already processed both segments
	// (the detector usually marks the segment via MarkDetectorProcessed
	// after the extractor wrote extractor_processed; for the
	// completion check we just need extractor_processed >= total).
	q.extractorProcessed[progressKey("dongle1", "2024-01-01--12-00-00", 0)] = true
	q.extractorProcessed[progressKey("dongle1", "2024-01-01--12-00-00", 1)] = true

	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		yMin := 100
		conf := 0.9
		return []alpr.Detection{
			{
				PlateText:  "ABC123",
				Confidence: 0.95,
				BBox:       alpr.Rect{X: 10, Y: 20, W: 100, H: 50},
				Vehicle: &alpr.VehicleAttributes{
					Make:         "toyota",
					Model:        "camry",
					Color:        "silver",
					BodyType:     "sedan",
					SignatureKey: "toyota|camry|silver|sedan",
					YearMin:      &yMin,
					Confidence:   &conf,
				},
			},
		}, nil
	}

	frames := make(chan ExtractedFrame, 4)
	completions := make(chan RouteAlprDetectionsComplete, 4)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Two frames in two segments. Segment 0 frame at 500ms, segment 1
	// frame at 1500ms. With routeStart=12:00:00 and segments at 60s
	// boundaries, frame_ts lands at 12:00:00.500 (segment 0) and
	// 12:01:01.500 (segment 1).
	frames <- ExtractedFrame{DongleID: "dongle1", Route: "2024-01-01--12-00-00", Segment: 0, FrameOffsetMs: 500, JPEG: []byte("frame0")}
	frames <- ExtractedFrame{DongleID: "dongle1", Route: "2024-01-01--12-00-00", Segment: 1, FrameOffsetMs: 1500, JPEG: []byte("frame1")}
	close(frames)
	<-done

	// Two detections persisted, one per frame.
	q.mu.Lock()
	defer q.mu.Unlock()
	if got, want := len(q.detections), 2; got != want {
		t.Fatalf("len(detections) = %d, want %d", got, want)
	}
	// Hash determinism: same plate text -> same hash.
	if !bytes.Equal(q.detections[0].PlateHash, q.detections[1].PlateHash) {
		t.Errorf("plate_hash differs across two detections of the same plate")
	}
	if len(q.detections[0].PlateHash) != 32 {
		t.Errorf("plate_hash length = %d, want 32", len(q.detections[0].PlateHash))
	}
	// Encryption: ciphertext is NOT plaintext bytes. The keyring's
	// AES-GCM output is nonce(12) || ciphertext || tag(16); for a
	// 6-byte plaintext that means at least 34 bytes total, none of
	// which equal the plaintext.
	for i, d := range q.detections {
		if bytes.Contains(d.PlateCiphertext, []byte("ABC123")) {
			t.Errorf("detection[%d] plate_ciphertext leaks plaintext", i)
		}
		if len(d.PlateCiphertext) < 12+16 {
			t.Errorf("detection[%d] plate_ciphertext too short: %d", i, len(d.PlateCiphertext))
		}
		// And the keyring should be able to decrypt it back.
		got, err := keyring.Decrypt(d.PlateCiphertext)
		if err != nil {
			t.Errorf("detection[%d] decrypt: %v", i, err)
		} else if got != "ABC123" {
			t.Errorf("detection[%d] decrypt = %q, want ABC123", i, got)
		}
	}

	// GPS join: every detection should have lat/lng populated and a
	// heading.
	for i, d := range q.detections {
		if !d.GpsLat.Valid || !d.GpsLng.Valid {
			t.Errorf("detection[%d] missing GPS", i)
		}
		if !d.GpsHeadingDeg.Valid {
			t.Errorf("detection[%d] missing heading", i)
		}
	}

	// BBox JSON should round-trip to the engine's rect.
	if !bytes.Contains(q.detections[0].Bbox, []byte(`"x":10`)) {
		t.Errorf("bbox = %s, want x=10", q.detections[0].Bbox)
	}

	// Signature: same key seen twice -> sample_count = 2.
	if got := q.signatureCounts["toyota|camry|silver|sedan"]; got != 2 {
		t.Errorf("signature sample_count = %d, want 2", got)
	}
	// Detection update: both rows should have signature_id = 1.
	if got := len(q.detectionUpdates); got != 2 {
		t.Fatalf("detection updates = %d, want 2", got)
	}
	for i, u := range q.detectionUpdates {
		if !u.SignatureID.Valid || u.SignatureID.Int64 != 1 {
			t.Errorf("update[%d] signature_id = %+v, want 1", i, u.SignatureID)
		}
		if !u.DetMake.Valid || u.DetMake.String != "toyota" {
			t.Errorf("update[%d] det_make = %+v, want toyota", i, u.DetMake)
		}
	}

	// Completion event fired exactly once.
	q.mu.Unlock()
	events := drainEvents(completions)
	q.mu.Lock()
	if len(events) != 1 {
		t.Fatalf("got %d completion events, want 1: %+v", len(events), events)
	}
	if events[0].DongleID != "dongle1" || events[0].Route != "2024-01-01--12-00-00" {
		t.Errorf("event = %+v, want dongle1/2024-01-01--12-00-00", events[0])
	}
	if events[0].TotalDetections != 2 {
		t.Errorf("event total_detections = %d, want 2", events[0].TotalDetections)
	}
}

// TestALPRDetector_NoGPSWithin2sDropsDetection asserts the no-GPS
// branch: a frame whose timestamp lands more than 2s away from any
// vertex must NOT produce a plate_detections row, even though the
// engine returned a detection.
func TestALPRDetector_NoGPSWithin2sDropsDetection(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	// Geometry has only 5 vertices, all at routeRel ms in [0, 4000].
	// A frame at offset 10000ms within segment 0 lands at
	// route-relative ms = 10000, which is >2s from any vertex.
	q.geometryWKT, q.geometryTimes = makeStraightWKT(5) // 0..4000 ms
	q.segmentsTotal = 1
	q.extractorProcessed[progressKey("dongle1", "2024-01-01--12-00-00", 0)] = true

	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		return []alpr.Detection{{
			PlateText:  "FAR1",
			Confidence: 0.95,
			BBox:       alpr.Rect{X: 0, Y: 0, W: 10, H: 10},
		}}, nil
	}

	frames := make(chan ExtractedFrame, 1)
	completions := make(chan RouteAlprDetectionsComplete, 1)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	frames <- ExtractedFrame{
		DongleID:      "dongle1",
		Route:         "2024-01-01--12-00-00",
		Segment:       0,
		FrameOffsetMs: 10000,
		JPEG:          []byte("frame"),
	}
	close(frames)
	<-done

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.detections) != 0 {
		t.Errorf("expected zero detections written, got %d", len(q.detections))
	}
	// MarkDetectorProcessed should still have been called -- we
	// finished with the frame, even if we couldn't localize it.
	if q.markDetectorCalls == 0 {
		t.Errorf("MarkDetectorProcessed was not called for the dropped-no-GPS frame")
	}
}

// TestALPRDetector_EngineErrorDoesNotCrash drives a single frame
// through with the engine returning ErrEngineUnreachable. Asserts:
//   - no row is persisted,
//   - the worker keeps running (channel can still be closed cleanly).
func TestALPRDetector_EngineErrorDoesNotCrash(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Now().UTC()
	q.geometryWKT, q.geometryTimes = makeStraightWKT(60)
	q.segmentsTotal = 1

	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		return nil, fmt.Errorf("%w: connection refused", alpr.ErrEngineUnreachable)
	}

	frames := make(chan ExtractedFrame, 1)
	completions := make(chan RouteAlprDetectionsComplete, 1)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 0, FrameOffsetMs: 0, JPEG: []byte("frame")}
	close(frames)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit on channel close after engine error")
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.detections) != 0 {
		t.Errorf("engine error path persisted %d detections, want 0", len(q.detections))
	}
}

// TestALPRDetector_LowConfidenceFiltered asserts a detection below
// confidence_min is filtered out. The settings store is nil so the
// default (set via DefaultConfidenceMin) applies.
func TestALPRDetector_LowConfidenceFiltered(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Now().UTC()
	q.geometryWKT, q.geometryTimes = makeStraightWKT(60)
	q.segmentsTotal = 1
	q.extractorProcessed[progressKey("d", "r", 0)] = true

	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		return []alpr.Detection{
			{PlateText: "LOW", Confidence: 0.1, BBox: alpr.Rect{X: 0, Y: 0, W: 1, H: 1}},
			{PlateText: "HI", Confidence: 0.99, BBox: alpr.Rect{X: 0, Y: 0, W: 1, H: 1}},
		}, nil
	}

	frames := make(chan ExtractedFrame, 1)
	completions := make(chan RouteAlprDetectionsComplete, 1)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.DefaultConfidenceMin = 0.5
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 0, FrameOffsetMs: 100, JPEG: []byte("f")}
	close(frames)
	<-done

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.detections) != 1 {
		t.Fatalf("got %d detections, want 1 (low-confidence filtered)", len(q.detections))
	}
	got, err := keyring.Decrypt(q.detections[0].PlateCiphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "HI" {
		t.Errorf("kept plate = %q, want HI", got)
	}
}

// TestALPRDetector_NilKeyringIdles asserts the defense-in-depth path:
// when the keyring is nil the worker logs once and drains frames
// without writing anything.
func TestALPRDetector_NilKeyringIdles(t *testing.T) {
	q := newFakeQuerier()
	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		t.Fatalf("Detect should not be called when keyring is nil")
		return nil, nil
	}

	frames := make(chan ExtractedFrame, 1)
	completions := make(chan RouteAlprDetectionsComplete, 1)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, nil /* keyring */, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 0, JPEG: []byte("x")}
	close(frames)
	<-done

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.detections) != 0 {
		t.Errorf("nil keyring persisted %d detections, want 0", len(q.detections))
	}
}

// TestALPRDetector_CompleteFiresOncePerRoute asserts the dedup guard:
// even if MarkDetectorProcessed is called multiple times for the same
// (already-complete) route, the completion event fires exactly once.
func TestALPRDetector_CompleteFiresOncePerRoute(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Now().UTC()
	q.geometryWKT, q.geometryTimes = makeStraightWKT(60)
	q.segmentsTotal = 2
	q.extractorProcessed[progressKey("d", "r", 0)] = true
	q.extractorProcessed[progressKey("d", "r", 1)] = true

	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		return nil, nil // empty detections; we only care about progress
	}

	frames := make(chan ExtractedFrame, 4)
	completions := make(chan RouteAlprDetectionsComplete, 4)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Send segment 0 then segment 1. After segment 1 the route is
	// complete; emission fires.
	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 0, FrameOffsetMs: 0, JPEG: []byte("a")}
	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 1, FrameOffsetMs: 0, JPEG: []byte("b")}
	// And a duplicate-frame for segment 1 just to see that a
	// follow-up MarkDetectorProcessed does not re-fire emission.
	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 1, FrameOffsetMs: 1000, JPEG: []byte("c")}
	close(frames)
	<-done

	events := drainEvents(completions)
	if len(events) != 1 {
		t.Fatalf("got %d completion events, want exactly 1: %+v", len(events), events)
	}
}

// TestALPRDetector_NoSignatureSkipsUpsert asserts that a detection
// without Vehicle attributes does NOT touch vehicle_signatures. The
// detection row still lands; only the signature path is skipped.
func TestALPRDetector_NoSignatureSkipsUpsert(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Now().UTC()
	q.geometryWKT, q.geometryTimes = makeStraightWKT(60)
	q.segmentsTotal = 1
	q.extractorProcessed[progressKey("d", "r", 0)] = true

	det := &fakeDetector{}
	det.detect = func(_ int, _ []byte) ([]alpr.Detection, error) {
		return []alpr.Detection{
			{PlateText: "PLAIN", Confidence: 0.95, BBox: alpr.Rect{X: 0, Y: 0, W: 10, H: 10}},
		}, nil
	}

	frames := make(chan ExtractedFrame, 1)
	completions := make(chan RouteAlprDetectionsComplete, 1)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 0, FrameOffsetMs: 100, JPEG: []byte("f")}
	close(frames)
	<-done

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.detections) != 1 {
		t.Fatalf("got %d detections, want 1", len(q.detections))
	}
	if len(q.signatures) != 0 {
		t.Errorf("got %d signature upserts, want 0 (vehicle was nil)", len(q.signatures))
	}
	if len(q.detectionUpdates) != 0 {
		t.Errorf("got %d detection-signature updates, want 0", len(q.detectionUpdates))
	}
}

// TestALPRDetector_HashDeterminismAcrossNormalization asserts that
// "ABC 123" and "abc-123" hash to the same bytes. The crypto package
// owns the normalization rules; this test pins them at the worker
// boundary so a future change to normalize() that drops dashes from
// canonical form (or vice versa) would surface as a hash drift here.
func TestALPRDetector_HashDeterminismAcrossNormalization(t *testing.T) {
	keyring := freshKeyring(t)
	q := newFakeQuerier()
	q.routeStartTime = time.Now().UTC()
	q.geometryWKT, q.geometryTimes = makeStraightWKT(60)
	q.segmentsTotal = 1
	q.extractorProcessed[progressKey("d", "r", 0)] = true

	plates := []string{"ABC 123", "abc-123", "abc.123", "ABC123"}
	det := &fakeDetector{}
	det.detect = func(call int, _ []byte) ([]alpr.Detection, error) {
		return []alpr.Detection{
			{PlateText: plates[call], Confidence: 0.95, BBox: alpr.Rect{X: 0, Y: 0, W: 10, H: 10}},
		}, nil
	}

	frames := make(chan ExtractedFrame, len(plates))
	completions := make(chan RouteAlprDetectionsComplete, 1)
	w := NewALPRDetector(frames, det, q, &fakePool{}, nil, keyring, nil, completions)
	w.alprEnabledForTest = trueP()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	for i := range plates {
		frames <- ExtractedFrame{DongleID: "d", Route: "r", Segment: 0, FrameOffsetMs: int(i * 100), JPEG: []byte("f")}
	}
	close(frames)
	<-done

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.detections) != len(plates) {
		t.Fatalf("got %d detections, want %d", len(q.detections), len(plates))
	}
	first := q.detections[0].PlateHash
	for i := 1; i < len(q.detections); i++ {
		if !bytes.Equal(first, q.detections[i].PlateHash) {
			t.Errorf("detection[%d] hash differs from detection[0] (plate %q vs %q)",
				i, plates[i], plates[0])
		}
	}
}
