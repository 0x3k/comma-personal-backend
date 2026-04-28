// Package worker -- alpr_backfill.go is the singleton goroutine that
// drives the ALPR historical-backfill state machine. It reads job rows
// from alpr_backfill_jobs, walks each row's filtered route set in
// chronological order, and for each route enqueues every fcamera
// segment onto the same channel the live extractor uses (deps.alprFrames)
// so the detector + aggregator pipeline handles backfill and live ingest
// identically.
//
// # Lifecycle
//
// startup
//
//	On process boot, look up any state='running' row. If found, resume
//	the loop from last_processed_route; if not, idle and wait for a
//	start-handler Wake().
//
// running
//
//	Re-read the job's state at every route boundary so the cooperative
//	pause/cancel signals (state set to 'paused' or 'failed' by the API
//	handlers, or 'paused' by the worker itself when alpr_enabled flips
//	off) are honored within one route. For each route in the filter
//	set, compute the segment list, throttle each frame through the
//	token bucket, lower-priority-defer to the live extractor when
//	deps.alprFrames is non-empty, run ffmpeg + send frames, then mark
//	the segment processed and bump the job's progress counter.
//
// terminal
//
//	When the route iterator exhausts (or max_routes is hit) without
//	any pause signal, transition to state='done' and stamp finished_at.
//	On a hard error the state moves to 'failed' with the error text.
//
// # Throttling and priority
//
//   - Token bucket: time.NewTicker(time.Second / fpsBudget). Each
//     enqueue waits on a tick, so the backfill cannot exceed fpsBudget
//     frames per second on the engine queue regardless of what the
//     extractor is doing.
//   - Soft live priority: before each enqueue, if len(deps.alprFrames)
//     is non-zero, sleep ~100ms and re-check. This is a soft signal
//     (not a hard guarantee), but it keeps backfill stalled while the
//     live extractor is producing frames; in steady state with no
//     uploads the channel sits at 0 and the backfill runs at full
//     throttle.
//
// # Crash resumability
//
// last_processed_route is stamped after a route's segments are all
// enqueued (or skipped for being already complete). On restart, the
// worker re-runs CountBackfillRoutes for the original filter set so
// total_routes stays stable, then iterates routes WHERE route_name >
// last_processed_route. This means a server crash mid-route re-runs
// that route -- which is safe because alpr_segment_progress.processed
// is the idempotency guard at the per-segment level (the live
// extractor's existing pattern, reused here).
//
// # Settings flag
//
// alpr_enabled is consulted at every route boundary. Flipping it to
// false transitions the job to 'paused', preserves last_processed_route,
// and lets the user resume from the same point when they re-enable.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/storage"
)

// Defaults for the ALPR backfill worker.
const (
	// DefaultALPRBackfillFPSBudget is the per-engine frame budget the
	// backfill is allowed to consume. 0.5 means half a frame per
	// second (one frame every 2s) so the live extractor at 2 fps can
	// always reach the engine first. Override via
	// ALPR_BACKFILL_FPS_BUDGET; values <=0 are coerced to the default.
	DefaultALPRBackfillFPSBudget = 0.5

	// DefaultALPRBackfillRouteBatch is how many routes the worker
	// pulls from the database per iteration of the main loop. Small
	// enough that the worker re-reads the job state frequently
	// enough to honour pause/cancel without overhead, large enough
	// that the per-batch query cost amortizes well across many routes.
	DefaultALPRBackfillRouteBatch = 50

	// DefaultALPRBackfillIdlePoll is how long the worker sleeps when
	// it has no running job to process before re-checking the table.
	// Wake() shortens this in practice; the timer is a safety net so
	// a missed Wake (e.g. nil trigger after a manual SQL INSERT) is
	// still picked up within a poll interval.
	DefaultALPRBackfillIdlePoll = 30 * time.Second

	// DefaultALPRBackfillLivePriorityDelay is how long the worker
	// pauses before re-checking deps.alprFrames when the live queue
	// is non-empty. 100ms is the spec-recommended value: short
	// enough that the backfill resumes quickly when live ingest
	// drains, long enough that the worker isn't busy-looping on the
	// channel length.
	DefaultALPRBackfillLivePriorityDelay = 100 * time.Millisecond

	// alprBackfillWorkerName is the metrics label used by
	// ObserveWorkerRun for runs of the backfill worker's outer loop.
	alprBackfillWorkerName = "alpr_backfill"
)

// ALPRBackfillQuerier is the slice of *db.Queries the worker needs.
// Carved out as an interface so tests can pass an in-memory fake
// without a real Postgres connection. The production wiring passes
// *db.Queries directly.
type ALPRBackfillQuerier interface {
	GetBackfillJob(ctx context.Context, id int64) (db.AlprBackfillJob, error)
	GetRunningBackfillJob(ctx context.Context) (db.AlprBackfillJob, error)
	UpdateBackfillJobState(ctx context.Context, arg db.UpdateBackfillJobStateParams) error
	IncrementBackfillJobProgress(ctx context.Context, arg db.IncrementBackfillJobProgressParams) error
	ListBackfillRoutesAsc(ctx context.Context, arg db.ListBackfillRoutesAscParams) ([]db.Route, error)
	ListBackfillRoutesDesc(ctx context.Context, arg db.ListBackfillRoutesDescParams) ([]db.Route, error)
	IsExtractorProcessed(ctx context.Context, arg db.IsExtractorProcessedParams) (bool, error)
	MarkExtractorProcessed(ctx context.Context, arg db.MarkExtractorProcessedParams) error
	CountRouteDetectorProgress(ctx context.Context, arg db.CountRouteDetectorProgressParams) (db.CountRouteDetectorProgressRow, error)
}

// Compile-time assertion that *db.Queries satisfies the worker contract.
var _ ALPRBackfillQuerier = (*db.Queries)(nil)

// alprBackfillFilters mirrors the API-level ALPRBackfillFilters struct.
// The worker decodes filters_json into this shape on every pause/
// resume so the live HTTP type can evolve independently of the worker's
// understanding (e.g. an API-only field that the worker should ignore).
type alprBackfillFilters struct {
	FromDate    *time.Time `json:"from_date,omitempty"`
	ToDate      *time.Time `json:"to_date,omitempty"`
	DongleID    string     `json:"dongle_id,omitempty"`
	MaxRoutes   int        `json:"max_routes,omitempty"`
	NewestFirst bool       `json:"newest_first,omitempty"`
}

// ALPRBackfillFrameSink is the small interface the worker uses to
// produce frames. The production wiring passes a thin adapter that
// writes to deps.alprFrames; tests substitute a fake that records
// pushes for assertion. queueDepth lets the worker honour the soft
// "live extractor takes priority" rule without exposing the channel
// directly.
type ALPRBackfillFrameSink interface {
	Push(ctx context.Context, frame ExtractedFrame) error
	QueueDepth() int
}

// ChannelFrameSink is a simple ALPRBackfillFrameSink backed by a
// chan ExtractedFrame -- the same channel the live extractor writes
// into. Production wiring constructs this against deps.alprFrames so
// the backfill worker shares the engine queue.
type ChannelFrameSink struct {
	Frames chan<- ExtractedFrame
	// Depth, when non-nil, is consulted by QueueDepth to read the
	// channel length. We accept a separate read-side reference because
	// chan<- doesn't expose len() and the production wiring already
	// has a bidirectional reference handy.
	Depth func() int
}

// Push sends the frame onto the underlying channel, blocking until
// either there's room or ctx is cancelled.
func (c *ChannelFrameSink) Push(ctx context.Context, frame ExtractedFrame) error {
	if c == nil || c.Frames == nil {
		return errors.New("alpr backfill sink: channel not configured")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.Frames <- frame:
		return nil
	}
}

// QueueDepth returns the current channel length so the worker can
// honour the "live extractor first" soft priority rule.
func (c *ChannelFrameSink) QueueDepth() int {
	if c == nil || c.Depth == nil {
		return 0
	}
	return c.Depth()
}

// ALPRBackfillMetrics is the subset of *metrics.Metrics this worker
// uses. Same pattern as the extractor: a nil implementation degrades
// to logs only, and tests can pass a fake without standing up a
// Prometheus registry.
type ALPRBackfillMetrics interface {
	ObserveWorkerRun(worker string, d time.Duration)
}

// ALPRBackfill is the singleton worker. Construct via NewALPRBackfill;
// start with Run.
type ALPRBackfill struct {
	// Queries is the small ALPRBackfillQuerier interface (rather than
	// *db.Queries) so tests can swap in a fake without standing up a
	// Postgres connection.
	Queries ALPRBackfillQuerier

	// Storage resolves on-disk paths for fcamera.hevc and is used to
	// list segments per route.
	Storage *storage.Storage

	// Settings is the runtime tunables store. Used to read
	// alpr_enabled at every route boundary. May be nil; in that case
	// the worker treats alpr_enabled as false (matching the
	// extractor's safe default for an opt-in feature).
	Settings *settings.Store

	// Sink is the producer interface for the engine queue. Construct
	// from a chan ExtractedFrame in production wiring or a fake in
	// tests.
	Sink ALPRBackfillFrameSink

	// FPSBudget is the throttling budget in frames per second. Each
	// pushed frame waits on a tick of (1 second / FPSBudget) so the
	// backfill cannot exceed this rate regardless of upstream/
	// downstream behaviour.
	FPSBudget float64

	// LivePriorityDelay is how long to sleep before re-checking the
	// queue depth when the live extractor is producing frames. Used
	// only when Sink reports a non-zero queue.
	LivePriorityDelay time.Duration

	// IdlePoll is the safety-net poll interval when the worker has
	// no running job. The Wake channel shortens this in practice.
	IdlePoll time.Duration

	// RouteBatch is how many routes the worker fetches per database
	// query. Small batches keep the worker responsive to pause/
	// cancel signals.
	RouteBatch int32

	// FFmpegPath, FramesPerSecond, StderrLogEvery are passed through
	// to the embedded extractor that does the actual frame
	// production. We re-use ALPRExtractor's per-segment ffmpeg
	// pipeline rather than duplicating it -- it already handles
	// process-group cleanup, MJPEG parsing, stderr tailing, and
	// metrics; the only difference for backfill is who pushes onto
	// the sink and at what rate.
	FFmpegPath             string
	DefaultFramesPerSecond float64

	// Metrics receives outer-loop observations. Per-segment metrics
	// are emitted by the embedded extractor.
	Metrics ALPRBackfillMetrics

	// wakeCh is the buffered wakeup signal the API start/resume
	// handlers send when a new job is inserted. Buffered to 1 so a
	// double-wake collapses (we just need "look at the table").
	wakeCh chan struct{}
	// wakeOnce guards lazy construction of wakeCh in Wake().
	wakeOnce sync.Once
}

// NewALPRBackfill wires defaults but does not start the worker. Caller
// must populate Queries / Storage / Sink; everything else has a sensible
// default. Wake() is allowed before Run() -- the wake channel is
// constructed lazily on first use.
func NewALPRBackfill(q ALPRBackfillQuerier, store *storage.Storage, settingsStore *settings.Store, sink ALPRBackfillFrameSink, m ALPRBackfillMetrics) *ALPRBackfill {
	return &ALPRBackfill{
		Queries:                q,
		Storage:                store,
		Settings:               settingsStore,
		Sink:                   sink,
		FPSBudget:              DefaultALPRBackfillFPSBudget,
		LivePriorityDelay:      DefaultALPRBackfillLivePriorityDelay,
		IdlePoll:               DefaultALPRBackfillIdlePoll,
		RouteBatch:             DefaultALPRBackfillRouteBatch,
		FFmpegPath:             "ffmpeg",
		DefaultFramesPerSecond: DefaultALPRExtractorFramesPerSecond,
		Metrics:                m,
	}
}

// Wake nudges the worker to re-check the table immediately rather than
// wait for the next IdlePoll. Safe to call from multiple goroutines;
// the channel is buffered to 1 so concurrent calls collapse.
func (b *ALPRBackfill) Wake() {
	b.ensureWake()
	select {
	case b.wakeCh <- struct{}{}:
	default:
	}
}

// ensureWake constructs wakeCh on first use. Done lazily so callers
// (the API start handler) can hold a *ALPRBackfill reference and call
// Wake before Run is invoked, e.g. when the operator manually inserts
// a job row before the server has finished starting the worker.
func (b *ALPRBackfill) ensureWake() {
	b.wakeOnce.Do(func() {
		if b.wakeCh == nil {
			b.wakeCh = make(chan struct{}, 1)
		}
	})
}

// Run drives the worker until ctx is cancelled. The contract:
//   - On entry: look for any state='running' job and resume it.
//   - On a finished/failed/paused job: idle until Wake() or IdlePoll.
//   - On ctx.Done: return promptly. The current state is left in the
//     DB so a fresh process can resume from last_processed_route.
//
// Run is intended to be invoked as `go b.Run(ctx)` exactly once per
// process.
func (b *ALPRBackfill) Run(ctx context.Context) {
	b.ensureWake()
	if b.Queries == nil || b.Storage == nil || b.Sink == nil {
		log.Printf("alpr backfill: queries/storage/sink not configured; worker will idle")
		<-ctx.Done()
		return
	}

	idle := b.IdlePoll
	if idle <= 0 {
		idle = DefaultALPRBackfillIdlePoll
	}

	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		b.runOnce(ctx)
		if b.Metrics != nil {
			b.Metrics.ObserveWorkerRun(alprBackfillWorkerName, time.Since(start))
		}
		// Wait for either a wakeup or the idle poll. ctx.Done takes
		// precedence so a shutdown does not get stuck waiting on the
		// idle ticker.
		select {
		case <-ctx.Done():
			return
		case <-b.wakeCh:
		case <-time.After(idle):
		}
	}
}

// runOnce processes the current running job (if any) until it
// finishes or pauses. Returns on the first pause/cancel/done so the
// outer loop can re-check (e.g. for a fresh start that happened
// concurrently). Returns silently if no running job exists.
func (b *ALPRBackfill) runOnce(ctx context.Context) {
	job, err := b.Queries.GetRunningBackfillJob(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		log.Printf("alpr backfill: read running job: %v", err)
		return
	}
	b.processJob(ctx, job)
}

// processJob walks the route set for `job` until completion or a state
// transition (pause/cancel/done). Each route is processed inside a
// helper that handles the per-segment ffmpeg + sink + bookkeeping.
//
// Filter parsing happens inside the loop (re-parsed from filters_json
// on every iteration via getJobState) so a column edit (e.g. an
// admin updating filters_json mid-pause via SQL) is honoured on
// resume.
func (b *ALPRBackfill) processJob(ctx context.Context, initial db.AlprBackfillJob) {
	jobID := initial.ID

	// alpr_enabled is the secondary gate. If the user disabled ALPR
	// after starting a backfill, we transition to 'paused' so the
	// resume button restarts from the same route. The check happens
	// at the top of every route boundary, matching the live
	// extractor's pattern.
	if !b.alprEnabled(ctx) {
		b.setState(ctx, jobID, "paused", "", time.Time{})
		return
	}

	for {
		if ctx.Err() != nil {
			return
		}
		// Re-read the job row so external state changes (pause /
		// cancel via API; alpr_enabled flip; filters_json edit) are
		// observed at every route boundary.
		fresh, err := b.Queries.GetBackfillJob(ctx, jobID)
		if err != nil {
			log.Printf("alpr backfill: re-read job %d: %v", jobID, err)
			return
		}
		if fresh.State != "running" {
			return
		}
		if !b.alprEnabled(ctx) {
			b.setState(ctx, jobID, "paused", "", time.Time{})
			return
		}

		filters, err := decodeFilters(fresh.FiltersJson)
		if err != nil {
			log.Printf("alpr backfill: decode filters for job %d: %v", jobID, err)
			b.setState(ctx, jobID, "failed", fmt.Sprintf("decode filters: %v", err), time.Now().UTC())
			return
		}

		afterRoute := ""
		if fresh.LastProcessedRoute.Valid {
			afterRoute = fresh.LastProcessedRoute.String
		}

		// MaxRoutes acts as a hard ceiling on processed_routes. Once
		// reached, we transition the job to 'done' and stop.
		if filters.MaxRoutes > 0 && int(fresh.ProcessedRoutes) >= filters.MaxRoutes {
			b.setState(ctx, jobID, "done", "", time.Now().UTC())
			return
		}

		batch, err := b.fetchRouteBatch(ctx, filters, afterRoute)
		if err != nil {
			log.Printf("alpr backfill: list routes for job %d: %v", jobID, err)
			b.setState(ctx, jobID, "failed", fmt.Sprintf("list routes: %v", err), time.Now().UTC())
			return
		}
		if len(batch) == 0 {
			// No more routes to process: terminal 'done' state.
			b.setState(ctx, jobID, "done", "", time.Now().UTC())
			return
		}

		for _, route := range batch {
			if ctx.Err() != nil {
				return
			}
			// Mid-batch state recheck: pause/cancel that happens
			// between routes is honoured without waiting for the
			// outer loop to rerun.
			cur, err := b.Queries.GetBackfillJob(ctx, jobID)
			if err != nil {
				log.Printf("alpr backfill: mid-batch re-read job %d: %v", jobID, err)
				return
			}
			if cur.State != "running" {
				return
			}
			if !b.alprEnabled(ctx) {
				b.setState(ctx, jobID, "paused", "", time.Time{})
				return
			}
			if filters.MaxRoutes > 0 && int(cur.ProcessedRoutes) >= filters.MaxRoutes {
				b.setState(ctx, jobID, "done", "", time.Now().UTC())
				return
			}

			b.processRoute(ctx, jobID, route)

			if err := b.Queries.IncrementBackfillJobProgress(ctx, db.IncrementBackfillJobProgressParams{
				ID:                 jobID,
				LastProcessedRoute: pgtype.Text{String: route.RouteName, Valid: true},
			}); err != nil {
				log.Printf("alpr backfill: increment progress for job %d (%s): %v", jobID, route.RouteName, err)
				// Continue: the segment progress rows are the
				// authoritative idempotency layer. Worst case we
				// re-iterate the same route on resume.
			}
		}
	}
}

// fetchRouteBatch returns the next batch of routes to process,
// respecting the newest_first knob. Limit is RouteBatch; the worker
// re-queries with the new last_processed_route after each route, so
// the actual batch size is "as many as fit in the limit before we
// step out".
func (b *ALPRBackfill) fetchRouteBatch(ctx context.Context, f alprBackfillFilters, afterRoute string) ([]db.Route, error) {
	limit := b.RouteBatch
	if limit <= 0 {
		limit = DefaultALPRBackfillRouteBatch
	}
	if f.NewestFirst {
		params := db.ListBackfillRoutesDescParams{Limit: limit}
		applyFilters(&params.FromDate, &params.ToDate, &params.DongleID, &params.AfterRoute, f, afterRoute)
		return b.Queries.ListBackfillRoutesDesc(ctx, params)
	}
	params := db.ListBackfillRoutesAscParams{Limit: limit}
	applyFilters(&params.FromDate, &params.ToDate, &params.DongleID, &params.AfterRoute, f, afterRoute)
	return b.Queries.ListBackfillRoutesAsc(ctx, params)
}

// applyFilters copies filter fields onto the sqlc params struct. We
// take pointers so the same helper works for the asc and desc query
// variants without reflection.
func applyFilters(fromDate, toDate *pgtype.Timestamptz, dongleID, afterRoute *pgtype.Text, f alprBackfillFilters, after string) {
	if f.FromDate != nil {
		*fromDate = pgtype.Timestamptz{Time: *f.FromDate, Valid: true}
	}
	if f.ToDate != nil {
		*toDate = pgtype.Timestamptz{Time: *f.ToDate, Valid: true}
	}
	if f.DongleID != "" {
		*dongleID = pgtype.Text{String: f.DongleID, Valid: true}
	}
	if after != "" {
		*afterRoute = pgtype.Text{String: after, Valid: true}
	}
}

// processRoute walks a single route's fcamera segments, throttling
// each frame onto the engine queue. Skips segments already marked
// processed_at_extractor (idempotency).
//
// Errors are logged but do NOT abort the job: a single bad segment
// (corrupt fcamera.hevc, ffmpeg crash) should not stop the user's
// backfill of the rest of the route set. The segment progress row is
// only stamped on a clean run, so the next pass will retry the
// failure.
func (b *ALPRBackfill) processRoute(ctx context.Context, jobID int64, route db.Route) {
	segments, err := b.Storage.ListSegments(route.DongleID, route.RouteName)
	if err != nil {
		// No on-disk segments is normal for some routes (e.g. an
		// upload-failed row). Log at debug level and move on.
		log.Printf("alpr backfill: list segments %s/%s: %v", route.DongleID, route.RouteName, err)
		return
	}
	for _, segNum := range segments {
		if ctx.Err() != nil {
			return
		}
		// Cooperative pause check at the segment boundary too. A
		// route with many segments otherwise wouldn't honour pause
		// for minutes.
		cur, err := b.Queries.GetBackfillJob(ctx, jobID)
		if err == nil && cur.State != "running" {
			return
		}
		segName := strconv.Itoa(segNum)
		if !b.Storage.Exists(route.DongleID, route.RouteName, segName, FCameraFile) {
			continue
		}
		processed, err := b.Queries.IsExtractorProcessed(ctx, db.IsExtractorProcessedParams{
			DongleID: route.DongleID,
			Route:    route.RouteName,
			Segment:  int32(segNum),
		})
		if err != nil {
			log.Printf("alpr backfill: IsExtractorProcessed %s/%s/%d: %v",
				route.DongleID, route.RouteName, segNum, err)
			continue
		}
		if processed {
			continue
		}
		if err := b.processSegment(ctx, route, segNum); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			log.Printf("alpr backfill: process segment %s/%s/%d: %v",
				route.DongleID, route.RouteName, segNum, err)
			continue
		}
		if err := b.Queries.MarkExtractorProcessed(ctx, db.MarkExtractorProcessedParams{
			DongleID:             route.DongleID,
			Route:                route.RouteName,
			Segment:              int32(segNum),
			ProcessedAtExtractor: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		}); err != nil {
			log.Printf("alpr backfill: mark extractor processed %s/%s/%d: %v",
				route.DongleID, route.RouteName, segNum, err)
		}
	}
}

// processSegment runs ffmpeg on one fcamera.hevc segment and pushes
// the resulting JPEG frames through the throttling token bucket onto
// the sink. Reuses the live extractor's MJPEG pipeline so the
// pixel-level behaviour is identical for backfill and live ingest.
func (b *ALPRBackfill) processSegment(ctx context.Context, route db.Route, segNum int) error {
	// Build a tiny extractor that re-uses the well-tested ffmpeg
	// pipeline from alpr_extractor.go. We don't run its outer scanner
	// loop -- only the per-segment processSegment via runFFmpeg's
	// underlying parser. To do that without exporting more surface
	// from the extractor, we wire up a temporary extractor instance
	// pointed at a private channel and forward its frames through our
	// throttle.
	//
	// This is a deliberately small reach into the extractor package's
	// internals: ALPRExtractor.runFFmpeg is unexported, so we use the
	// public path via a synthetic extractor + channel. The throttle +
	// live-priority happen in pushFrame below.
	pipe := make(chan ExtractedFrame, 1)
	ext := &ALPRExtractor{
		Storage:                b.Storage,
		FFmpegPath:             b.FFmpegPath,
		DefaultFramesPerSecond: b.DefaultFramesPerSecond,
		Frames:                 pipe,
		stderrLogEvery:         30 * time.Second,
		// Note: Queries / Settings / Metrics intentionally nil --
		// the worker handles per-segment progress and we don't want
		// the extractor double-marking.
	}

	job := extractorJob{
		dongleID: route.DongleID,
		route:    route.RouteName,
		segment:  segNum,
	}
	inputPath := b.Storage.Path(route.DongleID, route.RouteName, strconv.Itoa(segNum), FCameraFile)
	if inputPath == "" {
		return errors.New("storage path resolution returned empty")
	}
	fps := b.DefaultFramesPerSecond
	if fps <= 0 {
		fps = DefaultALPRExtractorFramesPerSecond
	}

	// Run ffmpeg in a separate goroutine so we can range over the
	// pipe channel and apply the throttle on the main goroutine.
	// Closing the pipe on goroutine exit lets the range loop
	// terminate cleanly.
	errCh := make(chan error, 1)
	go func() {
		defer close(pipe)
		_, err := ext.runFFmpeg(ctx, job, inputPath, fps)
		errCh <- err
	}()

	for frame := range pipe {
		if err := b.pushFrame(ctx, frame); err != nil {
			// Ctx cancellation: drain remaining frames so the
			// extractor's pushFrame doesn't deadlock on a closed
			// receiver, then return the error.
			go func() {
				for range pipe {
				}
			}()
			return err
		}
	}
	return <-errCh
}

// pushFrame applies the throttle (token bucket of 1/FPSBudget) and
// the soft live-priority rule, then forwards the frame onto the
// downstream sink. Returns ctx.Err() on cancellation.
func (b *ALPRBackfill) pushFrame(ctx context.Context, frame ExtractedFrame) error {
	// Soft live priority: if the live extractor is producing frames
	// (sink queue length > 0), step aside until it drains. This is a
	// loop because a busy live extractor can refill the queue between
	// our checks.
	for b.Sink.QueueDepth() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b.livePriorityDelay()):
		}
	}

	// Token bucket throttle: wait one tick of (1s / fpsBudget) before
	// each enqueue. fpsBudget < 1 means a slower-than-1Hz tick, e.g.
	// 0.5 fps -> 2s between frames.
	wait := b.tokenBucketWait()
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}

	return b.Sink.Push(ctx, frame)
}

// tokenBucketWait converts FPSBudget into a per-frame sleep
// duration. fpsBudget <= 0 falls back to the default; values >= 1000
// are coerced to a 1ms minimum to avoid tight loops.
func (b *ALPRBackfill) tokenBucketWait() time.Duration {
	fps := b.FPSBudget
	if fps <= 0 {
		fps = DefaultALPRBackfillFPSBudget
	}
	wait := time.Duration(float64(time.Second) / fps)
	if wait < time.Millisecond {
		wait = time.Millisecond
	}
	return wait
}

// livePriorityDelay returns the configured re-check delay or the
// default if unset. Exposed as a method so tests can force a faster
// poll.
func (b *ALPRBackfill) livePriorityDelay() time.Duration {
	if b.LivePriorityDelay > 0 {
		return b.LivePriorityDelay
	}
	return DefaultALPRBackfillLivePriorityDelay
}

// alprEnabled reads the runtime master flag. A missing settings store
// or an unreadable row both fall back to false: matches the
// extractor's safe default for an opt-in feature. Pause-on-disable
// is the response: the caller transitions the job to 'paused' so the
// user can resume from the same route.
func (b *ALPRBackfill) alprEnabled(ctx context.Context) bool {
	if alprEnabledForTest != nil {
		return *alprEnabledForTest
	}
	if b.Settings == nil {
		return false
	}
	v, err := b.Settings.BoolOr(ctx, settings.KeyALPREnabled, false)
	if err != nil {
		log.Printf("alpr backfill: read alpr_enabled: %v", err)
		return false
	}
	return v
}

// setState transitions the job to the given state with optional
// finished_at and error text. A zero time.Time on finishedAt leaves
// the column unchanged. An empty errText leaves error unchanged.
func (b *ALPRBackfill) setState(ctx context.Context, jobID int64, state, errText string, finishedAt time.Time) {
	params := db.UpdateBackfillJobStateParams{
		ID:    jobID,
		State: state,
	}
	if !finishedAt.IsZero() {
		params.FinishedAt = pgtype.Timestamptz{Time: finishedAt, Valid: true}
	}
	if errText != "" {
		params.ErrorText = pgtype.Text{String: errText, Valid: true}
	}
	if err := b.Queries.UpdateBackfillJobState(ctx, params); err != nil {
		log.Printf("alpr backfill: update state job=%d state=%s: %v", jobID, state, err)
	}
}

// decodeFilters parses the filters_json blob. An empty blob (e.g. a
// pre-existing row written before the worker shipped) is treated as
// "no filters" rather than an error.
func decodeFilters(raw []byte) (alprBackfillFilters, error) {
	var f alprBackfillFilters
	if len(raw) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	return f, nil
}

// EnvFloatALPRBackfillFPSBudget reads ALPR_BACKFILL_FPS_BUDGET, falling
// back to DefaultALPRBackfillFPSBudget on absent or malformed values.
// Exported so cmd/server/workers.go can use it without duplicating the
// parsing logic.
func EnvFloatALPRBackfillFPSBudget() float64 {
	raw := os.Getenv("ALPR_BACKFILL_FPS_BUDGET")
	if raw == "" {
		return DefaultALPRBackfillFPSBudget
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		log.Printf("warning: ALPR_BACKFILL_FPS_BUDGET=%q is not a valid positive float; using default %g", raw, DefaultALPRBackfillFPSBudget)
		return DefaultALPRBackfillFPSBudget
	}
	return v
}
