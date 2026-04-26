package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/cereal"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// Defaults for RouteMetadataWorker. Tuned to the same shape as the
// TripAggregator defaults so the two workers play nicely together; the
// metadata pass MUST happen before the trip aggregator can produce useful
// stats, so a shorter poll than the aggregator would just spin needlessly.
const (
	defaultMetadataPollInterval   = 60 * time.Second
	defaultMetadataFinalizedAfter = 5 * time.Minute
	defaultMetadataBatchLimit     = 50
)

// RouteMetadataWorker is a background worker that backfills the
// (start_time, end_time, geometry) columns of the routes table from the
// uploaded qlogs. It mirrors TripAggregator's shape on purpose: same
// configuration knobs, same per-route error logging, same RunOnce error
// contract (only failures to *list* candidates are returned).
//
// The worker MUST run before the trip aggregator can produce useful
// numbers -- the aggregator reads start_time / end_time / geometry off the
// routes row and writes NULL stats when they are missing. Wiring puts
// metadata before trip aggregation in cmd/server/workers.go to make the
// dependency obvious; the trip aggregator's behavior is otherwise
// unchanged by this worker.
type RouteMetadataWorker struct {
	// Queries is the sqlc-generated db handle. Required.
	Queries *db.Queries

	// Storage is the filesystem store. Required.
	Storage *storage.Storage

	// PollInterval is how long to sleep between passes. Defaults to 60s.
	PollInterval time.Duration

	// FinalizedAfter is the minimum age of the most recent segment
	// before a route is considered "done uploading" and eligible for
	// metadata extraction. Defaults to 5m.
	FinalizedAfter time.Duration

	// BatchLimit caps the number of routes processed per pass so the
	// worker doesn't monopolize disk I/O on a backlog. Defaults to 50.
	BatchLimit int32

	// Extractor lets callers override the parser entry point (used in
	// tests). When nil, the worker concatenates qlog files from disk
	// using Storage and the qlogPickerOrder defined in event_detector.go.
	Extractor func(ctx context.Context, dongleID, route string) (*cereal.RouteMetadata, error)
}

// NewRouteMetadataWorker constructs a worker with sane defaults. Both
// queries and storage are required at run time; the constructor does not
// validate them so callers can inject doubles in tests.
func NewRouteMetadataWorker(queries *db.Queries, store *storage.Storage) *RouteMetadataWorker {
	return &RouteMetadataWorker{
		Queries:        queries,
		Storage:        store,
		PollInterval:   defaultMetadataPollInterval,
		FinalizedAfter: defaultMetadataFinalizedAfter,
		BatchLimit:     defaultMetadataBatchLimit,
	}
}

// Run drives the metadata extraction loop until ctx is cancelled. Per-route
// errors are logged and do not abort the loop; only a context cancellation
// or an unrecoverable list failure ends the run.
func (w *RouteMetadataWorker) Run(ctx context.Context) {
	poll := w.PollInterval
	if poll <= 0 {
		poll = defaultMetadataPollInterval
	}

	// Run one pass immediately so a server restart doesn't leave a backlog
	// waiting for the first tick. Same pattern as TripAggregator.
	if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("route_metadata: pass failed: %v", err)
	}

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("route_metadata: pass failed: %v", err)
			}
		}
	}
}

// RunOnce executes a single extraction pass. Per-route errors are logged
// and do not abort the pass; only a failure to list candidate routes is
// surfaced as the returned error.
func (w *RouteMetadataWorker) RunOnce(ctx context.Context) error {
	limit := w.BatchLimit
	if limit <= 0 {
		limit = defaultMetadataBatchLimit
	}
	finalizedAfter := w.FinalizedAfter
	if finalizedAfter <= 0 {
		finalizedAfter = defaultMetadataFinalizedAfter
	}

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-finalizedAfter), Valid: true}
	routes, err := w.Queries.ListRoutesNeedingMetadata(ctx, db.ListRoutesNeedingMetadataParams{
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
		if err := w.processRoute(ctx, r); err != nil {
			log.Printf("route_metadata: route %s/%s: %v", r.DongleID, r.RouteName, err)
		}
	}
	return nil
}

// processRoute extracts metadata from the route's uploaded qlogs and writes
// any non-zero values back via UpdateRouteMetadata. A route with no GPS
// samples still gets its start/end timestamps written so the trip
// aggregator can compute duration; a route with no usable timestamps and
// no GPS triggers no UPDATE at all (so the row stays NULL and the worker
// will retry on the next pass when more data arrives).
func (w *RouteMetadataWorker) processRoute(ctx context.Context, r db.RouteNeedingMetadata) error {
	meta, err := w.extract(ctx, r.DongleID, r.RouteName)
	if err != nil {
		return fmt.Errorf("extract metadata: %w", err)
	}
	if meta == nil {
		return nil
	}

	params := db.UpdateRouteMetadataParams{ID: r.ID}

	if !meta.StartTime.IsZero() {
		params.StartTime = pgtype.Timestamptz{Time: meta.StartTime.UTC(), Valid: true}
	}
	if !meta.EndTime.IsZero() {
		params.EndTime = pgtype.Timestamptz{Time: meta.EndTime.UTC(), Valid: true}
	}
	if wkt := buildLineStringWKT(meta.Track); wkt != "" {
		params.GeometryWkt = pgtype.Text{String: wkt, Valid: true}
		// Per-vertex times are written in lockstep with the WKT so the
		// parallel-array invariant (length(geometry_times) ==
		// ST_NumPoints(geometry)) is preserved at every write. The SQL
		// layer also enforces this server-side as defense in depth.
		params.GeometryTimesMs = buildTrackTimesMs(meta.Track, meta.StartTime)
	}

	if !params.StartTime.Valid && !params.EndTime.Valid && !params.GeometryWkt.Valid {
		// Nothing to write. Leaving the route untouched means the next
		// pass will pick it up again -- expected when a route has no
		// uploaded qlogs yet, or when the qlogs were truncated before
		// any GPS / initData arrived.
		return nil
	}

	if err := w.Queries.UpdateRouteMetadata(ctx, params); err != nil {
		return fmt.Errorf("update route metadata: %w", err)
	}
	return nil
}

// extract runs either a caller-supplied extractor (tests) or concatenates
// every uploaded qlog file for the route on disk and feeds it to the
// cereal RouteMetadataExtractor.
func (w *RouteMetadataWorker) extract(ctx context.Context, dongleID, route string) (*cereal.RouteMetadata, error) {
	if w.Extractor != nil {
		return w.Extractor(ctx, dongleID, route)
	}
	if w.Storage == nil {
		return nil, fmt.Errorf("storage not configured")
	}
	segments, err := w.Storage.ListSegments(dongleID, route)
	if err != nil {
		return nil, fmt.Errorf("list segments: %w", err)
	}
	readers := make([]io.Reader, 0, len(segments))
	closers := make([]io.Closer, 0, len(segments))
	defer func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}()
	for _, seg := range segments {
		segName := strconv.Itoa(seg)
		// Same picker order as the event detector: prefer zstd (current
		// upload format), then bz2, then raw. The cereal Parser
		// auto-detects all three, so we just pick the first existing
		// file per segment.
		for _, name := range qlogPickerOrder {
			if !w.Storage.Exists(dongleID, route, segName, name) {
				continue
			}
			f, err := os.Open(w.Storage.Path(dongleID, route, segName, name))
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", name, err)
			}
			readers = append(readers, f)
			closers = append(closers, f)
			break
		}
	}
	if len(readers) == 0 {
		// No qlogs uploaded yet. Return a zero metadata so the worker
		// no-ops on this pass; the route stays in NULL state and the
		// next pass will retry.
		return &cereal.RouteMetadata{}, nil
	}
	extractor := &cereal.RouteMetadataExtractor{}
	return extractor.ExtractRouteMetadata(io.MultiReader(readers...))
}

// buildLineStringWKT renders a slice of GPS points as a Postgres-friendly
// WKT LineString ("LINESTRING(lng lat, lng lat, ...)"). Returns the empty
// string when the input has fewer than two points -- a single-point WKT is
// not a valid LineString and ST_GeomFromText would either error or
// produce a POINT, neither of which matches the routes.geometry column.
//
// Coordinate order is (lng, lat) per the WKT spec, even though most
// human-readable geo APIs use (lat, lng). PostGIS ST_GeomFromText with
// SRID 4326 expects WKT in (X, Y) = (lng, lat) order; getting this
// reversed would put every route on the wrong continent.
func buildLineStringWKT(track []cereal.GpsPoint) string {
	if len(track) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteString("LINESTRING(")
	for i, p := range track {
		if i > 0 {
			b.WriteString(", ")
		}
		// %g keeps the WKT compact while preserving full float64 precision.
		fmt.Fprintf(&b, "%g %g", p.Lng, p.Lat)
	}
	b.WriteByte(')')
	return b.String()
}

// buildTrackTimesMs renders a slice of GPS points as a parallel-array of
// route-relative milliseconds (point.Time - startTime, in ms). The output
// length matches the WKT vertex count so the parallel-array invariant
// (length(geometry_times) == ST_NumPoints(geometry)) holds when both are
// fed into UpdateRouteMetadata in lockstep.
//
// Returns nil when the input has fewer than two points, because the WKT
// helper drops single-point tracks too -- writing times without geometry
// would corrupt the parallel-array invariant on the next read.
//
// Times are clamped to zero when point.Time precedes startTime (a stale
// RTC at the head of a route can produce a wallTimeNanos that's a few
// seconds ahead of the first GPS fix; treating it as -2000 ms would
// confuse the bisection on the frontend). Points with a zero Time
// (extractor could not derive one) are written as zero rather than
// dropped, so the index alignment with geometry stays exact.
func buildTrackTimesMs(track []cereal.GpsPoint, startTime time.Time) []int64 {
	if len(track) < 2 {
		return nil
	}
	out := make([]int64, len(track))
	if startTime.IsZero() {
		// No anchor: every offset is meaningless. Leave zeroes; the
		// frontend will still render the dot via the fraction fallback.
		return out
	}
	for i, p := range track {
		if p.Time.IsZero() {
			out[i] = 0
			continue
		}
		out[i] = max(p.Time.Sub(startTime).Milliseconds(), 0)
	}
	return out
}
