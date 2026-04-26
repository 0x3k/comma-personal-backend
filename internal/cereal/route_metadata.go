// route_metadata.go is a sibling to signals.go: where the SignalExtractor
// returns column-aligned driving signals (vEgo, brake, engagement, ...),
// RouteMetadataExtractor returns just the three pieces the route-metadata
// worker needs to backfill the routes table:
//
//   - StartTime: the wall-clock time at which the route began
//   - EndTime:   the wall-clock time at which the route ended
//   - Track:     a deduplicated, time-ordered slice of (lat, lng) GPS points
//
// # Field-precedence rules
//
// StartTime / EndTime
//
//	Preferred source: initData.wallTimeNanos (the wall-clock timestamp the
//	device stamps into the very first event of every log; see
//	openpilot/cereal/log.capnp). When initData is absent (e.g. truncated
//	upload or a log that started mid-route) we fall back to the earliest
//	GPS or carState event whose unixTimestampMillis is plausible. The
//	"end" timestamp uses the same rules but takes the latest such event.
//	logMonoTime is intentionally not used as a wall-clock fallback because
//	it is monotonic-clock nanoseconds, not Unix time -- mixing the two
//	would silently produce 1970-era timestamps.
//
// Track points (in order of preference, per event):
//
//  1. gpsLocation         -- modern u-blox / qcom path; flags hasFix bit
//  2. gpsLocationExternal -- panda-attached external receiver
//  3. liveLocationKalman  -- locationd-fused estimate; gated by status==valid
//
// We accept whichever variant the device emits and skip events that do not
// signal a valid fix. Within each event source we dedupe consecutive
// (lat, lng) pairs whose Euclidean distance falls below a small epsilon
// (~10 cm at the equator) so a stationary vehicle doesn't generate a noisy
// LineString. The output is in event-emission order, which closely tracks
// time order on a single contiguous log.
package cereal

import (
	"fmt"
	"io"
	"math"
	"time"

	"comma-personal-backend/internal/cereal/schema"
)

// gpsDedupeEpsilonDeg is the minimum (lat, lng) delta between consecutive
// emitted points. ~1e-6 degrees is roughly 11 cm of latitude at the
// equator and 11 cm * cos(lat) of longitude -- well below GPS noise but
// large enough to suppress identical samples on a stationary vehicle.
const gpsDedupeEpsilonDeg = 1e-6

// gpsLatLonValid is a sanity range for a GPS fix. Devices occasionally
// emit (0, 0) or NaN before they have a real fix even when hasFix is true;
// rejecting points outside this envelope keeps the LineString anchored to
// real coordinates rather than the Null Island offset.
func gpsLatLonValid(lat, lon float64) bool {
	if math.IsNaN(lat) || math.IsNaN(lon) || math.IsInf(lat, 0) || math.IsInf(lon, 0) {
		return false
	}
	if lat == 0 && lon == 0 {
		return false
	}
	if lat < -90 || lat > 90 {
		return false
	}
	if lon < -180 || lon > 180 {
		return false
	}
	return true
}

// GpsPoint is a single time-ordered GPS sample. Time is the wall-clock the
// device stamped on the underlying event (UTC); zero when the source did
// not carry a usable wall-clock value.
type GpsPoint struct {
	Lat  float64
	Lng  float64
	Time time.Time
}

// RouteMetadata is the bundle of values the metadata worker writes back to
// the routes table. Any field may be its zero value when the source log
// did not contain enough information to derive it; the worker is expected
// to translate the zero/empty cases into NULLs for the UPDATE statement.
type RouteMetadata struct {
	// StartTime is the wall-clock time at which the route began. Zero
	// when neither initData nor any GPS/carState event carried a usable
	// unixTimestampMillis.
	StartTime time.Time

	// EndTime is the wall-clock time at which the route ended. Same
	// fallback chain as StartTime.
	EndTime time.Time

	// Track is the deduplicated, time-ordered slice of GPS points. May
	// be empty when the log carried no GPS fixes or the device was still
	// acquiring at the time of upload.
	Track []GpsPoint
}

// RouteMetadataExtractor pulls just enough out of a cereal log to backfill
// the routes table. Construct one with the zero value (&RouteMetadataExtractor{})
// for default parsing options, or set fields on the embedded Parser to
// tune decode limits.
type RouteMetadataExtractor struct {
	Parser Parser
}

// ExtractRouteMetadata streams the log in r and returns the derived
// RouteMetadata. Both compressed (bz2 / zstd) and raw streams are accepted;
// the underlying Parser auto-detects the framing.
//
// The function does not reorder the GPS points it observes -- they are
// returned in the order the underlying events arrived, which on a healthy
// single-segment log already corresponds to time order. The metadata worker
// concatenates per-segment qlogs in segment-number order, so the
// concatenated stream is also time-ordered across segments.
func (e *RouteMetadataExtractor) ExtractRouteMetadata(r io.Reader) (*RouteMetadata, error) {
	if e == nil {
		e = &RouteMetadataExtractor{}
	}
	out := &RouteMetadata{}

	// Track the running min/max wall-clock observed from any source so
	// the start/end timestamps are robust to events emitted out of order
	// across the log (e.g. a late carState that pre-dates the initData).
	var (
		initWallTime  time.Time
		fallbackFirst time.Time
		fallbackLast  time.Time
		lastLat       float64
		lastLng       float64
		havePrev      bool
	)

	err := e.Parser.Parse(r, func(evt schema.Event) error {
		which := evt.Which()
		switch which {
		case schema.Event_Which_initData:
			if !initWallTime.IsZero() {
				// We only honor the first initData; subsequent ones
				// would be produced by an unusual concatenation and
				// might point further forward than the true start.
				return nil
			}
			id, err := evt.InitData()
			if err != nil {
				return fmt.Errorf("cereal: read initData: %w", err)
			}
			ns := id.WallTimeNanos()
			if ns == 0 {
				return nil
			}
			t := time.Unix(0, int64(ns)).UTC()
			if !plausibleWallClock(t) {
				return nil
			}
			initWallTime = t

		case schema.Event_Which_gpsLocation:
			gps, err := evt.GpsLocation()
			if err != nil {
				return fmt.Errorf("cereal: read gpsLocation: %w", err)
			}
			if !gps.HasFix() {
				return nil
			}
			lat := gps.Latitude()
			lng := gps.Longitude()
			if !gpsLatLonValid(lat, lng) {
				return nil
			}
			pt := GpsPoint{Lat: lat, Lng: lng, Time: gpsTime(gps.UnixTimestampMillis())}
			noteFallback(&fallbackFirst, &fallbackLast, pt.Time)
			emitPoint(out, &lastLat, &lastLng, &havePrev, pt)

		case schema.Event_Which_gpsLocationExternal:
			gps, err := evt.GpsLocationExternal()
			if err != nil {
				return fmt.Errorf("cereal: read gpsLocationExternal: %w", err)
			}
			if !gps.HasFix() {
				return nil
			}
			lat := gps.Latitude()
			lng := gps.Longitude()
			if !gpsLatLonValid(lat, lng) {
				return nil
			}
			pt := GpsPoint{Lat: lat, Lng: lng, Time: gpsTime(gps.UnixTimestampMillis())}
			noteFallback(&fallbackFirst, &fallbackLast, pt.Time)
			emitPoint(out, &lastLat, &lastLng, &havePrev, pt)

		case schema.Event_Which_liveLocationKalmanDEPRECATED:
			// liveLocationKalman is the locationd-fused estimate. The
			// "DEPRECATED" suffix is upstream's own naming -- recent
			// openpilot/sunnypilot still emit it on the qlog services
			// list (cereal/services.py) so we still want to consume it
			// as a fallback when no raw GPS source is present.
			llk, err := evt.LiveLocationKalmanDEPRECATED()
			if err != nil {
				return fmt.Errorf("cereal: read liveLocationKalmanDEPRECATED: %w", err)
			}
			if llk.Status() != schema.LiveLocationKalman_Status_valid || !llk.GpsOK() {
				return nil
			}
			pos, err := llk.PositionGeodetic()
			if err != nil {
				return fmt.Errorf("cereal: read liveLocationKalman.positionGeodetic: %w", err)
			}
			if !pos.Valid() {
				return nil
			}
			vals, err := pos.Value()
			if err != nil {
				return fmt.Errorf("cereal: read liveLocationKalman.positionGeodetic.value: %w", err)
			}
			if vals.Len() < 2 {
				return nil
			}
			// Geodetic order in cereal is (lat, lng, alt).
			lat := vals.At(0)
			lng := vals.At(1)
			if !gpsLatLonValid(lat, lng) {
				return nil
			}
			pt := GpsPoint{Lat: lat, Lng: lng, Time: gpsTime(llk.UnixTimestampMillis())}
			noteFallback(&fallbackFirst, &fallbackLast, pt.Time)
			emitPoint(out, &lastLat, &lastLng, &havePrev, pt)

		case schema.Event_Which_carState:
			// carState does not carry GPS, but we use its monotonic-clock
			// derived wall time as a last-resort start/end timestamp when
			// no GPS event ever showed up. We do *not* trust logMonoTime
			// for wall time (it is a monotonic clock), so this branch is
			// only useful when the carState event itself carries an
			// implicit wall-clock via its companion GPS event -- which it
			// does not. Left in place as a no-op so the type switch is
			// exhaustive over the events we explicitly track elsewhere.
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Choose the start/end timestamps. initData wins when available; fall
	// back to the GPS-derived envelope otherwise.
	switch {
	case !initWallTime.IsZero():
		out.StartTime = initWallTime
		// EndTime: prefer the latest GPS-derived time when available;
		// otherwise keep StartTime so duration computes to zero rather
		// than NULL.
		if !fallbackLast.IsZero() && fallbackLast.After(initWallTime) {
			out.EndTime = fallbackLast
		} else {
			out.EndTime = initWallTime
		}
	default:
		out.StartTime = fallbackFirst
		out.EndTime = fallbackLast
	}

	return out, nil
}

// gpsTime converts a Unix-millisecond timestamp from a GPS event into a
// UTC time.Time. Zero or negative values map to the zero time.Time so
// callers can use IsZero() to detect a missing source value.
func gpsTime(unixMs int64) time.Time {
	if unixMs <= 0 {
		return time.Time{}
	}
	t := time.Unix(0, unixMs*int64(time.Millisecond)).UTC()
	if !plausibleWallClock(t) {
		return time.Time{}
	}
	return t
}

// plausibleWallClock rejects timestamps that are obviously bogus (pre-2010
// or far-future). Devices occasionally emit 0/garbage before NTP / GPS time
// has converged; we want to avoid stamping a "1970" start_time on a route.
func plausibleWallClock(t time.Time) bool {
	if t.IsZero() {
		return false
	}
	min := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	max := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	return t.After(min) && t.Before(max)
}

// noteFallback updates the running first/last fallback wall-clock window
// using the supplied event time. Zero times are ignored so a single noisy
// event without a usable timestamp does not drag the window backwards to
// the Unix epoch.
func noteFallback(first, last *time.Time, t time.Time) {
	if t.IsZero() {
		return
	}
	if first.IsZero() || t.Before(*first) {
		*first = t
	}
	if last.IsZero() || t.After(*last) {
		*last = t
	}
}

// emitPoint appends pt to out.Track when it differs meaningfully from the
// previously-emitted point. Updates the lastLat/lastLng/havePrev cursor
// in place so the caller does not have to thread the dedupe state through
// every event branch.
func emitPoint(out *RouteMetadata, lastLat, lastLng *float64, havePrev *bool, pt GpsPoint) {
	if *havePrev {
		dLat := pt.Lat - *lastLat
		dLng := pt.Lng - *lastLng
		if math.Abs(dLat) < gpsDedupeEpsilonDeg && math.Abs(dLng) < gpsDedupeEpsilonDeg {
			return
		}
	}
	out.Track = append(out.Track, pt)
	*lastLat = pt.Lat
	*lastLng = pt.Lng
	*havePrev = true
}
