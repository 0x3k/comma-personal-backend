package worker

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// fakeALPRQuerier is a minimal in-memory stand-in for *db.Queries. Each
// test gets its own instance; concurrent access is guarded so the
// scanner goroutines and the test goroutine can race on it safely.
type fakeALPRQuerier struct {
	mu        sync.Mutex
	routes    []db.Route
	processed map[string]bool // dongle|route|seg -> processed
	markCalls map[string]int  // count of MarkExtractorProcessed calls
	listErr   error
	checkErr  error
}

func newFakeALPRQuerier() *fakeALPRQuerier {
	return &fakeALPRQuerier{
		processed: make(map[string]bool),
		markCalls: make(map[string]int),
	}
}

func (f *fakeALPRQuerier) ListRecentRoutes(_ context.Context, _ int32) ([]db.Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]db.Route, len(f.routes))
	copy(out, f.routes)
	return out, nil
}

func (f *fakeALPRQuerier) IsExtractorProcessed(_ context.Context, arg db.IsExtractorProcessedParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkErr != nil {
		return false, f.checkErr
	}
	return f.processed[fakeKey(arg.DongleID, arg.Route, int(arg.Segment))], nil
}

func (f *fakeALPRQuerier) MarkExtractorProcessed(_ context.Context, arg db.MarkExtractorProcessedParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fakeKey(arg.DongleID, arg.Route, int(arg.Segment))
	f.processed[k] = true
	f.markCalls[k]++
	return nil
}

func (f *fakeALPRQuerier) addRoute(dongle, route string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = append(f.routes, db.Route{DongleID: dongle, RouteName: route})
}

func (f *fakeALPRQuerier) markCount(dongle, route string, seg int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markCalls[fakeKey(dongle, route, seg)]
}

func fakeKey(dongle, route string, seg int) string {
	return dongle + "|" + route + "|" + strconv.Itoa(seg)
}

// stageFcamera copies the test fixture into the right path under
// store so the worker's segmentNeedsExtract / runFFmpeg pair can find
// it.
func stageFcamera(t *testing.T, store *storage.Storage, dongle, route string, seg int) {
	t.Helper()
	src, err := filepath.Abs("testdata/test_segment.hevc")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("open fixture %s: %v (regenerate with `ffmpeg -f lavfi -i testsrc=duration=3:size=320x180:rate=10 -c:v libx265 -y internal/worker/testdata/test_segment.hevc`)", src, err)
	}
	defer f.Close()
	if err := store.Store(dongle, route, strconv.Itoa(seg), FCameraFile, f); err != nil {
		t.Fatalf("Store: %v", err)
	}
}

// requireFFmpeg skips when ffmpeg is not on PATH so the suite remains
// runnable in environments that do not bundle ffmpeg.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("alpr extractor tests require unix shell semantics")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
}

// TestALPRExtractor_ExtractsFramesAt2FPS drives a single segment
// through processSegment at 2 fps. Asserts:
//   - the expected number of frames is emitted (3s fixture * 2 fps ~= 6),
//   - frame_offset_ms is monotonically increasing at 500ms cadence,
//   - the JPEG bytes carry SOI/EOI markers,
//   - the segment is marked processed in the (fake) database.
func TestALPRExtractor_ExtractsFramesAt2FPS(t *testing.T) {
	requireFFmpeg(t)

	store := storage.New(t.TempDir())
	q := newFakeALPRQuerier()
	const dongle = "dongle1"
	const route = "2024-01-01--00-00-00"
	const seg = 0
	q.addRoute(dongle, route)
	stageFcamera(t, store, dongle, route, seg)

	frames := make(chan ExtractedFrame, 32)
	ext := NewALPRExtractor(q, store, nil, frames, nil)
	ext.DefaultFramesPerSecond = 2
	// Force gating to true: the test does not exercise Settings.
	prev := alprEnabledForTest
	alprEnabledForTest = boolPtr(true)
	t.Cleanup(func() { alprEnabledForTest = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// processSegment runs synchronously; spin it on a goroutine so we
	// can drain frames as they arrive.
	processDone := make(chan struct{})
	go func() {
		ext.processSegment(ctx, extractorJob{dongleID: dongle, route: route, segment: seg})
		close(processDone)
	}()

	var got []ExtractedFrame
	timeout := time.After(20 * time.Second)
collect:
	for {
		select {
		case <-timeout:
			t.Fatalf("timed out collecting frames; got %d", len(got))
		case <-processDone:
			// Drain any remaining frames already in the channel.
			for {
				select {
				case f := <-frames:
					got = append(got, f)
				default:
					break collect
				}
			}
		case f := <-frames:
			got = append(got, f)
		}
	}

	if len(got) < 5 || len(got) > 7 {
		// 3-second fixture at 2 fps yields ~6 frames. Allow +/-1 for
		// the fps filter's edge behavior on the first/last frame.
		t.Errorf("frame count = %d, want 5-7", len(got))
	}

	// Monotonic offsets at exactly 500ms cadence (1000/fps).
	for i := 0; i < len(got); i++ {
		want := i * 500
		if got[i].FrameOffsetMs != want {
			t.Errorf("frame[%d].FrameOffsetMs = %d, want %d", i, got[i].FrameOffsetMs, want)
		}
		if got[i].DongleID != dongle || got[i].Route != route || got[i].Segment != seg {
			t.Errorf("frame[%d] identity = (%q,%q,%d), want (%q,%q,%d)",
				i, got[i].DongleID, got[i].Route, got[i].Segment, dongle, route, seg)
		}
		if !bytes.HasPrefix(got[i].JPEG, jpegSOI) {
			n := len(got[i].JPEG)
			if n > 4 {
				n = 4
			}
			t.Errorf("frame[%d] missing SOI prefix; first %d bytes = %x", i, n, got[i].JPEG[:n])
		}
		if !bytes.HasSuffix(got[i].JPEG, jpegEOI) {
			n := len(got[i].JPEG) - 4
			if n < 0 {
				n = 0
			}
			t.Errorf("frame[%d] missing EOI suffix; last 4 bytes = %x", i, got[i].JPEG[n:])
		}
	}

	if q.markCount(dongle, route, seg) == 0 {
		t.Errorf("expected MarkExtractorProcessed to be called once on success, got 0")
	}
}

// TestALPRExtractor_Idempotent re-runs segmentNeedsExtract on a
// segment whose processed_at_extractor row is already set. Expectation:
// the gate returns false, no ffmpeg invocation, no Mark call.
func TestALPRExtractor_Idempotent(t *testing.T) {
	store := storage.New(t.TempDir())
	q := newFakeALPRQuerier()
	const dongle = "dongle2"
	const route = "2024-01-02--00-00-00"
	const seg = 0
	q.addRoute(dongle, route)
	stageFcamera(t, store, dongle, route, seg)
	q.processed[fakeKey(dongle, route, seg)] = true

	frames := make(chan ExtractedFrame, 32)
	ext := NewALPRExtractor(q, store, nil, frames, nil)

	ctx := context.Background()
	if ext.segmentNeedsExtract(ctx, dongle, route, seg) {
		t.Fatalf("segmentNeedsExtract returned true on a pre-processed segment")
	}

	// Confirm no frames pushed.
	select {
	case f := <-frames:
		t.Errorf("expected no frames; got %+v", f)
	case <-time.After(50 * time.Millisecond):
	}

	// MarkExtractorProcessed counter is 0: the pre-mark above was a
	// direct map assignment, not a query call.
	if got := q.markCount(dongle, route, seg); got != 0 {
		t.Errorf("MarkExtractorProcessed called %d times on idempotent path; want 0", got)
	}
}

// TestALPRExtractor_CancelDuringExtraction starts Run with a single
// pending segment and a tiny output buffer. The producer will block
// on the full channel almost immediately. We then cancel the context
// and assert:
//   - Run returns within a reasonable bound (no hang),
//   - the Frames channel is closed by Run during shutdown,
//   - the goroutine count after shutdown is at or below the baseline.
func TestALPRExtractor_CancelDuringExtraction(t *testing.T) {
	requireFFmpeg(t)

	store := storage.New(t.TempDir())
	q := newFakeALPRQuerier()
	const dongle = "dongle3"
	const route = "2024-01-03--00-00-00"
	const seg = 0
	q.addRoute(dongle, route)
	stageFcamera(t, store, dongle, route, seg)

	frames := make(chan ExtractedFrame, 1)
	ext := NewALPRExtractor(q, store, nil, frames, nil)
	ext.DefaultFramesPerSecond = 2
	ext.PollInterval = 10 * time.Millisecond
	prev := alprEnabledForTest
	alprEnabledForTest = boolPtr(true)
	t.Cleanup(func() { alprEnabledForTest = prev })

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ext.Run(ctx)
		close(done)
	}()

	// Give Run a moment to spawn ffmpeg and start producing frames.
	// The 1-slot buffer guarantees the producer will block soon
	// (before the test cancels).
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return within 10s after cancel")
	}

	// Frames must be closed by Run on shutdown. Drain any buffered
	// frames first; the channel becomes closed once empty.
	drainDeadline := time.After(2 * time.Second)
drain:
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				break drain
			}
		case <-drainDeadline:
			t.Fatalf("Frames channel not closed within 2s after Run returned")
		}
	}

	// Allow lingering child-process reapers a moment to wind down,
	// then assert no significant leak. We tolerate +1 goroutine for
	// scheduling jitter.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - before; leaked > 1 {
		t.Errorf("goroutine leak: before=%d after=%d (delta=%d)", before, runtime.NumGoroutine(), leaked)
	}
}

// TestExtractJPEG_HappyPath verifies the parser splits two
// concatenated JPEGs at SOI/EOI boundaries and leaves a partial third
// frame in the buffer.
func TestExtractJPEG_HappyPath(t *testing.T) {
	a := []byte{0xFF, 0xD8, 0x01, 0x02, 0xFF, 0xD9}
	b := []byte{0xFF, 0xD8, 0x03, 0x04, 0x05, 0xFF, 0xD9}
	partial := []byte{0xFF, 0xD8, 0x06}
	buf := &bytes.Buffer{}
	buf.Write(a)
	buf.Write(b)
	buf.Write(partial)

	got1, ok := extractJPEG(buf)
	if !ok || !bytes.Equal(got1, a) {
		t.Fatalf("first frame: ok=%v got=%x want=%x", ok, got1, a)
	}
	got2, ok := extractJPEG(buf)
	if !ok || !bytes.Equal(got2, b) {
		t.Fatalf("second frame: ok=%v got=%x want=%x", ok, got2, b)
	}
	if _, ok := extractJPEG(buf); ok {
		t.Fatalf("expected no third frame yet (buffered partial); ok=true")
	}
	if !bytes.Equal(buf.Bytes(), partial) {
		t.Errorf("buffer after partial = %x, want %x", buf.Bytes(), partial)
	}
}

// TestFrameOffsetMs guards the integer-rounding behavior so a future
// refactor that swaps math.Round for a truncation cannot regress the
// offset cadence.
func TestFrameOffsetMs(t *testing.T) {
	cases := []struct {
		i    int
		fps  float64
		want int
	}{
		{0, 2, 0},
		{1, 2, 500},
		{2, 2, 1000},
		{1, 3, 333},
		{2, 3, 667},
		{0, 0, 0},
		{5, 1, 5000},
	}
	for _, c := range cases {
		got := frameOffsetMs(c.i, c.fps)
		if got != c.want {
			t.Errorf("frameOffsetMs(i=%d, fps=%g) = %d, want %d", c.i, c.fps, got, c.want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
