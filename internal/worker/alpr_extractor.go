// Package worker -- alpr_extractor.go is the producer half of the ALPR
// pipeline. It samples JPEG frames from each fcamera segment at the
// configured FPS and emits them on a buffered channel that the (separate)
// alpr-detection-worker consumes.
//
// # Lifecycle
//
// The extractor is a poll-based worker, modelled on event_detector.go and
// transcoder.go: it wakes up every PollInterval, lists recent routes via
// the same query the transcoder scanner uses (ListRecentRoutes), and walks
// each route's segments looking for fcamera.hevc files whose
// alpr_segment_progress.processed_at_extractor row is still NULL. Per
// segment it spawns ffmpeg, streams MJPEG frames on stdout, parses each
// SOI/EOI-bounded JPEG out of the stream, computes a millisecond offset
// (frame_index * 1000 / fps) relative to segment start, and pushes a
// frame onto Frames. On clean ffmpeg exit the segment's
// processed_at_extractor row is set so subsequent passes skip it.
//
// # Toggle responsiveness
//
// The runtime alpr_enabled flag (settings table, key alpr_enabled) is
// consulted at the top of every poll AND between segments inside a single
// poll, so a flip-to-false stops new work within ~PollInterval at worst
// and within one segment in practice. Choice: poll-based, not channel-
// notified, to keep the worker independent of the settings package's
// internal API and to match the existing pattern in event_detector and
// transcoder which also re-read their gating predicates each iteration.
//
// # Backpressure
//
// Frames is a small buffered channel (default cap 32, env
// ALPR_EXTRACTOR_BUFFER). When the detector is slow the channel fills,
// the extractor's send blocks, the ffmpeg child's stdout buffer fills,
// and ffmpeg pauses producing more frames. This is the intended
// memory-bound: at any moment we hold at most BUFFER+CONCURRENCY frames
// in flight regardless of how far behind the detector falls.
//
// # Failure semantics
//
// A non-zero ffmpeg exit on a single segment is logged with a stderr
// tail and we move on to the next segment WITHOUT marking it processed,
// so the next pass retries it. The worker itself never crashes on a bad
// segment. On context cancellation the ffmpeg child is killed (SIGKILL
// of the process group) and the output channel is closed before Run
// returns.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
)

// Compile-time check: a real *metrics.Metrics satisfies our local
// ALPRExtractorMetrics interface. We don't import the package directly
// here (the worker only depends on the interface) but the wiring layer
// in cmd/server passes *metrics.Metrics in, so this guard catches
// signature drift at build time without an import cycle.
//
// var _ ALPRExtractorMetrics = (*metrics.Metrics)(nil)
//
// Kept commented because tests exercise the interface via fakes and
// the production wiring is checked by the cmd/server package's own
// build.

// FCameraFile is the front-camera HEVC filename uploaded by openpilot
// devices. The extractor only consumes the front camera; the e/d cameras
// are not useful for plate recognition.
const FCameraFile = "fcamera.hevc"

// Defaults for the ALPR extractor. Exported so tests and the worker
// wiring layer can reach in without reflecting on unexported fields.
const (
	// DefaultALPRExtractorPollInterval is how often the extractor wakes
	// up to scan for new fcamera segments. Tuned to match the
	// transcoder's scanner cadence so a fresh upload reaches both
	// pipelines on roughly the same beat.
	DefaultALPRExtractorPollInterval = 60 * time.Second

	// DefaultALPRExtractorScanLimit caps the number of recent routes the
	// scanner inspects per pass. Same reasoning as the transcoder: bounds
	// the amount of work per tick on a busy database.
	DefaultALPRExtractorScanLimit = 200

	// DefaultALPRExtractorFramesPerSecond is the fallback sampling rate
	// when neither the settings table nor the config struct supply one.
	// Matches config.alprDefaults.FramesPerSecond.
	DefaultALPRExtractorFramesPerSecond = 2.0

	// DefaultALPRExtractorBuffer is the default capacity of the
	// extractor->detector channel. Override via ALPR_EXTRACTOR_BUFFER.
	DefaultALPRExtractorBuffer = 32

	// alprExtractorWorkerName is the metrics label used by
	// ObserveWorkerRun for runs of the extractor's outer poll.
	alprExtractorWorkerName = "alpr_extractor"

	// stderrTailBytes caps how much ffmpeg stderr we hold in memory per
	// segment for the failure log line. ffmpeg can be chatty about
	// container-format quirks; we only keep the tail so a malformed
	// stream does not explode worker memory.
	stderrTailBytes = 4 * 1024
)

// JPEG SOI/EOI markers. We parse the MJPEG bytestream by hand because
// `image2pipe` writes a raw concatenation of complete JPEG files with
// no length prefix, so the only reliable boundary is SOI -> EOI.
var (
	jpegSOI = []byte{0xFF, 0xD8}
	jpegEOI = []byte{0xFF, 0xD9}
)

// ExtractedFrame is the unit of work the extractor produces and the
// detector consumes. JPEG holds the raw image bytes (as written by
// ffmpeg's mjpeg encoder) and is owned by the receiver after it lands
// on the channel; the extractor does NOT retain a reference. FrameOffsetMs
// is the millisecond offset from the start of the segment.
type ExtractedFrame struct {
	DongleID      string
	Route         string
	Segment       int
	FrameOffsetMs int
	JPEG          []byte
}

// ALPRExtractorMetrics is the subset of *metrics.Metrics this worker
// uses. Carved out as an interface so tests can pass nil (a nil
// *metrics.Metrics is a no-op already) or a fake without spinning up a
// Prometheus registry. Mirrors the TurnDetectorMetrics pattern.
type ALPRExtractorMetrics interface {
	IncALPRFrameExtracted(result string)
	AddALPRFramesExtracted(result string, n int)
	ObserveALPRExtractorSegment(d time.Duration)
	SetALPRExtractorQueueDepth(n int)
	ObserveWorkerRun(worker string, d time.Duration)
}

// ALPRExtractor is the long-running fcamera->JPEG worker. Construct via
// NewALPRExtractor; start with Run.
type ALPRExtractor struct {
	// Queries provides idempotency markers via alpr_segment_progress and
	// route discovery via ListRecentRoutes. Required.
	Queries *db.Queries

	// Storage resolves on-disk paths for fcamera.hevc and is used to
	// list segments per route. Required.
	Storage *storage.Storage

	// Settings is the runtime tunables store. Used to read
	// alpr_enabled and alpr_frames_per_second. May be nil, in which
	// case the extractor treats alpr_enabled as false (the safe
	// default for a deployment that has not opted in to ALPR) and uses
	// DefaultFramesPerSecond instead of querying.
	Settings *settings.Store

	// Frames is the buffered channel onto which the extractor pushes
	// produced frames. The detection worker (separate goroutine, later
	// wave) is the consumer. The extractor closes Frames on graceful
	// shutdown (ctx.Done) so the detector's range loop terminates.
	Frames chan ExtractedFrame

	// Concurrency controls how many segments are processed in parallel.
	// 1 is the right default for CPU-only ALPR engines; on GPU the
	// operator can bump it via ALPR_EXTRACTOR_CONCURRENCY. Values < 1
	// are coerced to 1.
	Concurrency int

	// PollInterval is how long to sleep between scanner passes.
	// Defaults to DefaultALPRExtractorPollInterval when zero.
	PollInterval time.Duration

	// ScanLimit caps the number of recent routes the scanner inspects
	// per pass. Defaults to DefaultALPRExtractorScanLimit when zero.
	ScanLimit int32

	// DefaultFramesPerSecond is the fallback FPS used when the
	// settings table does not have an alpr_frames_per_second row.
	// The env-var-derived value from config.ALPRConfig is plumbed in
	// here at startup so a re-deploy is not required for the seeded
	// row to take effect.
	DefaultFramesPerSecond float64

	// FFmpegPath is the binary to invoke. Defaults to "ffmpeg".
	FFmpegPath string

	// Metrics receives per-segment / per-frame observations. Safe to
	// leave nil; degrades to logs only.
	Metrics ALPRExtractorMetrics

	// stderrLogEvery throttles ffmpeg stderr noise: only one warning
	// every stderrLogEvery is emitted per worker. Default 30s.
	stderrLogEvery time.Duration

	// inflight de-dupes segments that overlap between scanner passes
	// (a slow segment can outlive its own poll interval).
	inflightMu sync.Mutex
	inflight   map[string]struct{}
}

// NewALPRExtractor wires defaults but does not start the worker. Caller
// must populate Queries / Storage; everything else has a sensible
// default.
func NewALPRExtractor(q *db.Queries, store *storage.Storage, settingsStore *settings.Store, frames chan ExtractedFrame, m ALPRExtractorMetrics) *ALPRExtractor {
	return &ALPRExtractor{
		Queries:                q,
		Storage:                store,
		Settings:               settingsStore,
		Frames:                 frames,
		Concurrency:            1,
		PollInterval:           DefaultALPRExtractorPollInterval,
		ScanLimit:              DefaultALPRExtractorScanLimit,
		DefaultFramesPerSecond: DefaultALPRExtractorFramesPerSecond,
		FFmpegPath:             "ffmpeg",
		Metrics:                m,
		stderrLogEvery:         30 * time.Second,
		inflight:               make(map[string]struct{}),
	}
}

// Run drives the worker until ctx is cancelled. The contract:
//
//   - On entry, runs one pass immediately so a freshly started server
//     drains the backlog without waiting a full PollInterval.
//   - Between passes, sleeps PollInterval. The flag is re-checked at
//     the top of every pass and again before each segment.
//   - On ctx.Done, the in-flight ffmpeg child is killed (process group
//     SIGKILL via exec.Cmd.Cancel), Frames is closed, and Run returns
//     after all worker goroutines are joined.
//
// Run is intended to be invoked as `go ext.Run(ctx)` exactly once per
// process. Calling it twice on the same struct will produce undefined
// behaviour around the inflight map and the Frames close.
func (e *ALPRExtractor) Run(ctx context.Context) {
	defer func() {
		if e.Frames != nil {
			close(e.Frames)
		}
	}()

	if e.Queries == nil || e.Storage == nil {
		log.Printf("alpr extractor: queries or storage not configured; worker will idle")
		<-ctx.Done()
		return
	}

	concurrency := e.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	poll := e.PollInterval
	if poll <= 0 {
		poll = DefaultALPRExtractorPollInterval
	}

	// jobs feeds a small inner worker pool so we can run N segments in
	// parallel without spawning a goroutine per segment. The capacity
	// matches concurrency so a slow worker never lets the scanner run
	// arbitrarily ahead of producers.
	jobs := make(chan extractorJob, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.workerLoop(ctx, jobs)
		}()
	}

	// First pass runs immediately. Subsequent passes wait the full
	// poll interval.
	e.scanOnce(ctx, jobs)

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case <-t.C:
			e.scanOnce(ctx, jobs)
		}
	}
}

// extractorJob bundles a single segment for the worker pool.
type extractorJob struct {
	dongleID string
	route    string
	segment  int
}

// scanOnce performs one scanner pass: fetch recent routes, walk their
// segments, and enqueue any that have fcamera.hevc on disk and a NULL
// processed_at_extractor row. Errors on individual routes are logged
// and the loop continues.
func (e *ALPRExtractor) scanOnce(ctx context.Context, jobs chan<- extractorJob) {
	start := time.Now()
	defer func() {
		if e.Metrics != nil {
			e.Metrics.ObserveWorkerRun(alprExtractorWorkerName, time.Since(start))
		}
	}()

	if !e.alprEnabled(ctx) {
		return
	}

	routes, err := e.Queries.ListRecentRoutes(ctx, e.scanLimit())
	if err != nil {
		log.Printf("alpr extractor: list recent routes: %v", err)
		return
	}
	for _, r := range routes {
		if ctx.Err() != nil {
			return
		}
		// Re-check the flag mid-pass so a flip-to-false during a long
		// scan stops queuing new segments within ~one route.
		if !e.alprEnabled(ctx) {
			return
		}
		segments, err := e.Storage.ListSegments(r.DongleID, r.RouteName)
		if err != nil {
			continue
		}
		for _, n := range segments {
			if ctx.Err() != nil {
				return
			}
			if !e.segmentNeedsExtract(ctx, r.DongleID, r.RouteName, n) {
				continue
			}
			// Block until a worker slot is free or ctx is cancelled.
			// Backpressure naturally throttles the scanner without
			// dropping segments.
			select {
			case <-ctx.Done():
				return
			case jobs <- extractorJob{dongleID: r.DongleID, route: r.RouteName, segment: n}:
			}
		}
	}
}

func (e *ALPRExtractor) scanLimit() int32 {
	if e.ScanLimit > 0 {
		return e.ScanLimit
	}
	return DefaultALPRExtractorScanLimit
}

// alprEnabled reads the runtime master flag. A missing settings store
// or an unreadable row both fall back to false: the conservative choice
// for a feature the operator must explicitly opt in to.
func (e *ALPRExtractor) alprEnabled(ctx context.Context) bool {
	if e.Settings == nil {
		return false
	}
	v, err := e.Settings.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		log.Printf("alpr extractor: read alpr_enabled: %v", err)
		return false
	}
	return v
}

// segmentNeedsExtract reports whether a segment should be enqueued.
// Skip when fcamera.hevc is missing on disk OR the segment is already
// marked processed_at_extractor in the database OR another worker has
// already claimed it via the inflight map.
func (e *ALPRExtractor) segmentNeedsExtract(ctx context.Context, dongleID, route string, segment int) bool {
	segName := strconv.Itoa(segment)
	if !e.Storage.Exists(dongleID, route, segName, FCameraFile) {
		return false
	}
	if e.Queries != nil {
		processed, err := e.Queries.IsExtractorProcessed(ctx, db.IsExtractorProcessedParams{
			DongleID: dongleID,
			Route:    route,
			Segment:  int32(segment),
		})
		if err != nil {
			log.Printf("alpr extractor: IsExtractorProcessed(%s/%s/%d): %v", dongleID, route, segment, err)
			return false
		}
		if processed {
			return false
		}
	}
	key := alprExtractorKey(dongleID, route, segment)
	e.inflightMu.Lock()
	defer e.inflightMu.Unlock()
	if _, busy := e.inflight[key]; busy {
		return false
	}
	e.inflight[key] = struct{}{}
	return true
}

// workerLoop drains the job channel until the channel is closed (clean
// shutdown) or ctx is cancelled. Per-segment errors do not terminate
// the loop.
func (e *ALPRExtractor) workerLoop(ctx context.Context, jobs <-chan extractorJob) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-jobs:
			if !ok {
				return
			}
			e.processSegment(ctx, job)
			e.inflightMu.Lock()
			delete(e.inflight, alprExtractorKey(job.dongleID, job.route, job.segment))
			e.inflightMu.Unlock()
		}
	}
}

// processSegment runs ffmpeg on one fcamera.hevc, streams JPEG frames
// onto Frames, and on clean exit marks processed_at_extractor. On
// failure (ffmpeg non-zero, parse error, ctx cancel) it logs and
// returns without marking, so the next scanner pass retries the
// segment. The function handles its own metrics; callers do not need
// to time it.
func (e *ALPRExtractor) processSegment(ctx context.Context, job extractorJob) {
	// Mid-job toggle: if the operator flipped alpr_enabled off
	// between scan and dispatch, drop the job before paying the ffmpeg
	// startup cost.
	if !e.alprEnabled(ctx) {
		return
	}

	start := time.Now()
	defer func() {
		if e.Metrics != nil {
			e.Metrics.ObserveALPRExtractorSegment(time.Since(start))
		}
	}()

	fps := e.framesPerSecond(ctx)
	segName := strconv.Itoa(job.segment)
	inputPath := e.Storage.Path(job.dongleID, job.route, segName, FCameraFile)
	if inputPath == "" {
		return
	}
	// Defensive: the path resolver returns the joined string regardless
	// of disk presence; re-check existence so a race with cleanup
	// doesn't try to spawn ffmpeg on a missing file.
	if !e.Storage.Exists(job.dongleID, job.route, segName, FCameraFile) {
		return
	}

	count, err := e.runFFmpeg(ctx, job, inputPath, fps)
	if err != nil {
		// Cancellation is not an error; just fall through without
		// marking processed.
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		log.Printf("alpr extractor: %s/%s/%d: %v", job.dongleID, job.route, job.segment, err)
		return
	}
	if e.Metrics != nil {
		e.Metrics.AddALPRFramesExtracted("ok", count)
	}

	if e.Queries != nil {
		now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
		if err := e.Queries.MarkExtractorProcessed(ctx, db.MarkExtractorProcessedParams{
			DongleID:             job.dongleID,
			Route:                job.route,
			Segment:              int32(job.segment),
			ProcessedAtExtractor: now,
		}); err != nil {
			log.Printf("alpr extractor: mark processed %s/%s/%d: %v",
				job.dongleID, job.route, job.segment, err)
		}
	}
}

// framesPerSecond reads the runtime fps tunable, falling back to the
// struct default. Out-of-range values (<= 0 or absurdly large) are
// clamped here so a misconfigured row cannot bring down the worker.
func (e *ALPRExtractor) framesPerSecond(ctx context.Context) float64 {
	def := e.DefaultFramesPerSecond
	if def <= 0 {
		def = DefaultALPRExtractorFramesPerSecond
	}
	if e.Settings == nil {
		return def
	}
	v, err := e.Settings.FloatOr(ctx, settings.KeyALPRFramesPerSecond, def)
	if err != nil {
		log.Printf("alpr extractor: read alpr_frames_per_second: %v", err)
		return def
	}
	if v <= 0 || v > 30 {
		return def
	}
	return v
}

// runFFmpeg spawns the ffmpeg child, parses MJPEG frames out of its
// stdout, and pushes each onto Frames. Returns the number of frames
// pushed and any error. ffmpeg's stderr is read concurrently so a
// chatty stream cannot deadlock the pipeline.
func (e *ALPRExtractor) runFFmpeg(ctx context.Context, job extractorJob, inputPath string, fps float64) (int, error) {
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
		"-i", inputPath,
		"-vf", fmt.Sprintf("fps=%g", fps),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-",
	}

	cmd := exec.CommandContext(ctx, e.FFmpegPath, args...)
	// Run ffmpeg in its own process group so ctx cancellation kills
	// the whole tree (no zombies after worker shutdown). Same pattern
	// used in transcoder.go.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start ffmpeg: %w", err)
	}

	// Drain stderr concurrently with a tail buffer + rate-limited log.
	// The buffer holds the most recent stderrTailBytes for the failure
	// log; the rate limit keeps recurring warnings from spamming the
	// process log on long segments with persistent quirks.
	stderrTail := newStderrTail(stderrTailBytes)
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		e.consumeStderr(stderr, stderrTail, job)
	}()

	count, parseErr := e.parseMJPEGStream(ctx, stdout, job, fps)
	// Always wait for ffmpeg to exit so we can join on stderr and
	// surface the exit code. Stop reading stdout so ffmpeg can see EOF
	// downstream.
	waitErr := cmd.Wait()
	stderrWG.Wait()

	if parseErr != nil {
		// Parse-side errors take precedence: they capture cancellation
		// and channel-close races.
		return count, parseErr
	}
	if waitErr != nil {
		// Distinguish ctx cancel from genuine ffmpeg failure for the
		// caller, but keep the failure log informative either way.
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		tail := stderrTail.String()
		if tail == "" {
			return count, fmt.Errorf("ffmpeg: %w", waitErr)
		}
		return count, fmt.Errorf("ffmpeg: %w; stderr: %s", waitErr, tail)
	}
	return count, nil
}

// parseMJPEGStream reads ffmpeg's stdout, splits it into JPEG frames at
// SOI/EOI markers, and pushes each frame onto Frames with the
// computed millisecond offset. The function blocks if Frames is full;
// that is the intended backpressure (see package doc).
func (e *ALPRExtractor) parseMJPEGStream(ctx context.Context, stdout io.Reader, job extractorJob, fps float64) (int, error) {
	r := bufio.NewReaderSize(stdout, 64*1024)
	var pending bytes.Buffer
	count := 0

	// Pull bytes in chunks. We do not assume a fixed chunk size; the
	// JPEG parser tolerates partial reads at any boundary because it
	// scans on the contents of `pending` after each append.
	chunk := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		n, err := r.Read(chunk)
		if n > 0 {
			pending.Write(chunk[:n])
			for {
				frame, ok := extractJPEG(&pending)
				if !ok {
					break
				}
				offsetMs := frameOffsetMs(count, fps)
				if pushErr := e.pushFrame(ctx, ExtractedFrame{
					DongleID:      job.dongleID,
					Route:         job.route,
					Segment:       job.segment,
					FrameOffsetMs: offsetMs,
					JPEG:          frame,
				}); pushErr != nil {
					return count, pushErr
				}
				count++
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return count, nil
			}
			return count, fmt.Errorf("read ffmpeg stdout: %w", err)
		}
	}
}

// pushFrame sends one frame onto the output channel and refreshes the
// queue-depth gauge. Blocks until the channel has room or ctx is
// cancelled. A nil Frames channel is treated as a no-op so tests that
// don't care about the frames don't have to drain.
func (e *ALPRExtractor) pushFrame(ctx context.Context, f ExtractedFrame) error {
	if e.Frames == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case e.Frames <- f:
		if e.Metrics != nil {
			e.Metrics.SetALPRExtractorQueueDepth(len(e.Frames))
		}
		return nil
	}
}

// frameOffsetMs computes round(frameIndex * 1000 / fps). Centred at
// each frame's nominal sample point: at 2 fps the first frame is 0ms,
// the second is 500ms, etc. ffmpeg's `-vf fps=N` filter emits the
// frame whose presentation timestamp is closest to each 1/N tick, so
// this offset is what the detector should correlate against the GPS
// timeline.
func frameOffsetMs(frameIndex int, fps float64) int {
	if fps <= 0 {
		return 0
	}
	return int(float64(frameIndex)*1000.0/fps + 0.5)
}

// extractJPEG removes one complete JPEG (from the next SOI to its
// matching EOI) from buf and returns the bytes. Returns ok=false when
// no complete JPEG is yet present. Bytes before the first SOI are
// discarded -- ffmpeg's mjpeg encoder does not emit any framing or
// preamble between concatenated frames, but warnings printed to stdout
// (mis-config) would otherwise poison the parser.
func extractJPEG(buf *bytes.Buffer) ([]byte, bool) {
	data := buf.Bytes()
	soi := bytes.Index(data, jpegSOI)
	if soi < 0 {
		return nil, false
	}
	// Search for EOI starting after SOI+2 so the SOI itself never
	// matches as part of a longer marker.
	eoi := bytes.Index(data[soi+2:], jpegEOI)
	if eoi < 0 {
		// Discard bytes before SOI to bound memory growth on a
		// malformed stream that never closes a frame.
		if soi > 0 {
			next := buf.Bytes()[soi:]
			buf.Reset()
			buf.Write(next)
		}
		return nil, false
	}
	end := soi + 2 + eoi + 2 // include both bytes of EOI
	frame := make([]byte, end-soi)
	copy(frame, data[soi:end])
	// Advance the buffer past the consumed frame.
	rest := append([]byte(nil), data[end:]...)
	buf.Reset()
	buf.Write(rest)
	return frame, true
}

// consumeStderr drains the ffmpeg stderr pipe into the rate-limited
// logger and into the tail buffer. It returns when the pipe closes
// (ffmpeg exits) so the caller can join on cmd.Wait().
func (e *ALPRExtractor) consumeStderr(stderr io.Reader, tail *stderrTail, job extractorJob) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 32*1024), 1<<20)
	var lastLog atomic.Int64
	logEvery := e.stderrLogEvery
	if logEvery <= 0 {
		logEvery = 30 * time.Second
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		tail.Append(line)
		now := time.Now().UnixNano()
		prev := lastLog.Load()
		if prev == 0 || time.Duration(now-prev) >= logEvery {
			if lastLog.CompareAndSwap(prev, now) {
				log.Printf("alpr extractor: ffmpeg stderr (%s/%s/%d): %s",
					job.dongleID, job.route, job.segment, line)
			}
		}
	}
	// scanner.Err() == nil on EOF / clean close. A non-nil error here
	// is logged but does not bubble up: the scanner loop ending means
	// ffmpeg has stopped producing stderr, which the caller will
	// observe via cmd.Wait().
	if err := scanner.Err(); err != nil {
		log.Printf("alpr extractor: ffmpeg stderr scan: %v", err)
	}
}

// stderrTail is a tiny ring-style buffer that retains the most recent
// N bytes written to it. Used to attach a contextual snippet to the
// error log when ffmpeg fails. Appends are line-oriented because that
// is what the consumer scans, but the buffer itself is byte-counted.
type stderrTail struct {
	mu    sync.Mutex
	cap   int
	bytes []byte
}

func newStderrTail(cap int) *stderrTail {
	return &stderrTail{cap: cap}
}

// Append adds line + "\n" to the buffer, trimming the oldest bytes
// when the cap is exceeded. Safe for concurrent use because the
// stderr-reader goroutine and the wait-side caller may both touch it.
func (t *stderrTail) Append(line []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bytes = append(t.bytes, line...)
	t.bytes = append(t.bytes, '\n')
	if len(t.bytes) > t.cap {
		t.bytes = append([]byte(nil), t.bytes[len(t.bytes)-t.cap:]...)
	}
}

func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.bytes)
}

// alprExtractorKey is the inflight-map key for a segment. Mirrors the
// transcoder's transcodeKey but with its own namespace because the
// extractor's inflight set is unrelated to the transcoder's.
func alprExtractorKey(dongleID, route string, segment int) string {
	return dongleID + "|" + route + "|" + filepath.Clean(strconv.Itoa(segment))
}
