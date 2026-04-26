// Package worker -- transcoder.go runs FFmpeg to convert per-segment
// camera files into HLS playlists the web UI can play.
//
// # Inputs
//
// Two distinct input shapes are handled:
//
//   - HEVC dashcam streams (fcamera.hevc, ecamera.hevc, dcamera.hevc).
//     These are H.265-in-MP4-fragments that browsers cannot play
//     natively. The worker first tries -c:v copy (just rewrap into MPEG-TS
//     for HLS), and falls back to libx264 re-encode when that fails. HEVC
//     re-encoding is expensive, so concurrency should stay low.
//
//   - qcamera.ts (low-res preview, H.264-in-MPEG-TS). This is what
//     openpilot/sunnypilot upload by default for every drive; the HEVC
//     files only land for routes the operator preserves or pulls via
//     athena. The worker stream-copies qcamera.ts straight into HLS
//     (no re-encode), which is cheap and never falls back.
//
// Output for each input is written to <segment>/<basename>/index.m3u8
// alongside seg_NNN.ts chunks. The basename is derived from the input
// filename minus its extension ("fcamera.hevc" -> "fcamera",
// "qcamera.ts" -> "qcamera") so the static segment server URL pattern
// {seg}/{camera}/index.m3u8 is preserved.
//
// # Scanner
//
// Like the thumbnail worker, the transcoder owns a small scannerLoop
// that periodically (DefaultTranscoderScanInterval) lists the most
// recent routes, walks their segments, and enqueues any that have
// qcamera.ts but no qcamera/index.m3u8 yet. The scan deliberately
// targets qcamera (not the HEVC files) because qcamera is the only
// camera we expect to be present for every uploaded route -- HEVC files
// are still picked up via the existing per-segment ProcessSegment path
// when the operator preserves a route or triggers a manual transcode.
//
// Running the scanner once at startup means a freshly-restarted server
// drains the backlog of qcamera.ts uploads accumulated during downtime
// without waiting a full interval.
package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/metrics"
	"comma-personal-backend/internal/storage"
)

// workerName is the label value used for worker_run_duration_seconds.
const workerName = "transcoder"

// QcameraFile is the low-res preview MPEG-TS uploaded by default.
const QcameraFile = "qcamera.ts"

// hevcCameraFiles lists the HEVC camera files found in each segment.
// These require the copy-then-fallback-to-libx264 path because they are
// H.265 streams browsers cannot play natively.
var hevcCameraFiles = []string{
	"fcamera.hevc",
	"ecamera.hevc",
	"dcamera.hevc",
}

// Defaults for the Transcoder scanner. Exported so tests and callers
// can override without reaching into unexported fields.
const (
	DefaultTranscoderScanInterval = 60 * time.Second
	DefaultTranscoderScanLimit    = 200
	DefaultTranscoderQueueDepth   = 100
)

// Transcoder converts per-segment camera files (HEVC or qcamera.ts) to
// HLS using FFmpeg. It can process individual files, full segments, or
// run as a background worker pool with a scanner that watches for new
// qcamera.ts uploads.
type Transcoder struct {
	queries     *db.Queries
	storage     *storage.Storage
	concurrency int
	ffmpegPath  string
	metrics     *metrics.Metrics

	scanInterval time.Duration
	scanLimit    int32

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobs   chan transcodeJob

	// inflight tracks segments currently queued or processing so the
	// scanner does not enqueue the same segment twice on consecutive
	// passes when the worker pool is slow.
	inflightMu sync.Mutex
	inflight   map[string]struct{}
}

// transcodeJob represents a single segment to be transcoded.
type transcodeJob struct {
	dongleID string
	route    string
	segment  string
}

// New creates a Transcoder backed by the given storage with no metrics
// and no DB connection (so the scanner is a no-op). Use NewWithDeps to
// wire in queries + metrics for the production worker.
func New(store *storage.Storage, concurrency int) *Transcoder {
	return NewWithDeps(nil, store, concurrency, nil)
}

// NewWithMetrics creates a Transcoder that records transcode durations
// and worker-run durations to the provided metrics instance. A nil
// metrics argument is treated as a no-op. Kept for backward
// compatibility with callers that do not need the scanner.
func NewWithMetrics(store *storage.Storage, concurrency int, m *metrics.Metrics) *Transcoder {
	return NewWithDeps(nil, store, concurrency, m)
}

// NewWithDeps returns a Transcoder wired with database queries (used
// by the scanner to discover new segments), storage, concurrency, and
// optional metrics. A nil queries disables the scanner; a nil metrics
// is a no-op.
func NewWithDeps(q *db.Queries, store *storage.Storage, concurrency int, m *metrics.Metrics) *Transcoder {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Transcoder{
		queries:      q,
		storage:      store,
		concurrency:  concurrency,
		ffmpegPath:   "ffmpeg",
		metrics:      m,
		scanInterval: DefaultTranscoderScanInterval,
		scanLimit:    DefaultTranscoderScanLimit,
		jobs:         make(chan transcodeJob, DefaultTranscoderQueueDepth),
		inflight:     make(map[string]struct{}),
	}
}

// SetFFmpegPath overrides the FFmpeg binary path (useful for testing).
func (t *Transcoder) SetFFmpegPath(path string) {
	t.ffmpegPath = path
}

// SetScanInterval overrides how often the scanner wakes up to look for
// segments needing a qcamera HLS playlist. Values <= 0 are ignored.
func (t *Transcoder) SetScanInterval(d time.Duration) {
	if d > 0 {
		t.scanInterval = d
	}
}

// SetScanLimit overrides the number of most-recent routes the scanner
// considers per pass. Values <= 0 are ignored.
func (t *Transcoder) SetScanLimit(n int32) {
	if n > 0 {
		t.scanLimit = n
	}
}

// ProbeFFmpeg checks whether the configured ffmpeg binary is callable.
// It returns nil on success; any error is returned unchanged so the
// caller can decide how to log it. The worker is still safe to run
// when ffmpeg is missing -- individual jobs will simply fail.
func (t *Transcoder) ProbeFFmpeg(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, t.ffmpegPath, "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg probe failed: %w", err)
	}
	return nil
}

// Start launches the background worker goroutines. If called while
// workers are already running, the previous workers are stopped first
// to prevent leaks. Safe to call concurrently with itself or Stop --
// the mutex is held across the full lifecycle transition so wg.Add and
// wg.Wait never race.
//
// When the Transcoder was constructed with a non-nil *db.Queries, an
// additional scanner goroutine is started alongside the worker pool.
func (t *Transcoder) Start(ctx context.Context) {
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
			t.worker(ctx)
		}()
	}
	if t.queries != nil {
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.scannerLoop(ctx)
		}()
	}
}

// Stop signals all workers to shut down and waits for them to finish.
// It is safe to call multiple times and to call Start again afterwards.
func (t *Transcoder) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	t.wg.Wait()
}

// Enqueue submits a segment for background transcoding. It returns
// false if the job channel is full or if the same segment is already
// queued / processing.
func (t *Transcoder) Enqueue(dongleID, route, segment string) bool {
	key := transcodeKey(dongleID, route, segment)
	t.inflightMu.Lock()
	if _, busy := t.inflight[key]; busy {
		t.inflightMu.Unlock()
		return false
	}
	t.inflight[key] = struct{}{}
	t.inflightMu.Unlock()

	select {
	case t.jobs <- transcodeJob{dongleID: dongleID, route: route, segment: segment}:
		return true
	default:
		t.inflightMu.Lock()
		delete(t.inflight, key)
		t.inflightMu.Unlock()
		return false
	}
}

// scannerLoop wakes up every scanInterval, lists recent routes, and
// enqueues any segment that has qcamera.ts but not yet
// qcamera/index.m3u8.
func (t *Transcoder) scannerLoop(ctx context.Context) {
	// Do one pass immediately so a freshly started server does not
	// wait a full interval before draining the backlog.
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

// scanOnce performs a single pass: fetch recent routes, walk their
// segments, and enqueue any whose qcamera.ts has not been packaged
// into HLS yet.
func (t *Transcoder) scanOnce(ctx context.Context) {
	if t.queries == nil {
		return
	}
	routes, err := t.queries.ListRecentRoutes(ctx, t.scanLimit)
	if err != nil {
		log.Printf("transcoder worker: list routes failed: %v", err)
		return
	}
	for _, r := range routes {
		if ctx.Err() != nil {
			return
		}
		segments, err := t.storage.ListSegments(r.DongleID, r.RouteName)
		if err != nil {
			continue
		}
		for _, n := range segments {
			if ctx.Err() != nil {
				return
			}
			seg := strconv.Itoa(n)
			if !t.storage.Exists(r.DongleID, r.RouteName, seg, QcameraFile) {
				continue
			}
			// Skip if qcamera HLS already exists.
			if t.storage.Exists(r.DongleID, r.RouteName, seg, filepath.Join("qcamera", "index.m3u8")) {
				continue
			}
			if !t.Enqueue(r.DongleID, r.RouteName, seg) {
				// Queue full or already in flight: skip this pass; the
				// next tick will pick the segment up.
				continue
			}
		}
	}
}

// worker drains the job channel, transcoding each segment until ctx is
// done.
func (t *Transcoder) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-t.jobs:
			if !ok {
				return
			}
			if err := t.ProcessSegment(ctx, job.dongleID, job.route, job.segment); err != nil {
				log.Printf("failed to transcode segment %s/%s/%s: %v",
					job.dongleID, job.route, job.segment, err)
			}
			t.inflightMu.Lock()
			delete(t.inflight, transcodeKey(job.dongleID, job.route, job.segment))
			t.inflightMu.Unlock()
		}
	}
}

// ProcessSegment transcodes every recognized camera input in the
// segment that does not yet have an HLS playlist. HEVC files use the
// copy-then-libx264-fallback path; qcamera.ts uses straight stream-copy
// into the HLS muxer (no re-encode, no fallback).
func (t *Transcoder) ProcessSegment(ctx context.Context, dongleID, route, segment string) error {
	start := time.Now()
	defer func() {
		t.metrics.ObserveWorkerRun(workerName, time.Since(start))
	}()

	var errs []string

	// HEVC cameras: existing copy-then-libx264-fallback path.
	for _, camera := range hevcCameraFiles {
		if !t.storage.Exists(dongleID, route, segment, camera) {
			continue
		}

		// Output directory name is the camera filename without
		// extension, e.g. "fcamera.hevc" -> "fcamera".
		base := strings.TrimSuffix(camera, filepath.Ext(camera))
		indexFile := filepath.Join(base, "index.m3u8")

		if t.storage.Exists(dongleID, route, segment, indexFile) {
			continue
		}

		inputPath := t.storage.Path(dongleID, route, segment, camera)
		outputDir := filepath.Join(filepath.Dir(inputPath), base)

		camStart := time.Now()
		err := t.TranscodeFile(ctx, inputPath, outputDir)
		camDur := time.Since(camStart)
		result := "success"
		if err != nil {
			result = "error"
			errs = append(errs, fmt.Sprintf("%s: %v", camera, err))
		}
		// Label value is the camera name without extension ("fcamera"
		// etc.) so the histogram labels match what an operator would
		// query on.
		t.metrics.ObserveTranscode(base, result, camDur)
	}

	// qcamera.ts: stream-copy into HLS, no re-encode fallback.
	if t.storage.Exists(dongleID, route, segment, QcameraFile) {
		base := strings.TrimSuffix(QcameraFile, filepath.Ext(QcameraFile))
		indexFile := filepath.Join(base, "index.m3u8")

		if !t.storage.Exists(dongleID, route, segment, indexFile) {
			inputPath := t.storage.Path(dongleID, route, segment, QcameraFile)
			outputDir := filepath.Join(filepath.Dir(inputPath), base)

			camStart := time.Now()
			err := t.PackageQcamera(ctx, inputPath, outputDir)
			camDur := time.Since(camStart)
			result := "success"
			if err != nil {
				result = "error"
				errs = append(errs, fmt.Sprintf("%s: %v", QcameraFile, err))
			}
			t.metrics.ObserveTranscode(base, result, camDur)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to transcode segment: %s", strings.Join(errs, "; "))
	}
	return nil
}

// TranscodeFile converts a single HEVC file to HLS format.
// It first tries container copy (-c:v copy) for speed, and falls back
// to re-encoding with libx264 if the copy fails.
func (t *Transcoder) TranscodeFile(ctx context.Context, inputPath, outputDir string) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	indexPath := filepath.Join(outputDir, "index.m3u8")
	segPattern := filepath.Join(outputDir, "seg_%03d.ts")

	// Try container copy first (fast, no re-encoding)
	err := t.runFFmpegHEVC(ctx, inputPath, indexPath, segPattern, "copy")
	if err == nil {
		return nil
	}

	// Clean up partial output from failed copy attempt
	t.cleanDir(outputDir)

	// Fall back to re-encoding with libx264
	if err := t.runFFmpegHEVC(ctx, inputPath, indexPath, segPattern, "libx264"); err != nil {
		return fmt.Errorf("failed to transcode (re-encode fallback): %w", err)
	}
	return nil
}

// PackageQcamera rewraps qcamera.ts (already H.264 in MPEG-TS) into an
// HLS playlist using stream-copy. There is no re-encode fallback: if
// the copy fails the input is unusable and a re-encode would not help.
func (t *Transcoder) PackageQcamera(ctx context.Context, inputPath, outputDir string) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	indexPath := filepath.Join(outputDir, "index.m3u8")
	segPattern := filepath.Join(outputDir, "seg_%03d.ts")

	args := []string{
		"-y",
		"-i", inputPath,
		"-c", "copy",
		"-f", "hls",
		"-hls_time", "6",
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", segPattern,
		indexPath,
	}

	if err := t.runFFmpeg(ctx, args); err != nil {
		// Best-effort cleanup so a half-written playlist does not get
		// served on the next request.
		t.cleanDir(outputDir)
		return fmt.Errorf("failed to package qcamera: %w", err)
	}
	return nil
}

// runFFmpegHEVC executes the FFmpeg command with the given video codec
// for HEVC -> HLS conversion (copy or libx264).
func (t *Transcoder) runFFmpegHEVC(ctx context.Context, inputPath, indexPath, segPattern, vcodec string) error {
	args := []string{
		"-i", inputPath,
		"-c:v", vcodec,
		"-hls_time", "6",
		"-hls_list_size", "0",
		"-hls_segment_filename", segPattern,
		indexPath,
	}
	if err := t.runFFmpeg(ctx, args); err != nil {
		return fmt.Errorf("ffmpeg (codec=%s): %w", vcodec, err)
	}
	return nil
}

// runFFmpeg executes the FFmpeg binary with the given argv. The child
// runs in its own process group so ctx cancellation kills the whole
// process tree (avoids zombie ffmpegs after a worker shutdown).
func (t *Transcoder) runFFmpeg(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, t.ffmpegPath, args...)
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
		return fmt.Errorf("%s: %w", string(output), err)
	}
	return nil
}

// cleanDir removes all files in a directory but keeps the directory itself.
func (t *Transcoder) cleanDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// transcodeKey returns the inflight-map key for a segment.
func transcodeKey(dongleID, route, segment string) string {
	return dongleID + "|" + route + "|" + segment
}
