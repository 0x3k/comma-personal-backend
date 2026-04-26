package main

import (
	"context"
	"log"
	"time"

	"comma-personal-backend/internal/geocode"
	"comma-personal-backend/internal/worker"
)

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
}
