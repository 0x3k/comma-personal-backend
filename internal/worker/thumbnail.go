// Package worker -- thumbnail.go implements the Thumbnailer, a background
// worker that generates a small JPEG preview for each route.
//
// # State tracking
//
// The Thumbnailer records per-route state on the filesystem rather than in
// the database. Two sentinel files live in the chosen segment directory
// (typically segment 0):
//
//   - thumbnail.jpg     -- the generated preview. Its presence means
//     "success"; the worker skips any route that already
//     has it. The HTTP endpoint reads this same file.
//   - thumbnail.failed  -- a text file whose contents are an RFC3339
//     timestamp. When the previous attempt failed, the
//     worker reads this file and skips the route until
//     ThumbnailRetryAfter has elapsed. This bounds the
//     retry rate for routes whose qcamera.ts is
//     corrupted or missing frames.
//
// The on-disk approach keeps the worker self-contained: no new migration,
// no new sqlc query for a nullable timestamp, no risk of a DB row drifting
// out of sync with the actual file. The trade-off is that the scan must
// Stat() each candidate, which is cheap on modern filesystems and bounded
// by ThumbnailScanLimit.
//
// # Segment selection
//
// The worker prefers segment "0" (the first minute of the route), because
// that is usually the most representative frame of the drive. If segment 0
// does not exist on disk or does not have qcamera.ts (e.g. the device
// never uploaded that one), it falls back to the lowest-numbered segment
// that does have qcamera.ts.
package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/storage"
)

// ThumbnailFileName is the success marker: a JPEG preview of the route.
const ThumbnailFileName = "thumbnail.jpg"

// ThumbnailFailedFileName is the failure marker: a text file whose
// contents are an RFC3339 timestamp. The worker skips the route until
// ThumbnailRetryAfter has elapsed since that timestamp.
const ThumbnailFailedFileName = "thumbnail.failed"

// QcameraFileName is the input the worker extracts a frame from.
const QcameraFileName = "qcamera.ts"

// Defaults for the Thumbnailer. All are exported so tests and callers can
// override without reaching into unexported fields.
const (
	DefaultThumbnailScanInterval = 60 * time.Second
	DefaultThumbnailScanLimit    = 200
	DefaultThumbnailRetryAfter   = 30 * time.Minute
	DefaultThumbnailQueueDepth   = 100
	DefaultThumbnailConcurrency  = 1
	DefaultThumbnailWidth        = 320
)

const thumbnailWorkerName = "thumbnail"

// ffmpegRunner abstracts how the worker invokes ffmpeg so tests can stub
// the binary without relying on a shell script. The real implementation
// uses exec.CommandContext; tests can supply a closure that writes a fake
// JPEG directly.
type ffmpegRunner func(ctx context.Context, ffmpegPath, inputPath, outputPath string, width int) error

// Thumbnailer generates a 320px-wide JPEG preview for each route by
// pulling a single frame out of qcamera.ts with ffmpeg.
//
// Lifecycle follows the Transcoder pattern: New / NewWithMetrics returns a
// Thumbnailer with no goroutines running; Start launches a worker pool and
// a scanner; Stop signals cancellation and waits for the pool to drain.
type Thumbnailer struct {
	queries *db.Queries
	storage *storage.Storage
	metrics *metrics.Metrics

	ffmpegPath  string
	width       int
	concurrency int
	queueSize   int

	scanInterval time.Duration
	scanLimit    int32
	retryAfter   time.Duration

	runFFmpeg ffmpegRunner
	nowFn     func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobs   chan thumbnailJob

	// inflight tracks routes currently being processed so the scanner does
	// not requeue the same route twice if the worker pool is slow.
	inflightMu sync.Mutex
	inflight   map[string]struct{}
}

// thumbnailJob carries the identity of a route plus the segment the worker
// should pull a frame from.
type thumbnailJob struct {
	dongleID string
	route    string
	segment  string
}

// NewThumbnailer returns a Thumbnailer with defaults and no metrics.
func NewThumbnailer(q *db.Queries, s *storage.Storage) *Thumbnailer {
	return NewThumbnailerWithMetrics(q, s, nil)
}

// NewThumbnailerWithMetrics returns a Thumbnailer wired to the given
// metrics instance. A nil metrics argument is treated as no-op.
func NewThumbnailerWithMetrics(q *db.Queries, s *storage.Storage, m *metrics.Metrics) *Thumbnailer {
	t := &Thumbnailer{
		queries:      q,
		storage:      s,
		metrics:      m,
		ffmpegPath:   "ffmpeg",
		width:        DefaultThumbnailWidth,
		concurrency:  DefaultThumbnailConcurrency,
		queueSize:    DefaultThumbnailQueueDepth,
		scanInterval: DefaultThumbnailScanInterval,
		scanLimit:    DefaultThumbnailScanLimit,
		retryAfter:   DefaultThumbnailRetryAfter,
		inflight:     make(map[string]struct{}),
	}
	t.jobs = make(chan thumbnailJob, t.queueSize)
	t.runFFmpeg = defaultFFmpegRunner
	return t
}

// SetFFmpegPath overrides the ffmpeg binary path. Used by tests that stub
// ffmpeg via a shell script on disk.
func (t *Thumbnailer) SetFFmpegPath(path string) {
	t.ffmpegPath = path
}

// SetFFmpegRunner injects a custom ffmpeg runner, bypassing exec entirely.
// Intended for unit tests that want deterministic success/failure without
// shelling out.
func (t *Thumbnailer) SetFFmpegRunner(r ffmpegRunner) {
	t.runFFmpeg = r
}

// SetScanInterval overrides how often the scanner wakes up to look for
// routes needing a thumbnail.
func (t *Thumbnailer) SetScanInterval(d time.Duration) {
	if d > 0 {
		t.scanInterval = d
	}
}

// SetScanLimit overrides the number of most-recent routes considered per
// scan pass.
func (t *Thumbnailer) SetScanLimit(n int32) {
	if n > 0 {
		t.scanLimit = n
	}
}

// SetRetryAfter overrides the cooldown applied to a route after a failed
// generation attempt.
func (t *Thumbnailer) SetRetryAfter(d time.Duration) {
	if d > 0 {
		t.retryAfter = d
	}
}

// SetConcurrency controls how many generator goroutines run in parallel.
// Must be called before Start. Values < 1 are clamped to 1.
func (t *Thumbnailer) SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	t.concurrency = n
}

// SetNowFunc injects a clock for tests that need to freeze time.
func (t *Thumbnailer) SetNowFunc(fn func() time.Time) {
	t.nowFn = fn
}

// ProbeFFmpeg checks whether the configured ffmpeg binary is callable. It
// returns nil on success; any error is returned unchanged so the caller
// can decide how to log it. The worker is still safe to run when ffmpeg
// is missing -- individual jobs will fail gracefully and be marked for
// backoff like any other failure.
func (t *Thumbnailer) ProbeFFmpeg(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, t.ffmpegPath, "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg probe failed: %w", err)
	}
	return nil
}

// Start launches the scanner plus a pool of generator goroutines. If the
// Thumbnailer is already running, previous workers are stopped first.
func (t *Thumbnailer) Start(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cancel != nil {
		t.cancel()
		t.wg.Wait()
	}
	ctx, t.cancel = context.WithCancel(ctx)

	for i := 0; i < t.concurrency; i++ {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.generatorLoop(ctx)
		}()
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.scannerLoop(ctx)
	}()
}

// Stop signals the workers to shut down and waits for them. Safe to call
// multiple times and to pair with a later Start.
func (t *Thumbnailer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.wg.Wait()
}

// Enqueue submits a single route for thumbnail generation. Returns false
// if the queue is full. The caller should pass the exact segment it wants
// the frame pulled from; use SelectSegment to resolve "segment 0 or
// fallback" first if needed.
func (t *Thumbnailer) Enqueue(dongleID, route, segment string) bool {
	key := thumbnailKey(dongleID, route)
	t.inflightMu.Lock()
	if _, busy := t.inflight[key]; busy {
		t.inflightMu.Unlock()
		return false
	}
	t.inflight[key] = struct{}{}
	t.inflightMu.Unlock()

	select {
	case t.jobs <- thumbnailJob{dongleID: dongleID, route: route, segment: segment}:
		t.metrics.SetThumbnailQueueDepth(len(t.jobs))
		return true
	default:
		t.inflightMu.Lock()
		delete(t.inflight, key)
		t.inflightMu.Unlock()
		return false
	}
}

// scannerLoop wakes up every scanInterval, lists recent routes, and
// enqueues any that still need a thumbnail.
func (t *Thumbnailer) scannerLoop(ctx context.Context) {
	// Do one pass immediately so a freshly started server does not wait a
	// full interval before generating thumbnails for the backlog.
	t.scanOnce(ctx)

	ticker := time.NewTicker(t.scanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.scanOnce(ctx)
		}
	}
}

// scanOnce performs a single pass: fetch recent routes, filter out those
// already done or in cooldown, and enqueue the rest.
func (t *Thumbnailer) scanOnce(ctx context.Context) {
	if t.queries == nil {
		return
	}
	routes, err := t.queries.ListRecentRoutes(ctx, t.scanLimit)
	if err != nil {
		log.Printf("thumbnail worker: list routes failed: %v", err)
		return
	}
	for _, r := range routes {
		if ctx.Err() != nil {
			return
		}
		segment, ok := t.SelectSegment(r.DongleID, r.RouteName)
		if !ok {
			continue
		}
		if t.hasThumbnail(r.DongleID, r.RouteName, segment) {
			continue
		}
		if t.inCooldown(r.DongleID, r.RouteName, segment) {
			continue
		}
		if !t.Enqueue(r.DongleID, r.RouteName, segment) {
			// Queue is full (or job already in-flight): skip this route
			// this tick; the next pass will pick it up.
			continue
		}
	}
}

// generatorLoop drains the job channel, running one thumbnail job at a
// time on this goroutine until ctx is cancelled.
func (t *Thumbnailer) generatorLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-t.jobs:
			if !ok {
				return
			}
			t.metrics.SetThumbnailQueueDepth(len(t.jobs))
			t.process(ctx, job)
			t.inflightMu.Lock()
			delete(t.inflight, thumbnailKey(job.dongleID, job.route))
			t.inflightMu.Unlock()
		}
	}
}

// process generates a single thumbnail and records metrics / markers.
func (t *Thumbnailer) process(ctx context.Context, job thumbnailJob) {
	start := time.Now()

	if err := t.generate(ctx, job.dongleID, job.route, job.segment); err != nil {
		log.Printf("thumbnail worker: generate failed for %s/%s seg=%s: %v",
			job.dongleID, job.route, job.segment, err)
		t.markFailed(job.dongleID, job.route, job.segment)
		t.metrics.ObserveThumbnailGeneration("failure", time.Since(start))
		t.metrics.ObserveWorkerRun(thumbnailWorkerName, time.Since(start))
		return
	}
	// Success -- clear any stale failure marker so subsequent changes are
	// not held back by a previous attempt.
	t.clearFailure(job.dongleID, job.route, job.segment)
	t.metrics.ObserveThumbnailGeneration("success", time.Since(start))
	t.metrics.ObserveWorkerRun(thumbnailWorkerName, time.Since(start))
}

// generate runs ffmpeg to produce thumbnail.jpg next to qcamera.ts. It
// returns an error if the input is missing or if ffmpeg fails.
func (t *Thumbnailer) generate(ctx context.Context, dongleID, route, segment string) error {
	inputPath := t.storage.Path(dongleID, route, segment, QcameraFileName)
	if _, err := os.Stat(inputPath); err != nil {
		return fmt.Errorf("qcamera.ts not found: %w", err)
	}
	outputPath := t.storage.Path(dongleID, route, segment, ThumbnailFileName)

	// Write to a sibling temp file first, then rename, so a concurrent HTTP
	// request never sees a half-written JPEG.
	tmpPath := outputPath + ".tmp"
	if err := t.runFFmpeg(ctx, t.ffmpegPath, inputPath, tmpPath, t.width); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename thumbnail: %w", err)
	}
	return nil
}

// SelectSegment returns the segment number (as a string) that the worker
// should extract a frame from for this route. It prefers "0" but falls
// back to the lowest-numbered segment that has qcamera.ts on disk. The
// second return value is false when no usable segment exists -- the
// worker skips such routes until a segment is uploaded.
func (t *Thumbnailer) SelectSegment(dongleID, route string) (string, bool) {
	// Fast path: segment 0 with qcamera.ts.
	if t.storage.Exists(dongleID, route, "0", QcameraFileName) {
		return "0", true
	}
	segments, err := t.storage.ListSegments(dongleID, route)
	if err != nil {
		return "", false
	}
	for _, n := range segments {
		s := strconv.Itoa(n)
		if t.storage.Exists(dongleID, route, s, QcameraFileName) {
			return s, true
		}
	}
	return "", false
}

// hasThumbnail reports whether the success marker already exists for this
// route/segment pair.
func (t *Thumbnailer) hasThumbnail(dongleID, route, segment string) bool {
	return t.storage.Exists(dongleID, route, segment, ThumbnailFileName)
}

// inCooldown reports whether a previous failure marker is recent enough
// to skip this route for now.
func (t *Thumbnailer) inCooldown(dongleID, route, segment string) bool {
	path := t.storage.Path(dongleID, route, segment, ThumbnailFailedFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	ts, err := time.Parse(time.RFC3339Nano, string(data))
	if err != nil {
		// Unreadable marker -- treat as no cooldown so the next attempt
		// either succeeds and clears it or rewrites a fresh timestamp.
		return false
	}
	now := t.now()
	return now.Sub(ts) < t.retryAfter
}

// markFailed writes the failure marker with the current timestamp so the
// worker backs off before the next retry.
func (t *Thumbnailer) markFailed(dongleID, route, segment string) {
	path := t.storage.Path(dongleID, route, segment, ThumbnailFailedFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Printf("thumbnail worker: mkdir for failure marker failed: %v", err)
		return
	}
	ts := t.now().UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(path, []byte(ts), 0644); err != nil {
		log.Printf("thumbnail worker: write failure marker failed: %v", err)
	}
}

// clearFailure removes any existing failure marker. Silent on "not
// exists".
func (t *Thumbnailer) clearFailure(dongleID, route, segment string) {
	path := t.storage.Path(dongleID, route, segment, ThumbnailFailedFileName)
	_ = os.Remove(path)
}

// now returns the current time, respecting an injected clock.
func (t *Thumbnailer) now() time.Time {
	if t.nowFn != nil {
		return t.nowFn()
	}
	return time.Now()
}

// thumbnailKey returns the inflight-map key for a route.
func thumbnailKey(dongleID, route string) string {
	return dongleID + "|" + route
}

// defaultFFmpegRunner implements the real ffmpeg invocation. It runs
// `ffmpeg -ss 0 -i <input> -frames:v 1 -vf scale=<width>:-2 -q:v 4 -f mjpeg <output>`
// with a process-group-aware cancel so ctx cancellation kills the child.
func defaultFFmpegRunner(ctx context.Context, ffmpegPath, inputPath, outputPath string, width int) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}

	// -f mjpeg forces the output muxer so ffmpeg does not try to infer it
	// from the file extension. The worker writes to <output>.tmp before
	// renaming for atomicity, and ffmpeg cannot guess "jpeg" from ".tmp".
	args := []string{
		"-y",
		"-ss", "0",
		"-i", inputPath,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-q:v", "4",
		"-f", "mjpeg",
		outputPath,
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg failed: %s: %w", string(output), err)
	}
	return nil
}
