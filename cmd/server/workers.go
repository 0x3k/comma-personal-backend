package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"comma-personal-backend/internal/api"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/geocode"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/worker"
	"comma-personal-backend/internal/ws"
)

// envFloat parses a float environment variable. Local helper because
// envInt and envBool live in env.go but float wasn't needed before the
// turn-detector tunables landed.
func envFloat(name string, defaultValue float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return defaultValue
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("warning: %s=%q is not a valid float; using default %g", name, v, defaultValue)
		return defaultValue
	}
	return f
}

// startWorkers launches the background goroutines that make up the
// server's non-HTTP side: event detection, trip aggregation, and the
// retention cleanup sweep. Each worker reads its own enablement env var
// so operators can disable one without touching the code, and each runs
// until ctx is cancelled.
//
// New background jobs belong here, grouped with their siblings -- NOT in
// main.go. Keeping them in one file makes it easy to reason about the
// set of long-running goroutines and keeps parallel feature branches
// from always colliding on the bootstrap.
func startWorkers(ctx context.Context, d *deps) {
	// Event detector background worker. Opt-out via
	// EVENT_DETECTOR_ENABLED=false (or "0"); any other value enables it.
	if envBool("EVENT_DETECTOR_ENABLED", true) {
		detector := worker.NewEventDetector(
			d.queries,
			d.store,
			30*time.Second,
			worker.LoadThresholdsFromEnv(),
		)
		go detector.Run(ctx)
		log.Printf("event detector worker started (thresholds: brake=%.2f m/s^2, min-sec=%.2f)",
			detector.Thresholds.HardBrakeMps2, detector.Thresholds.HardBrakeMinDurationSec)
	} else {
		log.Printf("event detector worker disabled via EVENT_DETECTOR_ENABLED")
	}

	// Route metadata worker: parses uploaded qlogs and backfills the
	// routes table's start_time / end_time / geometry columns. MUST run
	// before the trip aggregator -- the aggregator reads those columns
	// off routes and writes NULL stats on the trip row when they are
	// missing. ROUTE_METADATA_ENABLED defaults to true; set to false to
	// skip (e.g. when a sibling replica already owns the workload).
	if envBool("ROUTE_METADATA_ENABLED", true) {
		metaWorker := worker.NewRouteMetadataWorker(d.queries, d.store)
		go metaWorker.Run(ctx)
		log.Printf("route metadata worker started (poll=%s, finalized_after=%s)",
			metaWorker.PollInterval, metaWorker.FinalizedAfter)
	} else {
		log.Printf("route metadata worker disabled via ROUTE_METADATA_ENABLED")
	}

	// Background trip aggregator. Defaults on; set TRIP_AGGREGATOR_ENABLED=0
	// (or false/no/off) to skip it, e.g. in constrained test environments.
	// Depends on the route metadata worker above to populate start_time /
	// end_time / geometry; without that, every trip row would be all-NULL.
	if envBool("TRIP_AGGREGATOR_ENABLED", true) {
		aggregator := worker.NewTripAggregator(d.queries, geocode.NewClient("", ""))
		go aggregator.Run(ctx)
		log.Printf("trip aggregator started (poll=%s, finalized_after=%s)",
			aggregator.PollInterval, aggregator.FinalizedAfter)
	} else {
		log.Printf("trip aggregator disabled via TRIP_AGGREGATOR_ENABLED")
	}

	// Turn detector: emits per-route turn timelines from existing GPS
	// geometry. Always-on (not gated on ALPR -- turns are independently
	// useful for analytics, smart playback, and the ALPR stalking
	// heuristic). Subscribes to the same "route finalized" signal as
	// the trip aggregator above; runs a one-shot first-deploy backfill
	// on top so freshly installed servers don't have to wait for new
	// uploads to populate turn data for routes already in the database.
	turnWindow := envFloat("TURN_WINDOW_SECONDS", 4.0)
	turnDelta := envFloat("TURN_DELTA_DEG_MIN", 35.0)
	turnDedup := envFloat("TURN_DEDUP_SECONDS", 5.0)
	turnBackfillLimit := envInt("TURN_BACKFILL_LIMIT", 200)
	if d.settings != nil {
		seedCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := d.settings.SeedFloatIfMissing(seedCtx, settings.KeyTurnWindowSeconds, turnWindow); err != nil {
			log.Printf("warning: seed turn_window_seconds: %v", err)
		}
		if err := d.settings.SeedFloatIfMissing(seedCtx, settings.KeyTurnDeltaDegMin, turnDelta); err != nil {
			log.Printf("warning: seed turn_delta_deg_min: %v", err)
		}
		if err := d.settings.SeedFloatIfMissing(seedCtx, settings.KeyTurnDedupSeconds, turnDedup); err != nil {
			log.Printf("warning: seed turn_dedup_seconds: %v", err)
		}
		cancel()
	}
	turnDetector := worker.NewTurnDetectorWorker(d.queries, d.pool, d.settings, d.metrics)
	turnDetector.DefaultWindowSeconds = turnWindow
	turnDetector.DefaultDeltaDegMin = turnDelta
	turnDetector.DefaultDedupSeconds = turnDedup
	turnDetector.BackfillLimit = turnBackfillLimit
	go turnDetector.Run(ctx)
	log.Printf("turn detector worker started (window=%.1fs, delta>=%.1fdeg, dedup=%.1fs, backfill_limit=%d)",
		turnWindow, turnDelta, turnDedup, turnBackfillLimit)

	// Cleanup worker: deletes non-preserved routes older than the
	// configured retention window. CLEANUP_ENABLED defaults to true.
	// DELETE_DRY_RUN defaults to true so first-time operators see what
	// would happen before enabling real deletion.
	if envBool("CLEANUP_ENABLED", true) {
		cleanup := &worker.CleanupWorker{
			Queries:          d.queries,
			Storage:          d.store,
			Settings:         d.settings,
			Interval:         worker.DefaultCleanupInterval,
			EnvRetentionDays: d.cfg.RetentionDays,
			DryRun:           envBool("DELETE_DRY_RUN", true),
		}
		go cleanup.Run(ctx)
	} else {
		log.Printf("cleanup worker: disabled via CLEANUP_ENABLED=false")
	}

	// Thumbnail worker: extracts a single JPEG frame per route so the
	// dashboard route list can show a preview. THUMBNAIL_ENABLED defaults
	// to true; set to false to skip the worker entirely (useful when
	// ffmpeg is not installed or a second replica already owns it).
	if envBool("THUMBNAIL_ENABLED", true) {
		thumb := worker.NewThumbnailerWithMetrics(d.queries, d.store, d.metrics)
		if err := thumb.ProbeFFmpeg(ctx); err != nil {
			log.Printf("thumbnail worker: ffmpeg probe failed (%v); worker will run but every job will fail until ffmpeg is installed", err)
		}
		thumb.Start(ctx)
		log.Printf("thumbnail worker started")
	} else {
		log.Printf("thumbnail worker: disabled via THUMBNAIL_ENABLED=false")
	}

	// Route data request dispatcher: retries pending on-demand pulls when
	// the target device reconnects. ROUTE_DATA_DISPATCHER_ENABLED is
	// opt-IN by default-false so an operator who hasn't set up the
	// public-URL plumbing won't accidentally enqueue requests against a
	// localhost URL the device cannot reach. ROUTE_DATA_PUBLIC_URL is the
	// origin (e.g. https://comma.example.com) the device will PUT files
	// back to; empty falls back to a path-only URL the device resolves
	// against its own server origin (works when the device shares an
	// origin with the API, e.g. behind a single Caddy/Nginx).
	if envBool("ROUTE_DATA_DISPATCHER_ENABLED", false) {
		baseURL := os.Getenv("ROUTE_DATA_PUBLIC_URL")
		dispatcher := &worker.HubBackedDispatcher{
			Hub: d.hub,
			RPC: d.rpcCaller,
			BuildItems: func(ctx context.Context, row db.ListPendingRouteDataRequestsRow) ([]ws.UploadFileToUrlParams, error) {
				wanted, err := api.FilesForKind(row.Kind)
				if err != nil {
					return nil, err
				}
				segments, err := d.queries.ListSegmentsByRoute(ctx, row.RouteID)
				if err != nil {
					return nil, err
				}
				return api.BuildUploadItemsAt(baseURL, d.sessionSecret, row.DongleID, row.RouteName, segments, wanted), nil
			},
		}
		w := worker.NewRouteDataRequestDispatcher(d.queries, dispatcher)
		go w.Run(ctx)
		log.Printf("route data dispatcher started (poll=%s, max_attempts=%d, public_url=%q)",
			w.PollInterval, w.MaxAttempts, baseURL)
	} else {
		log.Printf("route data dispatcher: disabled via ROUTE_DATA_DISPATCHER_ENABLED")
	}

	// Transcoder worker: rewraps qcamera.ts (and re-encodes HEVC where
	// available) into HLS playlists the web UI can play. The scanner
	// only watches qcamera.ts because HEVC files are not uploaded by
	// default; HEVC re-encoding still runs on demand via Enqueue when
	// an operator triggers it. TRANSCODER_ENABLED defaults to true;
	// TRANSCODER_CONCURRENCY defaults to 1 because qcamera packaging
	// is cheap and a parallel HEVC re-encode would compete with it.
	if envBool("TRANSCODER_ENABLED", true) {
		concurrency := envInt("TRANSCODER_CONCURRENCY", 1)
		tr := worker.NewWithDeps(d.queries, d.store, concurrency, d.metrics)
		if err := tr.ProbeFFmpeg(ctx); err != nil {
			log.Printf("transcoder worker: ffmpeg probe failed (%v); worker will run but every job will fail until ffmpeg is installed", err)
		}
		tr.Start(ctx)
		log.Printf("transcoder worker started (concurrency=%d, scan_interval=%s)",
			concurrency, worker.DefaultTranscoderScanInterval)
	} else {
		log.Printf("transcoder worker: disabled via TRANSCODER_ENABLED=false")
	}

	// ALPR frame extractor: producer half of the ALPR pipeline. Always
	// started so the runtime alpr_enabled flag controls behaviour
	// without a process restart; the worker self-gates internally and
	// remains effectively idle when ALPR is off (one DB read per
	// PollInterval). The output channel is allocated here -- not in
	// the worker -- so the future detection worker can read from the
	// same instance via deps. Channel capacity is ALPR_EXTRACTOR_BUFFER
	// (default 32). Concurrency is ALPR_EXTRACTOR_CONCURRENCY (default
	// 1) but is also seeded from cfg.ALPR.ExtractorConcurrency so the
	// existing config plumbing stays the source of truth.
	bufCap := envInt("ALPR_EXTRACTOR_BUFFER", worker.DefaultALPRExtractorBuffer)
	d.alprFrames = make(chan worker.ExtractedFrame, bufCap)
	// Concurrency precedence: explicit env var > ALPRConfig (which is
	// itself env-derived from ALPR_EXTRACTOR_CONCURRENCY) > 1.
	alprConcurrency := 1
	if d.cfg != nil && d.cfg.ALPR != nil && d.cfg.ALPR.ExtractorConcurrency > 0 {
		alprConcurrency = d.cfg.ALPR.ExtractorConcurrency
	}
	alprConcurrency = envInt("ALPR_EXTRACTOR_CONCURRENCY", alprConcurrency)
	defaultFPS := worker.DefaultALPRExtractorFramesPerSecond
	if d.cfg != nil && d.cfg.ALPR != nil && d.cfg.ALPR.FramesPerSecond > 0 {
		defaultFPS = d.cfg.ALPR.FramesPerSecond
	}
	extractor := worker.NewALPRExtractor(d.queries, d.store, d.settings, d.alprFrames, d.metrics)
	extractor.Concurrency = alprConcurrency
	extractor.DefaultFramesPerSecond = defaultFPS
	go extractor.Run(ctx)
	log.Printf("alpr extractor started (concurrency=%d, fps_default=%g, buffer=%d, poll=%s)",
		alprConcurrency, defaultFPS, bufCap, extractor.PollInterval)

	// ALPR detection worker: consumer half of the pipeline. Started
	// AFTER the extractor so d.alprFrames is already constructed.
	// The detector self-gates on the runtime alpr_enabled flag and
	// idles cleanly when ALPR_ENCRYPTION_KEY is unconfigured (so a
	// deployment that has not opted into ALPR sees no log noise other
	// than a single startup line).
	//
	// Channel sizing rationale: the completion channel is buffered to
	// 64 so a slow encounter aggregator does not stall the detection
	// loop. 64 routes' worth of completion events is comfortably more
	// than any plausible burst, and the detector's drop-on-full path
	// is a soft fallback (the aggregator's own periodic scan will
	// pick up missed routes).
	detectorConcurrency := worker.DefaultALPRDetectorConcurrency
	if d.cfg != nil && d.cfg.ALPR != nil && d.cfg.ALPR.DetectorConcurrency > 0 {
		detectorConcurrency = d.cfg.ALPR.DetectorConcurrency
	}
	detectorConcurrency = envInt("ALPR_DETECTOR_CONCURRENCY", detectorConcurrency)

	detectTimeout := worker.DefaultALPRDetectTimeout
	if v := envInt("ALPR_DETECT_TIMEOUT_MS", 0); v > 0 {
		detectTimeout = time.Duration(v) * time.Millisecond
	}

	defaultConfMin := worker.DefaultALPRConfidenceMin
	if d.cfg != nil && d.cfg.ALPR != nil && d.cfg.ALPR.ConfidenceMin > 0 {
		defaultConfMin = d.cfg.ALPR.ConfidenceMin
	}

	d.alprDetectionsComplete = make(chan worker.RouteAlprDetectionsComplete, 64)

	detector := worker.NewALPRDetector(
		d.alprFrames,
		d.ALPRClient(),
		worker.WrapPgxQueries(d.queries),
		d.pool,
		d.settings,
		d.alprKeyring,
		d.metrics,
		d.alprDetectionsComplete,
	)
	detector.Concurrency = detectorConcurrency
	detector.DetectTimeout = detectTimeout
	detector.DefaultConfidenceMin = defaultConfMin
	go detector.Run(ctx)
	keyringConfigured := d.alprKeyring != nil
	log.Printf("alpr detector started (concurrency=%d, detect_timeout=%s, confidence_min=%g, keyring_configured=%v)",
		detectorConcurrency, detectTimeout, defaultConfMin, keyringConfigured)
}
