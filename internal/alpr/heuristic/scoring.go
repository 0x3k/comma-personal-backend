// Package heuristic implements the stalking-detection scoring rules for
// the ALPR pipeline. It is intentionally split into two layers:
//
//  1. A pure scoring function -- Score -- that takes a fully-loaded
//     ScoringInput and returns a Result. No I/O, no time.Now, no DB.
//     Tests exercise the rules at this layer with hand-crafted inputs so
//     boundary cases (severity 2.0/4.0/6.0/8.0, whitelist override,
//     equator/180-deg geo bucketing) are easy to assert.
//
//  2. A worker layer (worker.go) that hooks the scorer to the database:
//     loads the recent encounters for an affected plate, calls Score,
//     UPSERTs the watchlist row + inserts an alert_events row, and
//     emits an AlertCreated event for the notification channel.
//
// The two layers are deliberately decoupled because the rule set will
// evolve and we want test-driven iteration on the rules without spinning
// up Postgres or re-running detection. The HeuristicVersion constant
// rides on every alert_events row so the UI can surface "evaluated under
// v1.0.0" alongside the explanation.
package heuristic

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// HeuristicVersion identifies the rule set used to compute alerts.
//
// Bumping this is a deliberate act tied to a deploy: the constant is
// embedded in every plate_alert_events row so the UI can show "evaluated
// under heuristic vX.Y.Z" and the team can change rules without
// silently rewriting history. Document each change in CHANGELOG.md or a
// follow-up project doc. Do NOT make this a settings value -- a runtime
// override would let two replicas disagree on the meaning of "v1.0.0".
const HeuristicVersion = "v1.0.0"

// Default scoring thresholds. Operators can override these via the
// settings store (alpr_heuristic_* keys); the worker layer reads
// settings, applies precedence (settings > env > default), and passes
// the resolved values to Score via Thresholds. See worker.go.
const (
	// DefaultLookbackDays is the number of days of encounter history
	// the worker pulls when scoring a plate. 30 days balances
	// recall (genuinely repeated stalking events spread over weeks)
	// against false positives from a long-tail benign neighbour.
	DefaultLookbackDays = 30

	// DefaultTurnsMin is the per-encounter turn count above which we
	// start scoring within_route_turns. The component formula is
	// max(0, turn_count - turnsMin) * 1.0, capped at the
	// turns-points cap.
	DefaultTurnsMin = 2

	// DefaultTurnsPointsCap caps the within_route_turns component so
	// a single chaotic route cannot single-handedly push severity to
	// 5. 4 points is "obvious follower" without being decisive.
	DefaultTurnsPointsCap = 4.0

	// DefaultPersistenceMinutesMid is the duration (minutes) at which
	// within_route_persistence first kicks in (+1.0).
	DefaultPersistenceMinutesMid = 8.0

	// DefaultPersistenceMinutesHigh is the duration (minutes) at
	// which within_route_persistence promotes to +1.5 (replaces the
	// mid-tier, does not stack).
	DefaultPersistenceMinutesHigh = 15.0

	// DefaultPersistenceMidPoints is the points awarded when at
	// least one encounter exceeds DefaultPersistenceMinutesMid.
	DefaultPersistenceMidPoints = 1.0

	// DefaultPersistenceHighPoints is the points awarded when at
	// least one encounter exceeds DefaultPersistenceMinutesHigh.
	DefaultPersistenceHighPoints = 1.5

	// DefaultDistinctRoutesMid is the count above which
	// cross_route_count first awards points.
	DefaultDistinctRoutesMid = 3

	// DefaultDistinctRoutesMidPoints is the points for >=Mid distinct
	// routes.
	DefaultDistinctRoutesMidPoints = 1.5

	// DefaultDistinctRoutesHigh is the count at which the additional
	// cross_route_count tier kicks in.
	DefaultDistinctRoutesHigh = 5

	// DefaultDistinctRoutesHighPoints is the additional points for
	// >=High distinct routes (stacks on top of Mid for a total of
	// 2.5).
	DefaultDistinctRoutesHighPoints = 1.0

	// DefaultDistinctAreasMin is the cell-count threshold above which
	// cross_route_geo_spread fires. >=2 distinct cells means the
	// plate has been seen in two different "parts of your life",
	// which is a strong stalking signal.
	DefaultDistinctAreasMin = 2

	// DefaultDistinctAreasPoints is the points awarded when the geo
	// spread threshold is met.
	DefaultDistinctAreasPoints = 2.0

	// DefaultAreaCellKm is the side length of the geo grid cell used
	// for cross_route_geo_spread. 5 km is roughly "different
	// neighbourhood" without being so coarse that the home/work
	// commute is mistakenly counted as a single cell.
	DefaultAreaCellKm = 5.0

	// DefaultTimingWindowHours is the time window used by
	// cross_route_timing: multiple distinct routes within this many
	// hours of each other awards points. 24h is "same-day re-
	// encounter" -- legitimate co-commuters can hit this, so the
	// component is intentionally only +1.0.
	DefaultTimingWindowHours = 24.0

	// DefaultTimingPoints is the points awarded when the timing
	// window threshold is met.
	DefaultTimingPoints = 1.0
)

// Thresholds bundles every numeric knob the heuristic exposes. The
// worker layer fills this from the settings store at evaluation time;
// tests construct it directly.
//
// The defaults (DefaultThresholds) match the constants above. A
// zero-valued Thresholds is meaningful only for tests that explicitly
// want all components disabled.
type Thresholds struct {
	TurnsMin                 int
	TurnsPointsCap           float64
	PersistenceMinutesMid    float64
	PersistenceMinutesHigh   float64
	PersistenceMidPoints     float64
	PersistenceHighPoints    float64
	DistinctRoutesMid        int
	DistinctRoutesMidPoints  float64
	DistinctRoutesHigh       int
	DistinctRoutesHighPoints float64
	DistinctAreasMin         int
	DistinctAreasPoints      float64
	AreaCellKm               float64
	TimingWindowHours        float64
	TimingPoints             float64
}

// DefaultThresholds returns the rule set as documented in the feature
// spec and in the package constants above.
func DefaultThresholds() Thresholds {
	return Thresholds{
		TurnsMin:                 DefaultTurnsMin,
		TurnsPointsCap:           DefaultTurnsPointsCap,
		PersistenceMinutesMid:    DefaultPersistenceMinutesMid,
		PersistenceMinutesHigh:   DefaultPersistenceMinutesHigh,
		PersistenceMidPoints:     DefaultPersistenceMidPoints,
		PersistenceHighPoints:    DefaultPersistenceHighPoints,
		DistinctRoutesMid:        DefaultDistinctRoutesMid,
		DistinctRoutesMidPoints:  DefaultDistinctRoutesMidPoints,
		DistinctRoutesHigh:       DefaultDistinctRoutesHigh,
		DistinctRoutesHighPoints: DefaultDistinctRoutesHighPoints,
		DistinctAreasMin:         DefaultDistinctAreasMin,
		DistinctAreasPoints:      DefaultDistinctAreasPoints,
		AreaCellKm:               DefaultAreaCellKm,
		TimingWindowHours:        DefaultTimingWindowHours,
		TimingPoints:             DefaultTimingPoints,
	}
}

// Encounter is the in-memory shape Score consumes. The worker layer
// builds these from db.PlateEncounter rows joined with the plate's
// first-detection GPS so the scorer has everything it needs without a
// second DB hop.
//
// HasGPS distinguishes "no GPS" from "GPS at lat=0, lng=0". Without it
// the equator-edge test cannot tell a missing fix from a valid one.
type Encounter struct {
	// EncounterID is the database id of the encounter row. Surfaced
	// in component evidence so the UI can deep-link to "the
	// encounter that scored this".
	EncounterID int64

	// Route is the dongle-scoped route identifier (e.g.
	// "abc|2024-05-01--12-34-56"). Surfaced in evidence + is the
	// natural key for distinct-routes counting.
	Route string

	// FirstSeen / LastSeen bound the encounter in wall time. Used
	// for persistence (LastSeen-FirstSeen >= threshold) and for
	// cross_route_timing's "within 24h" check.
	FirstSeen time.Time
	LastSeen  time.Time

	// TurnCount is the number of turns the host vehicle made while
	// this plate was in frame. Used by within_route_turns.
	TurnCount int

	// StartLat / StartLng are the GPS coordinates of the encounter's
	// first detection. Used by cross_route_geo_spread. HasGPS
	// must be true for the values to be considered.
	StartLat float64
	StartLng float64
	HasGPS   bool
}

// ScoringInput is the full input to Score. The worker layer assembles
// it; tests construct it directly.
type ScoringInput struct {
	// PlateHash is the SHA-256 (or otherwise hashed) plate identifier.
	// Carried through to the Result so callers can pass the result to
	// the watchlist UPSERT without re-loading.
	PlateHash []byte

	// RecentEncounters is the encounter list within the lookback
	// window (default 30 days). The order does not matter -- Score
	// re-bucketizes internally.
	RecentEncounters []Encounter

	// Whitelisted is true when the plate's existing watchlist row is
	// of kind 'whitelist'. Whitelisting forces TotalScore=0 for
	// transparency the components are still listed.
	Whitelisted bool

	// Thresholds is the rule set in effect. Tests pass
	// DefaultThresholds(); the worker passes the resolved
	// settings-overlaid values.
	Thresholds Thresholds
}

// Component is one labelled contribution to the total score. Evidence
// captures the raw inputs the rule used so the UI can render an
// audit-quality "why we scored this" explanation. Points is exactly
// what was added to TotalScore (after caps, after whitelist override).
type Component struct {
	Name     string         `json:"name"`
	Points   float64        `json:"points"`
	Evidence map[string]any `json:"evidence"`
}

// Result is the output of Score: the total, the bucket-bounded
// severity, the per-component breakdown, and a one-line human reason.
type Result struct {
	TotalScore float64     `json:"total_score"`
	Severity   int         `json:"severity"`
	Components []Component `json:"components"`
	Reason     string      `json:"reason"`
}

// Component name constants. Stable strings -- the UI keys translations
// off these.
const (
	ComponentWithinRouteTurns       = "within_route_turns"
	ComponentWithinRoutePersistence = "within_route_persistence"
	ComponentCrossRouteCount        = "cross_route_count"
	ComponentCrossRouteGeoSpread    = "cross_route_geo_spread"
	ComponentCrossRouteTiming       = "cross_route_timing"
	ComponentWhitelistSuppression   = "whitelist_suppression"
)

// Score is the pure scoring function. No I/O, no time.Now -- everything
// it needs is in input. A nil RecentEncounters yields TotalScore=0 +
// severity=0 with no components (defensive: the worker should never
// call Score when there is nothing to score, but we don't want to
// panic if it does).
func Score(input ScoringInput) Result {
	t := input.Thresholds
	encs := input.RecentEncounters

	components := make([]Component, 0, 6)

	// (1) within_route_turns -- pick the encounter with the highest
	// turn count, award (turns - TurnsMin) points capped at
	// TurnsPointsCap.
	if c, ok := computeWithinRouteTurns(encs, t); ok {
		components = append(components, c)
	}

	// (2) within_route_persistence -- look at the longest encounter,
	// pick the highest tier its duration crosses (mid OR high; not
	// both).
	if c, ok := computeWithinRoutePersistence(encs, t); ok {
		components = append(components, c)
	}

	// (3) cross_route_count -- count distinct routes; award points
	// at Mid and an additional tier at High.
	if c, ok := computeCrossRouteCount(encs, t); ok {
		components = append(components, c)
	}

	// (4) cross_route_geo_spread -- bucket each encounter's start
	// GPS into a grid cell of size AreaCellKm, count distinct cells,
	// award points if >= DistinctAreasMin.
	if c, ok := computeCrossRouteGeoSpread(encs, t); ok {
		components = append(components, c)
	}

	// (5) cross_route_timing -- check if any two distinct routes are
	// within TimingWindowHours of each other.
	if c, ok := computeCrossRouteTiming(encs, t); ok {
		components = append(components, c)
	}

	// Sum points across components.
	total := 0.0
	for _, c := range components {
		total += c.Points
	}

	// (6) whitelist_suppression -- override AFTER the sum is
	// computed so the components list still records what the rules
	// would have done. The component itself is informational and
	// contributes 0 points (the override happens on TotalScore, not
	// at the component level).
	if input.Whitelisted {
		components = append(components, Component{
			Name:   ComponentWhitelistSuppression,
			Points: 0,
			Evidence: map[string]any{
				"suppressed":     true,
				"would_score":    total,
				"would_severity": severityForScore(total),
			},
		})
		total = 0
	}

	severity := severityForScore(total)
	reason := summarize(components, total, severity, input.Whitelisted)

	return Result{
		TotalScore: total,
		Severity:   severity,
		Components: components,
		Reason:     reason,
	}
}

// computeWithinRouteTurns implements component (1). Returns ok=false
// when no encounter exceeds the TurnsMin threshold (no points to
// award), so the result.Components list stays free of zero-point
// noise. Capped at TurnsPointsCap so a single pathological route
// cannot saturate severity on its own.
func computeWithinRouteTurns(encs []Encounter, t Thresholds) (Component, bool) {
	if len(encs) == 0 {
		return Component{}, false
	}
	bestIdx := -1
	for i, e := range encs {
		if bestIdx < 0 || e.TurnCount > encs[bestIdx].TurnCount {
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return Component{}, false
	}
	turns := encs[bestIdx].TurnCount
	excess := turns - t.TurnsMin
	if excess <= 0 {
		return Component{}, false
	}
	points := float64(excess) * 1.0
	if t.TurnsPointsCap > 0 && points > t.TurnsPointsCap {
		points = t.TurnsPointsCap
	}
	return Component{
		Name:   ComponentWithinRouteTurns,
		Points: points,
		Evidence: map[string]any{
			"turn_count":   turns,
			"route":        encs[bestIdx].Route,
			"encounter_id": encs[bestIdx].EncounterID,
			"turns_min":    t.TurnsMin,
			"capped_at":    t.TurnsPointsCap,
		},
	}, true
}

// computeWithinRoutePersistence implements component (2). The
// duration tiers are mutually exclusive: an encounter long enough for
// the High tier produces only the High points, NOT High + Mid. This
// matches the spec ("replace, don't add").
func computeWithinRoutePersistence(encs []Encounter, t Thresholds) (Component, bool) {
	if len(encs) == 0 {
		return Component{}, false
	}
	// Find the longest encounter that has a usable timestamp pair.
	var bestIdx = -1
	var bestDur time.Duration
	for i, e := range encs {
		if e.FirstSeen.IsZero() || e.LastSeen.IsZero() {
			continue
		}
		dur := e.LastSeen.Sub(e.FirstSeen)
		if dur < 0 {
			// Defensive: a negative duration means the inputs are
			// corrupted. Skip rather than score a phantom signal.
			continue
		}
		if bestIdx < 0 || dur > bestDur {
			bestIdx = i
			bestDur = dur
		}
	}
	if bestIdx < 0 {
		return Component{}, false
	}
	mins := bestDur.Minutes()
	var points float64
	var tier string
	switch {
	case mins >= t.PersistenceMinutesHigh:
		points = t.PersistenceHighPoints
		tier = "high"
	case mins >= t.PersistenceMinutesMid:
		points = t.PersistenceMidPoints
		tier = "mid"
	default:
		return Component{}, false
	}
	return Component{
		Name:   ComponentWithinRoutePersistence,
		Points: points,
		Evidence: map[string]any{
			"duration_minutes": mins,
			"tier":             tier,
			"route":            encs[bestIdx].Route,
			"encounter_id":     encs[bestIdx].EncounterID,
			"mid_minutes":      t.PersistenceMinutesMid,
			"high_minutes":     t.PersistenceMinutesHigh,
		},
	}, true
}

// computeCrossRouteCount implements component (3). Counts distinct
// route ids across all encounters in the input. The Mid tier is
// awarded once at >= DistinctRoutesMid; the High tier STACKS on top
// at >= DistinctRoutesHigh, so 5+ routes total 2.5 points.
func computeCrossRouteCount(encs []Encounter, t Thresholds) (Component, bool) {
	if len(encs) == 0 {
		return Component{}, false
	}
	routes := make(map[string]struct{}, len(encs))
	for _, e := range encs {
		if e.Route == "" {
			continue
		}
		routes[e.Route] = struct{}{}
	}
	count := len(routes)
	if count < t.DistinctRoutesMid {
		return Component{}, false
	}
	points := t.DistinctRoutesMidPoints
	tier := "mid"
	if count >= t.DistinctRoutesHigh {
		points += t.DistinctRoutesHighPoints
		tier = "high"
	}
	return Component{
		Name:   ComponentCrossRouteCount,
		Points: points,
		Evidence: map[string]any{
			"distinct_routes": count,
			"tier":            tier,
			"mid_threshold":   t.DistinctRoutesMid,
			"high_threshold":  t.DistinctRoutesHigh,
		},
	}, true
}

// computeCrossRouteGeoSpread implements component (4). Each
// encounter's start GPS is bucketized to a grid cell of side
// AreaCellKm; if the encounters span >= DistinctAreasMin distinct
// cells, award points.
func computeCrossRouteGeoSpread(encs []Encounter, t Thresholds) (Component, bool) {
	if len(encs) == 0 || t.AreaCellKm <= 0 {
		return Component{}, false
	}
	// Deduplicate by route first: two encounters from the same route
	// share a starting point and would inflate the cell count if both
	// happened to round to the same cell anyway. We want "distinct
	// areas across distinct routes".
	type routeStart struct {
		lat, lng float64
		hasGPS   bool
		route    string
	}
	byRoute := make(map[string]routeStart, len(encs))
	for _, e := range encs {
		if e.Route == "" {
			continue
		}
		if existing, ok := byRoute[e.Route]; ok {
			// Keep the first encounter we saw with GPS; otherwise
			// retain whatever we have.
			if existing.hasGPS {
				continue
			}
		}
		byRoute[e.Route] = routeStart{
			lat: e.StartLat, lng: e.StartLng, hasGPS: e.HasGPS,
			route: e.Route,
		}
	}

	cells := make(map[string]struct{}, len(byRoute))
	gpsRoutes := 0
	for _, rs := range byRoute {
		if !rs.hasGPS {
			continue
		}
		gpsRoutes++
		cells[geoCellKey(rs.lat, rs.lng, t.AreaCellKm)] = struct{}{}
	}
	count := len(cells)
	if count < t.DistinctAreasMin {
		return Component{}, false
	}
	return Component{
		Name:   ComponentCrossRouteGeoSpread,
		Points: t.DistinctAreasPoints,
		Evidence: map[string]any{
			"distinct_cells":  count,
			"routes_with_gps": gpsRoutes,
			"area_cell_km":    t.AreaCellKm,
			"min_threshold":   t.DistinctAreasMin,
		},
	}, true
}

// computeCrossRouteTiming implements component (5). Walks every pair
// of distinct routes and checks whether their first-seen timestamps
// are within TimingWindowHours of each other. We use a sorted-by-time
// sweep for O(n log n) instead of O(n^2): once the gap to the prior
// route exceeds the window, we never revisit it.
func computeCrossRouteTiming(encs []Encounter, t Thresholds) (Component, bool) {
	if len(encs) == 0 || t.TimingWindowHours <= 0 {
		return Component{}, false
	}
	// One representative timestamp per route -- the earliest first-
	// seen for that route. Two encounters of the same route on the
	// same drive share a route id and shouldn't double-count.
	earliestByRoute := make(map[string]time.Time, len(encs))
	for _, e := range encs {
		if e.Route == "" || e.FirstSeen.IsZero() {
			continue
		}
		if cur, ok := earliestByRoute[e.Route]; !ok || e.FirstSeen.Before(cur) {
			earliestByRoute[e.Route] = e.FirstSeen
		}
	}
	if len(earliestByRoute) < 2 {
		return Component{}, false
	}
	type rt struct {
		route string
		ts    time.Time
	}
	rts := make([]rt, 0, len(earliestByRoute))
	for r, ts := range earliestByRoute {
		rts = append(rts, rt{route: r, ts: ts})
	}
	sort.Slice(rts, func(i, j int) bool { return rts[i].ts.Before(rts[j].ts) })

	window := time.Duration(t.TimingWindowHours * float64(time.Hour))
	var pairCount int
	var minGap time.Duration = -1
	for i := 1; i < len(rts); i++ {
		gap := rts[i].ts.Sub(rts[i-1].ts)
		if gap <= window {
			pairCount++
			if minGap < 0 || gap < minGap {
				minGap = gap
			}
		}
	}
	if pairCount == 0 {
		return Component{}, false
	}
	return Component{
		Name:   ComponentCrossRouteTiming,
		Points: t.TimingPoints,
		Evidence: map[string]any{
			"adjacent_pairs_within_window": pairCount,
			"window_hours":                 t.TimingWindowHours,
			"min_gap_minutes":              minGap.Minutes(),
		},
	}, true
}

// severityForScore maps a TotalScore to one of the documented severity
// buckets. The bucket boundaries are inclusive on the lower bound:
// a score of exactly 2.0 produces severity 2, a score of 3.99 stays at
// severity 2, and 4.0 promotes to 3.
//
// Severity 1 is reserved for manual 'note' watchlist entries surfaced
// from the UI; the heuristic never emits it.
func severityForScore(total float64) int {
	switch {
	case total >= 8:
		return 5
	case total >= 6:
		return 4
	case total >= 4:
		return 3
	case total >= 2:
		return 2
	default:
		return 0
	}
}

// summarize produces a one-line human reason. The UI also has the
// component breakdown for the full explanation; this is the at-a-
// glance string the alert badge uses.
func summarize(components []Component, total float64, severity int, whitelisted bool) string {
	if whitelisted {
		return fmt.Sprintf("whitelist suppressed (would have scored %.2f, severity %d)",
			summedWithoutWhitelist(components), severityForScore(summedWithoutWhitelist(components)))
	}
	if severity == 0 {
		if total <= 0 {
			return "no signal"
		}
		return fmt.Sprintf("below alert threshold (score %.2f)", total)
	}
	if len(components) == 0 {
		return fmt.Sprintf("score %.2f, severity %d", total, severity)
	}
	// Pick the top-points component as the headline. Ties resolve to
	// the earliest-listed component so the headline is deterministic.
	headline := components[0]
	for _, c := range components[1:] {
		if c.Points > headline.Points {
			headline = c
		}
	}
	return fmt.Sprintf("severity %d (score %.2f, top %s +%.2f)",
		severity, total, headline.Name, headline.Points)
}

// summedWithoutWhitelist sums points across all components except
// whitelist_suppression. Used by summarize to render "would have
// scored" inside a suppressed alert.
func summedWithoutWhitelist(components []Component) float64 {
	sum := 0.0
	for _, c := range components {
		if c.Name == ComponentWhitelistSuppression {
			continue
		}
		sum += c.Points
	}
	return sum
}

// geoCellKey computes the grid-bucket key for a (lat, lng) pair given
// a cell size in km. The cell is roughly square at the given latitude
// because we apply the cos(lat) correction in the longitude axis.
//
// Edge cases:
//
//   - At the poles, cos(lat) is 0; we clamp lat to [-89.9, 89.9] so
//     the longitude divisor never goes to zero. A plate seen from a
//     pole-bound flight is unlikely to occur in this product, but
//     we still don't panic.
//   - At lng = +/- 180 the bucket boundary lines up cleanly because
//     the grid is keyed off floor(lng / cell), which is well-defined
//     for negative inputs (Go's math.Floor is the mathematical floor,
//     not truncation). Two plates on either side of the date line
//     fall into different buckets; this is intentional -- the
//     stalking heuristic operates at neighbourhood scale, not at a
//     5km circle around the date line.
func geoCellKey(lat, lng, cellKm float64) string {
	if cellKm <= 0 {
		// Defensive: a misconfigured cell size collapses everything
		// to a single bucket. Tests construct inputs with positive
		// cellKm; production threadsthe configured value through.
		return "0|0"
	}
	const kmPerDegLat = 111.32
	cellLatDeg := cellKm / kmPerDegLat

	// Clamp latitude to avoid cos(90 deg) = 0 in the longitude axis.
	clampedLat := lat
	if clampedLat > 89.9 {
		clampedLat = 89.9
	} else if clampedLat < -89.9 {
		clampedLat = -89.9
	}
	cosLat := math.Cos(clampedLat * math.Pi / 180.0)
	// math.Cos can underflow to a tiny but non-zero value near the
	// poles after the clamp; floor that to a sensible minimum so the
	// longitude cell width stays bounded.
	if cosLat < 1e-6 {
		cosLat = 1e-6
	}
	cellLngDeg := cellLatDeg / cosLat

	cellLat := int64(math.Floor(lat / cellLatDeg))
	cellLng := int64(math.Floor(lng / cellLngDeg))
	return fmt.Sprintf("%d|%d", cellLat, cellLng)
}
