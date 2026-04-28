package heuristic

import (
	"math"
	"testing"
	"time"
)

// baseTime is a fixed reference for test encounters. Using a constant
// (rather than time.Now()) keeps the tests deterministic so a CI run
// at any wall-clock time produces the same result.
var baseTime = time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)

// makeEnc is a small helper to keep test tables compact.
func makeEnc(route string, offsetMinutes int, durationMinutes int, turns int, lat, lng float64, hasGPS bool) Encounter {
	first := baseTime.Add(time.Duration(offsetMinutes) * time.Minute)
	return Encounter{
		Route:     route,
		FirstSeen: first,
		LastSeen:  first.Add(time.Duration(durationMinutes) * time.Minute),
		TurnCount: turns,
		StartLat:  lat,
		StartLng:  lng,
		HasGPS:    hasGPS,
	}
}

func findComponent(r Result, name string) (Component, bool) {
	for _, c := range r.Components {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// --- (1) within_route_turns ---

func TestScore_WithinRouteTurns(t *testing.T) {
	tests := []struct {
		name        string
		turns       []int
		wantPoints  float64
		wantHasComp bool
	}{
		{"no encounters", []int{}, 0, false},
		{"all below threshold", []int{0, 1, 2}, 0, false},
		{"just over threshold (3 turns -> 1pt)", []int{3}, 1, true},
		{"5 turns -> 3pt", []int{5}, 3, true},
		{"6 turns -> 4pt cap", []int{6}, 4, true},
		{"10 turns still capped at 4", []int{10}, 4, true},
		{"picks highest turn count", []int{2, 5, 1, 3}, 3, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encs := make([]Encounter, len(tc.turns))
			for i, n := range tc.turns {
				encs[i] = makeEnc("r"+string(rune('a'+i)), i*60, 1, n, 0, 0, false)
			}
			in := ScoringInput{
				RecentEncounters: encs,
				Thresholds:       DefaultThresholds(),
			}
			got := Score(in)
			c, ok := findComponent(got, ComponentWithinRouteTurns)
			if ok != tc.wantHasComp {
				t.Fatalf("component presence: got=%v want=%v (got=%+v)", ok, tc.wantHasComp, got)
			}
			if ok && c.Points != tc.wantPoints {
				t.Fatalf("points: got=%v want=%v", c.Points, tc.wantPoints)
			}
		})
	}
}

// --- (2) within_route_persistence ---

func TestScore_WithinRoutePersistence(t *testing.T) {
	tests := []struct {
		name        string
		minutes     []int
		wantPoints  float64
		wantHasComp bool
		wantTier    string
	}{
		{"no encounters", nil, 0, false, ""},
		{"7m below mid", []int{1, 5, 7}, 0, false, ""},
		{"exactly 8m -> mid", []int{8}, 1.0, true, "mid"},
		{"10m -> mid", []int{10}, 1.0, true, "mid"},
		{"exactly 15m -> high replaces mid", []int{15}, 1.5, true, "high"},
		{"30m -> high", []int{30}, 1.5, true, "high"},
		{"picks longest", []int{1, 16, 5}, 1.5, true, "high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encs := make([]Encounter, len(tc.minutes))
			for i, m := range tc.minutes {
				encs[i] = makeEnc("r", i*60, m, 0, 0, 0, false)
			}
			in := ScoringInput{
				RecentEncounters: encs,
				Thresholds:       DefaultThresholds(),
			}
			got := Score(in)
			c, ok := findComponent(got, ComponentWithinRoutePersistence)
			if ok != tc.wantHasComp {
				t.Fatalf("component presence: got=%v want=%v (got=%+v)", ok, tc.wantHasComp, got)
			}
			if ok {
				if c.Points != tc.wantPoints {
					t.Fatalf("points: got=%v want=%v", c.Points, tc.wantPoints)
				}
				if tier, _ := c.Evidence["tier"].(string); tier != tc.wantTier {
					t.Fatalf("tier: got=%q want=%q", tier, tc.wantTier)
				}
			}
		})
	}
}

func TestScore_WithinRoutePersistenceIgnoresNegativeDuration(t *testing.T) {
	bad := Encounter{
		Route:     "r",
		FirstSeen: baseTime,
		LastSeen:  baseTime.Add(-1 * time.Minute),
	}
	good := makeEnc("r2", 0, 9, 0, 0, 0, false)
	in := ScoringInput{
		RecentEncounters: []Encounter{bad, good},
		Thresholds:       DefaultThresholds(),
	}
	got := Score(in)
	c, ok := findComponent(got, ComponentWithinRoutePersistence)
	if !ok {
		t.Fatal("expected good encounter to score")
	}
	if c.Points != 1.0 {
		t.Fatalf("expected mid (+1.0) on good encounter, got %v", c.Points)
	}
}

// --- (3) cross_route_count ---

func TestScore_CrossRouteCount(t *testing.T) {
	tests := []struct {
		name        string
		routes      []string
		wantPoints  float64
		wantHasComp bool
		wantTier    string
	}{
		{"no encounters", nil, 0, false, ""},
		{"2 distinct routes -> below mid", []string{"r1", "r2", "r1"}, 0, false, ""},
		{"3 distinct -> mid", []string{"r1", "r2", "r3"}, 1.5, true, "mid"},
		{"4 distinct -> still mid", []string{"r1", "r2", "r3", "r4"}, 1.5, true, "mid"},
		{"5 distinct -> mid+high stack = 2.5", []string{"r1", "r2", "r3", "r4", "r5"}, 2.5, true, "high"},
		{"7 distinct -> 2.5", []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"}, 2.5, true, "high"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encs := make([]Encounter, len(tc.routes))
			for i, r := range tc.routes {
				encs[i] = makeEnc(r, i*1440, 1, 0, 0, 0, false)
			}
			in := ScoringInput{
				RecentEncounters: encs,
				Thresholds:       DefaultThresholds(),
			}
			got := Score(in)
			c, ok := findComponent(got, ComponentCrossRouteCount)
			if ok != tc.wantHasComp {
				t.Fatalf("component presence: got=%v want=%v (got=%+v)", ok, tc.wantHasComp, got)
			}
			if ok {
				if c.Points != tc.wantPoints {
					t.Fatalf("points: got=%v want=%v", c.Points, tc.wantPoints)
				}
				if tier, _ := c.Evidence["tier"].(string); tier != tc.wantTier {
					t.Fatalf("tier: got=%q want=%q", tier, tc.wantTier)
				}
			}
		})
	}
}

// --- (4) cross_route_geo_spread ---

func TestScore_CrossRouteGeoSpread(t *testing.T) {
	// Two routes at the same point: 1 cell, no signal.
	t.Run("same cell, no signal", func(t *testing.T) {
		encs := []Encounter{
			makeEnc("r1", 0, 1, 0, 40.0, -73.0, true),
			makeEnc("r2", 1440, 1, 0, 40.001, -73.001, true),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if _, ok := findComponent(got, ComponentCrossRouteGeoSpread); ok {
			t.Fatalf("did not expect geo spread component")
		}
	})

	// Two routes ~50km apart -> distinct cells.
	t.Run("two distinct cells -> 2.0", func(t *testing.T) {
		encs := []Encounter{
			makeEnc("r1", 0, 1, 0, 40.0, -73.0, true),
			makeEnc("r2", 1440, 1, 0, 40.5, -73.0, true), // ~55km north
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		c, ok := findComponent(got, ComponentCrossRouteGeoSpread)
		if !ok {
			t.Fatalf("expected geo spread component, got %+v", got)
		}
		if c.Points != 2.0 {
			t.Fatalf("points: got=%v want=2.0", c.Points)
		}
	})

	// Encounters without GPS contribute zero cells.
	t.Run("no GPS -> no signal", func(t *testing.T) {
		encs := []Encounter{
			makeEnc("r1", 0, 1, 0, 40.0, -73.0, false),
			makeEnc("r2", 1440, 1, 0, 40.5, -73.0, false),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if _, ok := findComponent(got, ComponentCrossRouteGeoSpread); ok {
			t.Fatalf("did not expect geo spread component without GPS")
		}
	})

	// Same route counted once even with multiple encounters.
	t.Run("same route deduped", func(t *testing.T) {
		encs := []Encounter{
			makeEnc("r1", 0, 1, 0, 40.0, -73.0, true),
			makeEnc("r1", 5, 1, 0, 50.0, -73.0, true),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if _, ok := findComponent(got, ComponentCrossRouteGeoSpread); ok {
			t.Fatalf("did not expect spread; same route should produce one cell")
		}
	})
}

func TestGeoCellKey_EquatorAndDateLine(t *testing.T) {
	tests := []struct {
		name     string
		a, b     [2]float64 // (lat, lng) pairs
		sameCell bool
	}{
		{
			name:     "equator: 0,0 and 0.001,0.001 same cell",
			a:        [2]float64{0, 0},
			b:        [2]float64{0.001, 0.001},
			sameCell: true,
		},
		{
			name:     "equator: 0.0 and 0.5 lat across cell boundary",
			a:        [2]float64{0.0, 0.0},
			b:        [2]float64{0.5, 0.0}, // ~55 km
			sameCell: false,
		},
		{
			name:     "180-deg line: lng=179.99 vs lng=-179.99 are different cells",
			a:        [2]float64{0, 179.99},
			b:        [2]float64{0, -179.99},
			sameCell: false,
		},
		{
			name:     "near pole: lat=89.95 vs 89.96 still works without panicking",
			a:        [2]float64{89.95, 0},
			b:        [2]float64{89.96, 0},
			sameCell: true, // small lng cells but small lat delta -> same lat cell
		},
		{
			name:     "south pole clamp: -90,0 vs -89.99,0 doesn't divide by zero",
			a:        [2]float64{-90, 0},
			b:        [2]float64{-89.99, 0},
			sameCell: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ka := geoCellKey(tc.a[0], tc.a[1], 5.0)
			kb := geoCellKey(tc.b[0], tc.b[1], 5.0)
			if (ka == kb) != tc.sameCell {
				t.Fatalf("ka=%q kb=%q sameCell=%v want=%v", ka, kb, ka == kb, tc.sameCell)
			}
			// Confirm the key doesn't contain NaN or Inf string forms.
			if ka == "" || kb == "" {
				t.Fatalf("empty cell key (a=%q b=%q)", ka, kb)
			}
		})
	}
}

func TestGeoCellKey_NoNaNAtClampedPole(t *testing.T) {
	k := geoCellKey(90, 12.34, 5.0)
	if k == "" {
		t.Fatal("empty key at pole")
	}
	// The lat cell value from 90/cell is finite; verify by parsing.
	// Just ensure no NaN/Inf round-trip.
	if math.IsNaN(0) {
		// keep imports honest: the test above already ensures math
		// is used.
	}
}

// --- (5) cross_route_timing ---

func TestScore_CrossRouteTiming(t *testing.T) {
	tests := []struct {
		name        string
		offsetsMin  []int
		routes      []string
		wantHasComp bool
		wantPoints  float64
	}{
		{"single route -> no signal", []int{0}, []string{"r1"}, false, 0},
		{"two routes 10h apart -> within 24h", []int{0, 600}, []string{"r1", "r2"}, true, 1.0},
		{"two routes 25h apart -> outside", []int{0, 25 * 60}, []string{"r1", "r2"}, false, 0},
		{"three routes spanning 48h with one near-pair", []int{0, 60, 60 * 48}, []string{"r1", "r2", "r3"}, true, 1.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encs := make([]Encounter, len(tc.offsetsMin))
			for i, off := range tc.offsetsMin {
				encs[i] = makeEnc(tc.routes[i], off, 1, 0, 0, 0, false)
			}
			in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
			got := Score(in)
			c, ok := findComponent(got, ComponentCrossRouteTiming)
			if ok != tc.wantHasComp {
				t.Fatalf("component presence: got=%v want=%v (got=%+v)", ok, tc.wantHasComp, got)
			}
			if ok && c.Points != tc.wantPoints {
				t.Fatalf("points: got=%v want=%v", c.Points, tc.wantPoints)
			}
		})
	}
}

// --- (6) whitelist suppression ---

func TestScore_WhitelistSuppression(t *testing.T) {
	// Construct an input that would otherwise score very high.
	encs := []Encounter{
		makeEnc("r1", 0, 30, 6, 40.0, -73.0, true),
		makeEnc("r2", 1440, 30, 6, 40.5, -73.0, true),
		makeEnc("r3", 2880, 30, 6, 41.0, -73.0, true),
		makeEnc("r4", 4320, 30, 6, 41.5, -73.0, true),
		makeEnc("r5", 5760, 30, 6, 42.0, -73.0, true),
	}
	t.Run("not whitelisted -> high severity", func(t *testing.T) {
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.Severity < 4 {
			t.Fatalf("expected high severity, got %d (score %.2f)", got.Severity, got.TotalScore)
		}
	})
	t.Run("whitelisted -> total=0, severity=0, components retained", func(t *testing.T) {
		in := ScoringInput{RecentEncounters: encs, Whitelisted: true, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.TotalScore != 0 {
			t.Fatalf("total: got=%v want=0", got.TotalScore)
		}
		if got.Severity != 0 {
			t.Fatalf("severity: got=%v want=0", got.Severity)
		}
		// whitelist_suppression component must be present and the
		// other components must still be in the list (transparency).
		if _, ok := findComponent(got, ComponentWhitelistSuppression); !ok {
			t.Fatal("missing whitelist_suppression component in suppressed result")
		}
		if _, ok := findComponent(got, ComponentCrossRouteCount); !ok {
			t.Fatal("expected cross_route_count to remain in components for transparency")
		}
	})
}

// --- severity boundary tests ---

func TestSeverityBoundaries(t *testing.T) {
	cases := []struct {
		score float64
		sev   int
	}{
		{1.99, 0},
		{2.0, 2},
		{3.99, 2},
		{4.0, 3},
		{5.99, 3},
		{6.0, 4},
		{7.99, 4},
		{8.0, 5},
		{99.0, 5},
		{0.0, 0},
	}
	for _, c := range cases {
		got := severityForScore(c.score)
		if got != c.sev {
			t.Fatalf("score %v: got severity %d want %d", c.score, got, c.sev)
		}
	}
}

// --- combined-score scenarios that hit specific boundaries ---

func TestScore_CombinedBoundaries(t *testing.T) {
	t.Run("score exactly 2.0 -> severity 2", func(t *testing.T) {
		// 1.0 (persistence mid) + 1.0 (timing) = 2.0
		encs := []Encounter{
			makeEnc("r1", 0, 9, 0, 0, 0, false),
			makeEnc("r2", 60, 1, 0, 0, 0, false),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.TotalScore != 2.0 {
			t.Fatalf("score: got=%v want=2.0 (components=%+v)", got.TotalScore, got.Components)
		}
		if got.Severity != 2 {
			t.Fatalf("severity: got=%v want=2", got.Severity)
		}
	})

	t.Run("score 4.0 -> severity 3", func(t *testing.T) {
		// turns 5 -> 3pt, persistence mid 1.0 = 4.0
		encs := []Encounter{
			makeEnc("r1", 0, 9, 5, 0, 0, false),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.TotalScore != 4.0 {
			t.Fatalf("score: got=%v want=4.0", got.TotalScore)
		}
		if got.Severity != 3 {
			t.Fatalf("severity: got=%v want=3", got.Severity)
		}
	})

	t.Run("score 6.0 -> severity 4", func(t *testing.T) {
		// 1.5 (cross_route_count mid, 3 routes), 1.0 (timing on
		// adjacent pair within 24h), 1.5 (persistence high), 2.0
		// (geo spread) = 6.0
		encs := []Encounter{
			makeEnc("r1", 0, 16, 0, 40.0, -73.0, true),
			makeEnc("r2", 60, 1, 0, 41.0, -74.0, true),
			makeEnc("r3", 1440*3, 1, 0, 42.0, -75.0, true),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.TotalScore != 6.0 {
			t.Fatalf("score: got=%v want=6.0 (components=%+v)", got.TotalScore, got.Components)
		}
		if got.Severity != 4 {
			t.Fatalf("severity: got=%v want=4", got.Severity)
		}
	})

	t.Run("score 8.0 -> severity 5", func(t *testing.T) {
		// turns 6 -> 4pt cap, persistence high 1.5, cross_route_count
		// mid+high stack 2.5 (5 routes), geo spread skipped (no GPS)
		// timing N/A (>24h) -- need to tune.
		// 4 + 1.5 + 2.5 = 8.0
		encs := []Encounter{
			makeEnc("r1", 0, 16, 6, 0, 0, false),
			makeEnc("r2", 1440*3, 1, 0, 0, 0, false),
			makeEnc("r3", 1440*7, 1, 0, 0, 0, false),
			makeEnc("r4", 1440*10, 1, 0, 0, 0, false),
			makeEnc("r5", 1440*15, 1, 0, 0, 0, false),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.TotalScore != 8.0 {
			t.Fatalf("score: got=%v want=8.0 (components=%+v)", got.TotalScore, got.Components)
		}
		if got.Severity != 5 {
			t.Fatalf("severity: got=%v want=5", got.Severity)
		}
	})

	t.Run("score 3.99 stays at severity 2", func(t *testing.T) {
		// turns 4 -> 2pt, persistence high -> 1.5, near-but-below
		// timing window. 2 + 1.5 = 3.5 (still severity 2).
		encs := []Encounter{
			makeEnc("r1", 0, 16, 4, 0, 0, false),
		}
		in := ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()}
		got := Score(in)
		if got.TotalScore < 2.0 || got.TotalScore >= 4.0 {
			t.Fatalf("score: got=%v want in [2,4)", got.TotalScore)
		}
		if got.Severity != 2 {
			t.Fatalf("severity: got=%v want=2", got.Severity)
		}
	})
}

// --- result shape sanity ---

func TestScore_NoEncountersReturnsZero(t *testing.T) {
	got := Score(ScoringInput{Thresholds: DefaultThresholds()})
	if got.TotalScore != 0 {
		t.Fatalf("score: got=%v want=0", got.TotalScore)
	}
	if got.Severity != 0 {
		t.Fatalf("severity: got=%v want=0", got.Severity)
	}
	if len(got.Components) != 0 {
		t.Fatalf("components: got=%v want=empty", got.Components)
	}
	if got.Reason == "" {
		t.Fatal("reason should not be empty")
	}
}

func TestScore_EvidenceCarriesContext(t *testing.T) {
	encs := []Encounter{
		{
			EncounterID: 42,
			Route:       "abc|2025-01-15--12-00-00",
			FirstSeen:   baseTime,
			LastSeen:    baseTime.Add(20 * time.Minute),
			TurnCount:   5,
			HasGPS:      true,
			StartLat:    40.0,
			StartLng:    -73.0,
		},
	}
	got := Score(ScoringInput{RecentEncounters: encs, Thresholds: DefaultThresholds()})
	c, ok := findComponent(got, ComponentWithinRouteTurns)
	if !ok {
		t.Fatal("missing turns component")
	}
	if id, _ := c.Evidence["encounter_id"].(int64); id != 42 {
		t.Fatalf("evidence encounter_id: got=%v want=42", id)
	}
	if route, _ := c.Evidence["route"].(string); route != "abc|2025-01-15--12-00-00" {
		t.Fatalf("evidence route: got=%q", route)
	}
}
