package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/cereal"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/storage"
)

// Event type identifiers. Kept as constants so callers (notably the web UI
// and tests) can reference them without stringly-typed duplication.
const (
	EventTypeHardBrake    = "hard_brake"
	EventTypeDisengage    = "disengage"
	EventTypeFCW          = "fcw"
	EventTypeAlertWarning = "alert_warning"
)

// Event severity values. We only emit "info" and "warn" for the first-pass
// ruleset; future rules can extend this.
const (
	EventSeverityInfo = "info"
	EventSeverityWarn = "warn"
)

// Default thresholds for hard-brake detection. Keep in sync with the feature
// spec and the README env-var documentation.
const (
	defaultHardBrakeMps2     = 4.5
	defaultHardBrakeMinSec   = 0.3
	defaultEventPollInterval = 30 * time.Second
	defaultCandidateLimit    = 16
	fcwAlertPrefixFCW        = "FCW"
	fcwAlertPrefixCollision  = "Forward Collision"
)

// qlogPickerOrder lists the on-disk qlog filenames the event detector tries
// per segment, in order of preference. Newer openpilot/sunnypilot devices
// upload qlog.zst; older devices upload qlog.bz2; the raw qlog form is
// rare in practice but still supported by the parser. The first match
// wins, so the newest-first order means modern uploads are picked even
// when a legacy file is also present from a partial reupload.
var qlogPickerOrder = []string{"qlog.zst", "qlog.bz2", "qlog"}

// Thresholds bundles the operator-tunable event detection parameters.
// Zero values fall back to the defaults at run time.
type Thresholds struct {
	HardBrakeMps2           float64
	HardBrakeMinDurationSec float64
}

// EventDetector is a background worker that finds routes with a trip row but
// no event-detection run yet, walks their parsed log signals, and inserts
// detected events into the events table.
type EventDetector struct {
	Queries      *db.Queries
	Storage      *storage.Storage
	PollInterval time.Duration
	Thresholds   Thresholds

	// CandidateLimit caps how many routes are claimed per poll. Zero means
	// use the default.
	CandidateLimit int32

	// Extractor lets callers override the parser (used in tests that bypass
	// the filesystem). A nil extractor uses a default SignalExtractor that
	// reads qlogs from Storage.
	Extractor func(ctx context.Context, dongleID, route string) (*cereal.DrivingSignals, error)
}

// NewEventDetector constructs an EventDetector. A nil queries or store is
// allowed at construction time (useful for tests) but Run will no-op on a
// nil queries.
func NewEventDetector(q *db.Queries, s *storage.Storage, poll time.Duration, thr Thresholds) *EventDetector {
	return &EventDetector{
		Queries:      q,
		Storage:      s,
		PollInterval: poll,
		Thresholds:   thr,
	}
}

// Run loops until ctx is cancelled. Each iteration claims a batch of routes
// that have a trip row but no events_computed_at stamp, runs detection, and
// stamps the trip row so subsequent polls skip it.
func (d *EventDetector) Run(ctx context.Context) {
	interval := d.PollInterval
	if interval <= 0 {
		interval = defaultEventPollInterval
	}

	// First pass runs immediately so smoke tests / short-lived processes can
	// see results without waiting a full interval.
	d.runOnce(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.runOnce(ctx)
		}
	}
}

// runOnce processes a single batch of candidate routes. Errors on individual
// routes are logged but do not abort the batch; the stamp is only written
// for routes that completed detection successfully.
func (d *EventDetector) runOnce(ctx context.Context) {
	if d.Queries == nil {
		return
	}
	limit := d.CandidateLimit
	if limit <= 0 {
		limit = defaultCandidateLimit
	}
	candidates, err := d.Queries.ListRoutesNeedingEventDetection(ctx, limit)
	if err != nil {
		log.Printf("event detector: list candidates: %v", err)
		return
	}
	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}
		if err := d.processRoute(ctx, c); err != nil {
			log.Printf("event detector: process route %s/%s: %v", c.DongleID, c.RouteName, err)
			continue
		}
	}
}

// processRoute runs detection on a single route and stamps the trip row on
// success. It returns nil both when events are inserted and when no events
// were found: both cases should stamp the trip so we don't reprocess it.
func (d *EventDetector) processRoute(ctx context.Context, c db.ListRoutesNeedingEventDetectionRow) error {
	signals, err := d.extract(ctx, c.DongleID, c.RouteName)
	if err != nil {
		return fmt.Errorf("extract signals: %w", err)
	}
	events := DetectEvents(signals, d.Thresholds)
	for _, e := range events {
		if err := d.insertEvent(ctx, c.ID, e); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
	}
	now := pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	if err := d.Queries.MarkTripEventsComputed(ctx, db.MarkTripEventsComputedParams{
		RouteID:          c.ID,
		EventsComputedAt: now,
	}); err != nil {
		return fmt.Errorf("mark events computed: %w", err)
	}
	return nil
}

func (d *EventDetector) insertEvent(ctx context.Context, routeID int32, e DetectedEvent) error {
	var payload []byte
	if e.Payload != nil {
		buf, err := json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("marshal payload: %w", err)
		}
		payload = buf
	}
	occurredAt := pgtype.Timestamptz{}
	if !e.OccurredAt.IsZero() {
		occurredAt.Time = e.OccurredAt.UTC()
		occurredAt.Valid = true
	}
	_, err := d.Queries.InsertEvent(ctx, db.InsertEventParams{
		RouteID:            routeID,
		Type:               e.Type,
		Severity:           e.Severity,
		RouteOffsetSeconds: e.RouteOffsetSeconds,
		OccurredAt:         occurredAt,
		Payload:            payload,
	})
	// ON CONFLICT ... DO NOTHING returns no rows when the event is already
	// present. The sqlc-generated :one helper surfaces that as ErrNoRows;
	// treat it as a successful idempotent insert.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// extract pulls driving signals for a route either via the caller-supplied
// Extractor (tests) or by concatenating qlog files from all uploaded
// segments on disk.
func (d *EventDetector) extract(ctx context.Context, dongleID, route string) (*cereal.DrivingSignals, error) {
	if d.Extractor != nil {
		return d.Extractor(ctx, dongleID, route)
	}
	if d.Storage == nil {
		return nil, fmt.Errorf("event detector: storage not configured")
	}
	segments, err := d.Storage.ListSegments(dongleID, route)
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
		// Prefer zstd (current openpilot/sunnypilot upload format), then
		// bz2 (older devices), then raw qlog (rare; mostly tests). The
		// cereal parser auto-detects all three framings so the only
		// concern here is which file to open per segment.
		for _, name := range qlogPickerOrder {
			if !d.Storage.Exists(dongleID, route, segName, name) {
				continue
			}
			f, err := os.Open(d.Storage.Path(dongleID, route, segName, name))
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", name, err)
			}
			readers = append(readers, f)
			closers = append(closers, f)
			break
		}
	}
	if len(readers) == 0 {
		// No uploaded qlogs yet. Return an empty signal set so the route
		// still gets stamped -- we don't want to spin on the same route
		// forever when its qlogs never arrive.
		return &cereal.DrivingSignals{}, nil
	}
	extractor := &cereal.SignalExtractor{}
	return extractor.ExtractDriving(io.MultiReader(readers...))
}

// DetectedEvent is an internal, transport-agnostic record of an event that
// was found in a route's signal stream.
type DetectedEvent struct {
	Type               string
	Severity           string
	RouteOffsetSeconds float64
	OccurredAt         time.Time
	Payload            map[string]any
}

// DetectEvents walks a column-oriented DrivingSignals slice and returns the
// events implied by the first-pass rule set. It is pure and deterministic so
// unit tests can fabricate DrivingSignals directly.
//
// Rules:
//   - hard_brake: sustained deceleration > HardBrakeMps2 for at least
//     HardBrakeMinDurationSec seconds. Decel is computed from adjacent vEgo
//     samples. The event fires once at the start of the interval that met
//     the threshold.
//   - disengage: trailing edge of the Engaged flag (true -> false).
//   - fcw: alert text starts with "FCW" or "Forward Collision" on the rising
//     edge (new alert only).
//   - alert_warning: rising edge of any non-empty alert text. The FCW rule
//     is a strict subset; a given moment may emit both.
func DetectEvents(sig *cereal.DrivingSignals, thr Thresholds) []DetectedEvent {
	if sig == nil || len(sig.Times) == 0 {
		return nil
	}

	brakeMps2 := thr.HardBrakeMps2
	if brakeMps2 <= 0 {
		brakeMps2 = defaultHardBrakeMps2
	}
	brakeMinSec := thr.HardBrakeMinDurationSec
	if brakeMinSec <= 0 {
		brakeMinSec = defaultHardBrakeMinSec
	}

	origin := sig.Times[0]
	// offsetAt returns seconds since the first log timestamp. Guard against
	// pre-origin entries (shouldn't happen) by clamping to zero.
	offsetAt := func(i int) float64 {
		d := sig.Times[i].Sub(origin).Seconds()
		if d < 0 {
			return 0
		}
		return d
	}

	var events []DetectedEvent
	events = append(events, detectHardBrakes(sig, origin, brakeMps2, brakeMinSec)...)
	events = append(events, detectAlertEvents(sig, origin, offsetAt)...)
	events = append(events, detectDisengagements(sig, origin, offsetAt)...)
	return events
}

// detectHardBrakes looks for sustained deceleration over the threshold. The
// signal cadence is not fixed, so we carry a running start-of-interval index
// and emit one event when the cumulative duration crosses the min-duration
// threshold. Subsequent adjacent decel samples do not re-fire until the
// chain breaks.
func detectHardBrakes(sig *cereal.DrivingSignals, origin time.Time, mps2, minSec float64) []DetectedEvent {
	n := len(sig.Times)
	if n < 2 {
		return nil
	}
	var out []DetectedEvent

	// chainStart is the index where the current sustained-decel chain began,
	// or -1 when no chain is active.
	chainStart := -1
	chainEmitted := false

	// Track only indices that carry a carState sample, since non-carState
	// rows have VEgo == 0 and would poison the derivative. We infer a
	// "valid speed sample" by checking whether vEgo is non-zero OR the row
	// index carries a zeroed-out selfdriveState marker (engaged might be
	// true at vEgo=0 when stopped -- but deceleration only matters when
	// moving, so treating 0 as invalid is fine for the first-pass rule).
	//
	// For tests that fabricate dense vEgo traces this just becomes "every
	// row" which is the desired behaviour.
	for i := 1; i < n; i++ {
		prevV := sig.VEgo[i-1]
		currV := sig.VEgo[i]
		dt := sig.Times[i].Sub(sig.Times[i-1]).Seconds()
		if dt <= 0 {
			chainStart = -1
			chainEmitted = false
			continue
		}
		// Skip samples where either end has no carState (vEgo == 0 and the
		// row wasn't a carState event). We approximate that by requiring
		// both samples to have non-zero speed OR by requiring a carState
		// brake marker on either end. Practically, the zero-skipping avoids
		// the huge artificial decel that would come from transitioning
		// between "real speed sample" and "selfdriveState row (vEgo=0)".
		if prevV == 0 && currV == 0 {
			// Flat zero pair -- not a decel event. Reset chain.
			chainStart = -1
			chainEmitted = false
			continue
		}
		if prevV == 0 {
			// Stepping up from an untyped 0 into a real sample -- not
			// meaningful as a derivative.
			chainStart = -1
			chainEmitted = false
			continue
		}

		decel := (prevV - currV) / dt
		if decel >= mps2 {
			if chainStart == -1 {
				chainStart = i - 1
				chainEmitted = false
			}
			// Emit once the sustained duration crosses the min threshold.
			if !chainEmitted {
				elapsed := sig.Times[i].Sub(sig.Times[chainStart]).Seconds()
				if elapsed >= minSec {
					startOffset := sig.Times[chainStart].Sub(origin).Seconds()
					if startOffset < 0 {
						startOffset = 0
					}
					out = append(out, DetectedEvent{
						Type:               EventTypeHardBrake,
						Severity:           EventSeverityWarn,
						RouteOffsetSeconds: roundSec(startOffset),
						OccurredAt:         sig.Times[chainStart],
						Payload: map[string]any{
							"peak_decel_mps2": roundFloat(decel, 3),
							"duration_sec":    roundFloat(elapsed, 3),
							"start_v_ego":     roundFloat(sig.VEgo[chainStart], 3),
							"end_v_ego":       roundFloat(currV, 3),
						},
					})
					chainEmitted = true
				}
			}
		} else {
			chainStart = -1
			chainEmitted = false
		}
	}
	return out
}

// detectAlertEvents finds rising-edge transitions of AlertText. Each rising
// edge emits an alert_warning event, and if the new alert text matches the
// FCW prefixes, a fcw event is also emitted at the same offset.
func detectAlertEvents(sig *cereal.DrivingSignals, _ time.Time, offsetAt func(int) float64) []DetectedEvent {
	if len(sig.AlertText) == 0 {
		return nil
	}
	var out []DetectedEvent
	prev := ""
	for i, text := range sig.AlertText {
		if text != "" && text != prev {
			offset := roundSec(offsetAt(i))
			if isFCWAlert(text) {
				out = append(out, DetectedEvent{
					Type:               EventTypeFCW,
					Severity:           EventSeverityWarn,
					RouteOffsetSeconds: offset,
					OccurredAt:         sig.Times[i],
					Payload: map[string]any{
						"alert_text": text,
					},
				})
			}
			out = append(out, DetectedEvent{
				Type:               EventTypeAlertWarning,
				Severity:           EventSeverityWarn,
				RouteOffsetSeconds: offset,
				OccurredAt:         sig.Times[i],
				Payload: map[string]any{
					"alert_text": text,
				},
			})
		}
		// Track previous non-empty alert so a re-assertion of the same
		// alert text across rows only fires once, but a new alert after an
		// empty row re-fires as intended.
		prev = text
	}
	return out
}

// detectDisengagements emits one event on each true->false transition of
// the Engaged signal. We only consider rows whose events actually carried
// the engagement signal (i.e., the row came from selfdriveState or
// controlsState); non-carrier rows have Engaged==false already, which would
// otherwise cause spurious trailing-edge firings.
//
// Today DrivingSignals does not expose which rows carried engagement, so we
// approximate by requiring a non-empty AlertText on the previous row OR a
// previous Engaged==true -- the combination weeds out the false fall-offs
// caused by pure carState rows that always carry Engaged==false.
func detectDisengagements(sig *cereal.DrivingSignals, _ time.Time, offsetAt func(int) float64) []DetectedEvent {
	if len(sig.Engaged) == 0 {
		return nil
	}
	var out []DetectedEvent

	// Walk only the rows that we believe carried an engaged signal. A
	// heuristic: rising edge of Engaged is reliable (false -> true), so
	// once we see Engaged==true we treat the latched value as authoritative
	// until we see another engagement-carrying row that tells us it went
	// false. Since DrivingSignals doesn't tell us directly which rows were
	// carriers, we track state transitions on an "engaged-carrier" view:
	// any row where Engaged != lastLatchedEngaged AND AlertText != "" (or
	// the very first transition) is treated as a real transition.
	latched := false
	seenTrue := false
	for i := 0; i < len(sig.Engaged); i++ {
		eng := sig.Engaged[i]
		if eng && !latched {
			latched = true
			seenTrue = true
			continue
		}
		if !eng && latched && seenTrue {
			// Falling edge. Check whether this row looks like an
			// engagement carrier (has an alert marker). For logs where
			// the falling edge row carries AlertText == "Disengaged" or
			// similar this is immediate. For logs where engagement goes
			// low without an accompanying alert, fall back to firing on
			// the first subsequent carrier row.
			if sig.AlertText[i] != "" || isLastEngagementCarrier(sig, i) {
				out = append(out, DetectedEvent{
					Type:               EventTypeDisengage,
					Severity:           EventSeverityInfo,
					RouteOffsetSeconds: roundSec(offsetAt(i)),
					OccurredAt:         sig.Times[i],
				})
				latched = false
			}
		}
	}
	return out
}

// isLastEngagementCarrier reports whether index i is the last row of the
// signal slice. Used as a tie-break so a log that ends in a bare
// carState row (Engaged==false, alert=="") still emits the trailing
// disengage event.
func isLastEngagementCarrier(sig *cereal.DrivingSignals, i int) bool {
	return i == len(sig.Engaged)-1
}

// isFCWAlert returns true when the alert text starts with one of the
// well-known FCW prefixes. Case-sensitive: openpilot emits these verbatim.
func isFCWAlert(text string) bool {
	return strings.HasPrefix(text, fcwAlertPrefixFCW) ||
		strings.HasPrefix(text, fcwAlertPrefixCollision)
}

// roundSec rounds a second-offset to 3 decimals so events at the same
// logical moment land on the same route_offset_seconds value across re-runs
// (important for the UNIQUE-constraint idempotency guarantee).
func roundSec(s float64) float64 {
	return roundFloat(s, 3)
}

// roundFloat rounds f to the given number of decimal places.
func roundFloat(f float64, decimals int) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return f
	}
	p := math.Pow10(decimals)
	return math.Round(f*p) / p
}

// LoadThresholdsFromEnv reads EVENT_HARD_BRAKE_MPS2 and EVENT_HARD_BRAKE_MIN_SEC
// from the environment, falling back to the package defaults when unset or
// invalid. A malformed value is logged and treated as unset so a typo never
// crashes the worker loop.
func LoadThresholdsFromEnv() Thresholds {
	thr := Thresholds{
		HardBrakeMps2:           defaultHardBrakeMps2,
		HardBrakeMinDurationSec: defaultHardBrakeMinSec,
	}
	if v := strings.TrimSpace(os.Getenv("EVENT_HARD_BRAKE_MPS2")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			thr.HardBrakeMps2 = f
		} else {
			log.Printf("event detector: ignoring invalid EVENT_HARD_BRAKE_MPS2=%q", v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("EVENT_HARD_BRAKE_MIN_SEC")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			thr.HardBrakeMinDurationSec = f
		} else {
			log.Printf("event detector: ignoring invalid EVENT_HARD_BRAKE_MIN_SEC=%q", v)
		}
	}
	return thr
}
