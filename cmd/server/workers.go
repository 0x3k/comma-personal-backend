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

	// Background trip aggregator. Defaults on; set TRIP_AGGREGATOR_ENABLED=0
	// (or false/no/off) to skip it, e.g. in constrained test environments.
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
}
