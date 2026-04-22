// Package worker includes background jobs that run alongside the API
// server. The TripAggregator in this file watches the routes table for
// uploads that look "finalized" (no new segments for a while) and writes
// derived summary rows into the trips table.
package worker

import (
	"context"
	"errors"
	"log"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/geocode"
)

// Defaults for TripAggregator configuration. These are intentionally
// conservative; operators who want faster turnaround can override them.
const (
	defaultPollInterval   = 60 * time.Second
	defaultFinalizedAfter = 5 * time.Minute
	defaultBatchLimit     = 50
)

// TripAggregator is a background worker that computes trip summaries from
// route geometry and segment timestamps. It is safe to run alongside the
// HTTP server; each iteration issues a small, bounded batch of queries.
//
// A nil Geocoder disables reverse-geocoding: start/end addresses will
// simply be left NULL on the trip row. This is useful for tests that
// don't want to depend on a Nominatim instance, and for operators who
// haven't configured one.
type TripAggregator struct {
	// Queries is the sqlc-generated db handle. Required.
	Queries *db.Queries

	// Geocoder performs reverse lookups for start/end coordinates.
	// Optional; if nil, addresses are left NULL.
	Geocoder *geocode.Client

	// PollInterval is how long to sleep between aggregation passes.
	// Defaults to 60s when zero.
	PollInterval time.Duration

	// FinalizedAfter is the minimum age of the most recent segment
	// before a route is considered "done uploading" and eligible for
	// aggregation. Defaults to 5 minutes when zero.
	FinalizedAfter time.Duration

	// BatchLimit caps the number of routes aggregated per pass so the
	// worker doesn't monopolize the DB on a backlog. Defaults to 50.
	BatchLimit int32
}

// NewTripAggregator constructs a TripAggregator with sane defaults.
// Queries is required; geocoder may be nil.
func NewTripAggregator(queries *db.Queries, geocoder *geocode.Client) *TripAggregator {
	return &TripAggregator{
		Queries:        queries,
		Geocoder:       geocoder,
		PollInterval:   defaultPollInterval,
		FinalizedAfter: defaultFinalizedAfter,
		BatchLimit:     defaultBatchLimit,
	}
}

// Run drives the aggregation loop until ctx is cancelled. It logs and
// continues on per-route errors so one bad route does not halt progress.
func (a *TripAggregator) Run(ctx context.Context) {
	poll := a.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}

	// Run one pass immediately so a server-restart doesn't leave a backlog
	// waiting for the first tick.
	if err := a.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("trip_aggregator: pass failed: %v", err)
	}

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("trip_aggregator: pass failed: %v", err)
			}
		}
	}
}

// RunOnce executes a single aggregation pass. It finds eligible routes
// and upserts a trip row for each. Per-route errors are logged and do
// not abort the pass; only a failure to list routes is returned.
func (a *TripAggregator) RunOnce(ctx context.Context) error {
	limit := a.BatchLimit
	if limit <= 0 {
		limit = defaultBatchLimit
	}
	finalizedAfter := a.FinalizedAfter
	if finalizedAfter <= 0 {
		finalizedAfter = defaultFinalizedAfter
	}

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-finalizedAfter), Valid: true}
	routes, err := a.Queries.ListRoutesForTripAggregation(ctx, db.ListRoutesForTripAggregationParams{
		FinalizedBefore: cutoff,
		Limit:           limit,
	})
	if err != nil {
		return err
	}

	for _, r := range routes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := a.aggregateRoute(ctx, r); err != nil {
			log.Printf("trip_aggregator: route %s/%s: %v", r.DongleID, r.RouteName, err)
		}
	}
	return nil
}

// aggregateRoute computes and upserts a trip row for a single route.
// A route without geometry still yields a row (with NULL stats) so the
// UI can distinguish "no GPS" from "not yet processed".
func (a *TripAggregator) aggregateRoute(ctx context.Context, r db.RouteForTripAggregation) error {
	params := db.UpsertTripParams{
		RouteID:    r.ID,
		ComputedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		// Engagement is filled in by the event-detector worker later; the
		// column is nullable so leaving this unset keeps the row honest.
		EngagedSeconds: pgtype.Int4{},
	}

	// Duration comes from the route's start/end timestamps when both are
	// set. Routes that haven't had their end_time recorded yet simply get
	// a NULL duration, same as the "no GPS" path.
	var durationSeconds float64
	if r.StartTime.Valid && r.EndTime.Valid {
		d := r.EndTime.Time.Sub(r.StartTime.Time).Seconds()
		if d > 0 {
			durationSeconds = d
			params.DurationSeconds = pgtype.Int4{Int32: int32(math.Round(d)), Valid: true}
		}
	}

	// Geometry-derived stats. We treat "no usable geometry" (missing,
	// empty, or single-point) the same as "no GPS" and leave the
	// distance/speed/lat/lng fields NULL. The row is still written so
	// downstream consumers see a deterministic shape.
	stats, err := a.Queries.GetRouteGeometryStats(ctx, r.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		params.DistanceMeters = pgtype.Float8{Float64: stats.DistanceMeters, Valid: true}
		params.StartLat = pgtype.Float8{Float64: stats.StartLat, Valid: true}
		params.StartLng = pgtype.Float8{Float64: stats.StartLng, Valid: true}
		params.EndLat = pgtype.Float8{Float64: stats.EndLat, Valid: true}
		params.EndLng = pgtype.Float8{Float64: stats.EndLng, Valid: true}

		if durationSeconds > 0 {
			avg := stats.DistanceMeters / durationSeconds
			if avg >= 0 {
				params.AvgSpeedMps = pgtype.Float8{Float64: avg, Valid: true}
			}

			// Approximate max speed by distributing the route duration
			// uniformly across the (n-1) geometry segments and picking
			// the longest segment. This is a lower bound on the true
			// peak speed (a high-frequency GPS trace would yield
			// tighter numbers), but it's still strictly better than
			// reporting the average as the max.
			if stats.NumPoints > 1 {
				maxLen, err := a.Queries.GetRouteGeometrySegmentMaxLength(ctx, r.ID)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
				if err == nil {
					pairs := float64(stats.NumPoints - 1)
					dt := durationSeconds / pairs
					if dt > 0 {
						params.MaxSpeedMps = pgtype.Float8{Float64: maxLen / dt, Valid: true}
					}
				}
			}
		}

		// Reverse-geocode the endpoints. Failures here are non-fatal:
		// we log and proceed with NULL addresses.
		if a.Geocoder != nil {
			if addr, err := a.Geocoder.Reverse(ctx, stats.StartLat, stats.StartLng); err == nil {
				params.StartAddress = pgtype.Text{String: addr, Valid: true}
			} else if !errors.Is(err, geocode.ErrNotFound) && !errors.Is(err, context.Canceled) {
				log.Printf("trip_aggregator: reverse start %s/%s: %v", r.DongleID, r.RouteName, err)
			}
			if addr, err := a.Geocoder.Reverse(ctx, stats.EndLat, stats.EndLng); err == nil {
				params.EndAddress = pgtype.Text{String: addr, Valid: true}
			} else if !errors.Is(err, geocode.ErrNotFound) && !errors.Is(err, context.Canceled) {
				log.Printf("trip_aggregator: reverse end %s/%s: %v", r.DongleID, r.RouteName, err)
			}
		}
	}

	if _, err := a.Queries.UpsertTrip(ctx, params); err != nil {
		return err
	}
	return nil
}
