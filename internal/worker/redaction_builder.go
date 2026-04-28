// Package worker -- redaction_builder.go renders the per-segment
// redacted qcamera HLS variant the share-link handler serves to viewers
// of a token whose RedactPlates flag is true.
//
// # Lifecycle
//
// Unlike the transcoder, the redaction builder is a one-shot
// background goroutine: there is no scanner. The builder is triggered
// by the share-link media handler when it observes a redacted-share
// request for a route whose qcamera-redacted/ directory is empty. The
// handler enqueues the route via Builder.Trigger and returns 503 with
// Retry-After: 30 to the viewer; the next poll either hits the cache
// (variant ready) or stays on the 503 path (still building).
//
// # De-duplication
//
// Trigger is idempotent: a second call for the same route while the
// first build is still running is a no-op. This keeps a hot-share-link
// burst from spawning N parallel ffmpegs for the same input.
//
// # Failure semantics
//
// A build failure on a single segment leaves whatever the segment had
// before (typically nothing); the next Trigger after the failure
// retries from scratch. We do NOT mark "build failed" anywhere
// persistent because the cache is a best-effort accelerator -- a
// permanent failure is logged and re-tried on the next viewer hit.
package worker

import (
	"context"
	"errors"
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
	"comma-personal-backend/internal/redaction"
	"comma-personal-backend/internal/storage"
)

// RedactionBuilder renders cached redacted-qcamera HLS variants on
// demand. It owns its own goroutine pool and an inflight set so the
// share handler can fire-and-forget Trigger calls without worrying
// about double-builds.
type RedactionBuilder struct {
	queries     *db.Queries
	storage     *storage.Storage
	concurrency int
	ffmpegPath  string

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	jobs   chan redactionJob

	inflightMu sync.Mutex
	inflight   map[string]struct{}
}

// redactionJob is one (dongle, route) pair to redact. The whole route
// is re-rendered as a single unit; per-segment locking would let two
// triggers race partial builds.
type redactionJob struct {
	dongleID string
	route    string
}

// NewRedactionBuilder constructs a builder backed by the given queries
// + storage. concurrency controls how many parallel route-builds run
// at once; default 1 keeps CPU cost low (qcamera re-encode is cheap
// but we don't want to starve the rest of the system on a Pi).
func NewRedactionBuilder(q *db.Queries, s *storage.Storage, concurrency int) *RedactionBuilder {
	if concurrency < 1 {
		concurrency = 1
	}
	return &RedactionBuilder{
		queries:     q,
		storage:     s,
		concurrency: concurrency,
		ffmpegPath:  "ffmpeg",
		jobs:        make(chan redactionJob, 32),
		inflight:    make(map[string]struct{}),
	}
}

// SetFFmpegPath overrides the ffmpeg binary used by the builder
// (test hook).
func (b *RedactionBuilder) SetFFmpegPath(path string) { b.ffmpegPath = path }

// Start spins up the worker pool. Safe to call concurrently with Stop;
// a previous Start is replaced.
func (b *RedactionBuilder) Start(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
		b.wg.Wait()
	}
	ctx, b.cancel = context.WithCancel(ctx)
	for i := 0; i < b.concurrency; i++ {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.workerLoop(ctx)
		}()
	}
}

// Stop signals the worker pool to drain and waits for it to finish.
func (b *RedactionBuilder) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.wg.Wait()
}

// Trigger requests a build for (dongleID, route). Idempotent: a call
// for an already-queued or already-running route is a no-op. Returns
// true when the job was enqueued, false when it was deduped or the
// queue is full.
func (b *RedactionBuilder) Trigger(dongleID, route string) bool {
	key := redactionKey(dongleID, route)
	b.inflightMu.Lock()
	if _, busy := b.inflight[key]; busy {
		b.inflightMu.Unlock()
		return false
	}
	b.inflight[key] = struct{}{}
	b.inflightMu.Unlock()

	select {
	case b.jobs <- redactionJob{dongleID: dongleID, route: route}:
		return true
	default:
		b.inflightMu.Lock()
		delete(b.inflight, key)
		b.inflightMu.Unlock()
		return false
	}
}

func (b *RedactionBuilder) workerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-b.jobs:
			if !ok {
				return
			}
			if err := b.buildRoute(ctx, job.dongleID, job.route); err != nil {
				log.Printf("redaction builder: build %s/%s failed: %v",
					job.dongleID, job.route, err)
			}
			b.inflightMu.Lock()
			delete(b.inflight, redactionKey(job.dongleID, job.route))
			b.inflightMu.Unlock()
		}
	}
}

// buildRoute renders the redacted qcamera HLS variant for every
// segment of one route. Per-segment processing is sequential within a
// single route (the inner ffmpeg invocations are themselves
// multi-threaded). Segments without a qcamera.ts are skipped silently.
func (b *RedactionBuilder) buildRoute(ctx context.Context, dongleID, route string) error {
	if b.storage == nil {
		return errors.New("storage is nil")
	}
	dets, err := b.loadDetections(ctx, dongleID, route)
	if err != nil {
		return fmt.Errorf("load detections: %w", err)
	}
	// Group detections by segment so the per-segment filter only
	// references its own bboxes -- otherwise a high-detection-count
	// route would pile every detection onto every segment's filter
	// graph.
	bySegment := map[int32][]redaction.Detection{}
	for _, d := range dets {
		bySegment[d.segment] = append(bySegment[d.segment], redaction.Detection{
			TimeSec: float64(d.frameOffsetMs) / 1000.0,
			Bbox:    d.bbox,
		})
	}

	segments, err := b.storage.ListSegments(dongleID, route)
	if err != nil {
		return fmt.Errorf("list segments: %w", err)
	}
	for _, n := range segments {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		segStr := strconv.Itoa(n)
		input := b.storage.Path(dongleID, route, segStr, "qcamera.ts")
		if _, err := os.Stat(input); err != nil {
			continue
		}
		// Direct construction rather than redaction.RedactedQcameraDirPath
		// because the storage helper exposes the per-route directory,
		// which is more convenient to work from than the storage root
		// the helper expects.
		outDir := filepath.Join(b.storage.RouteDir(dongleID, route), segStr, redaction.RedactedQcameraDir)
		if err := b.buildSegment(ctx, input, outDir, bySegment[int32(n)]); err != nil {
			return fmt.Errorf("segment %d: %w", n, err)
		}
	}
	return nil
}

// buildSegment renders one redacted qcamera HLS playlist + .ts chunks.
// When detections is empty the segment is still copied through (no
// boxblur) so the cached variant is complete and the share handler
// stays on the cache-hit path. The output mirrors PackageQcamera's
// layout (index.m3u8, seg_NNN.ts, hls_time=6, vod playlist).
func (b *RedactionBuilder) buildSegment(ctx context.Context, inputPath, outDir string, detections []redaction.Detection) error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("mkdir output: %w", err)
	}
	indexPath := filepath.Join(outDir, "index.m3u8")
	segPattern := filepath.Join(outDir, "seg_%03d.ts")

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", inputPath,
	}

	filter := redaction.BuildBoxblurFilter(detections, redaction.FilterOptions{
		// Bbox coordinates were recorded against fcamera frames; the
		// builder applies them to qcamera.ts directly. The filter
		// uses normalized iw/ih expressions so the resolution
		// difference (qcamera ships at 526x330, fcamera at 1928x1208)
		// is absorbed automatically at filter-eval time -- no
		// manual scale needed here.
		InputLabel:  "0:v",
		OutputLabel: "vout",
	})
	if filter != "" {
		args = append(args,
			"-filter_complex", filter,
			"-map", "[vout]",
			"-map", "0:a?",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-crf", "23",
			"-c:a", "copy",
		)
	} else {
		// No detections in this segment: stream-copy through to
		// preserve byte parity with the unredacted variant where
		// possible. This is the cache-warmup path for routes with
		// detections in other segments only.
		args = append(args, "-c", "copy")
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", "6",
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", segPattern,
		indexPath,
	)

	cmd := exec.CommandContext(ctx, b.ffmpegPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Best-effort cleanup so a half-written playlist does not
		// fool the share handler into a cache-hit on broken output.
		_ = removeDirContents(outDir)
		return fmt.Errorf("ffmpeg: %s: %w", string(output), err)
	}
	return nil
}

// loadDetections is the builder's slimmed-down version of the export
// handler's loader: it returns a flat slice tagged with the segment
// number so buildRoute can split the per-segment filter graphs.
type builderDetection struct {
	segment       int32
	frameOffsetMs int32
	bbox          redaction.Bbox
}

func (b *RedactionBuilder) loadDetections(ctx context.Context, dongleID, route string) ([]builderDetection, error) {
	if b.queries == nil {
		return nil, nil
	}
	rows, err := b.queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
		DongleID: dongleID,
		Route:    route,
	})
	if err != nil {
		return nil, err
	}
	out := make([]builderDetection, 0, len(rows))
	for _, r := range rows {
		bbox, derr := redaction.DecodeBbox(r.Bbox)
		if derr != nil {
			continue
		}
		out = append(out, builderDetection{
			segment:       r.Segment,
			frameOffsetMs: r.FrameOffsetMs,
			bbox:          bbox,
		})
	}
	return out, nil
}

// redactionKey is the inflight-map key for a (dongle, route) pair.
func redactionKey(dongleID, route string) string {
	return dongleID + "|" + route
}

// removeDirContents removes every file under dir but keeps the
// directory itself. Used to clean up partial ffmpeg output without
// disturbing concurrent share-handler reads of the directory's
// presence (which is the cache-hit signal).
func removeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// PollUntilReady waits up to timeout for the redacted variant of one
// segment to materialize on disk. Used by tests to replace
// time-based sleeps with a deterministic wait. Returns nil when the
// playlist exists, an error otherwise.
func (b *RedactionBuilder) PollUntilReady(dongleID, route, segment string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	idx := filepath.Join(b.storage.RouteDir(dongleID, route), segment, redaction.RedactedQcameraDir, "index.m3u8")
	for time.Now().Before(deadline) {
		if _, err := os.Stat(idx); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("redacted variant for %s/%s seg %s not ready before deadline", dongleID, route, segment)
}
