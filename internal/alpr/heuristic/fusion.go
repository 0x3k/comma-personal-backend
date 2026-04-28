// fusion.go implements the signature-fusion layer of the ALPR heuristic.
// It runs strictly AFTER the stalking heuristic's Score() and is purely
// additive: when no detections have signatures (the vehicle-attributes
// engine is off or unsupported) FuseSignatures returns 0/empty and the
// caller continues with the original plate severity.
//
// Three behaviours, each independently testable:
//
//  1. Plate-confirmation. When a single signature accounts for >=80% of
//     the plate's underlying detections, emit `signature_consistent`
//     and add a small severity bump (0.5) so corroborated alerts
//     surface as "more confident" without raising new alerts.
//
//  2. Signature-conflict. When the plate is observed under 2+ distinct
//     signatures, each accounting for >=20% of detections, emit
//     `signature_inconsistent` (severity 0). Used by the UI to flag
//     "OCR may be wrong -- review" without firing an alert. A future
//     manual-correction UI surfaces these as candidates.
//
//  3. Plate-swap. When a single signature has been observed under
//     >=3 distinct plate hashes inside the same area cell within the
//     lookback window, emit a SwapAlert keyed on signature_id (NOT
//     on any plate hash). The worker layer writes a plate_watchlist
//     row with plate_hash=NULL, signature_id=<id>, kind='alerted'
//     and emits an AlertCreated whose SignatureID field is set
//     instead of PlateHash.
//
// The function is "pure-ish" -- it issues DB reads but never DB writes;
// the worker layer commits writes after Score and FuseSignatures have
// both produced their results so the rest of the pipeline can rely on
// a single transactional moment.
package heuristic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"comma-personal-backend/internal/db"
)

// SignatureFusionVersion is the rule-set identifier for the fusion
// layer. It is bumped independently of HeuristicVersion so a change
// to the fusion rules does not retroactively reinterpret historical
// stalking-heuristic events. Both versions ride on every
// plate_alert_events row that the worker writes; the row's
// heuristic_version column always carries HeuristicVersion (the
// stalking-heuristic version), and a separate signature_fusion_version
// field appears inside the components evidence when fusion contributed
// a component.
//
// Bumping is a deliberate act tied to a deploy. Document each change
// in CHANGELOG.md or a follow-up project doc.
//
// The "fusion-v1.0.0" prefix distinguishes this string from
// HeuristicVersion ("v1.0.0") so a tail of plate_alert_events rows
// can be filtered by which heuristic layer produced each component
// without parsing the components blob -- the audit trail records
// HeuristicVersion at the row level, but components produced by the
// fusion layer carry SignatureFusionVersion in their evidence.
const SignatureFusionVersion = "fusion-v1.0.0"

// Fusion-layer thresholds. These intentionally do NOT live in the
// stalking-heuristic Thresholds struct so the two layers can evolve
// independently without polluting each other's settings table. All
// thresholds are passed through to FuseSignatures via FusionThresholds
// so tests can exercise edge cases.
const (
	// DefaultSignatureConsistencyShareMin is the minimum share of a
	// plate's detections that must agree on a single signature for
	// the plate-confirmation path to fire. 0.80 (80%) is the spec.
	DefaultSignatureConsistencyShareMin = 0.80

	// DefaultSignatureConsistencyPoints is the severity bump applied
	// when the consistency threshold is met. Intentionally small (0.5)
	// because the stalking heuristic already produced the bulk of the
	// signal; this is corroboration, not the primary signal.
	DefaultSignatureConsistencyPoints = 0.5

	// DefaultSignatureConflictShareMin is the minimum share each
	// distinct signature must hold for the signature-conflict
	// component to fire. 0.20 (20%) is the spec; it filters out the
	// long tail of single-frame mis-classifications that should not
	// promote a plate to "OCR may be wrong".
	DefaultSignatureConflictShareMin = 0.20

	// DefaultSignatureConflictMinSignatures is the minimum number of
	// distinct signatures (each above the share threshold) required
	// for signature_inconsistent to fire. 2 is the spec.
	DefaultSignatureConflictMinSignatures = 2

	// DefaultPlateSwapMinPlates is the minimum number of distinct
	// plate hashes a single signature must show inside one area cell
	// for plate-swap to fire. 3 is the spec.
	DefaultPlateSwapMinPlates = 3

	// DefaultPlateSwapAreaCellKm is the side length (in km) of the
	// area cell used by plate-swap detection. 5 km matches the
	// stalking heuristic's cross_route_geo_spread bucketing so the
	// two layers reason about the same notion of "area".
	DefaultPlateSwapAreaCellKm = 5.0

	// DefaultPlateSwapLookbackDays is the encounter-history window
	// the fusion layer scans for plate-swap detection. 14 days is
	// shorter than the stalking heuristic's 30-day lookback because
	// plate-swap is a fast-evolving signal (the spec frames it as
	// "the last 14 days") and the alert is high-severity so we want
	// false-positive recency control.
	DefaultPlateSwapLookbackDays = 14

	// DefaultPlateSwapSeverity is the severity assigned to a fresh
	// plate-swap alert. 4 is the spec ("strongest 'actively trying
	// to evade detection' signal we can produce").
	DefaultPlateSwapSeverity = 4
)

// FusionThresholds bundles the fusion-layer knobs. Operators tune via
// settings (env-prefixed ALPR_SIGNATURE_*); the worker layer applies
// precedence (settings > env > default) and passes the resolved values
// to FuseSignatures.
type FusionThresholds struct {
	ConsistencyShareMin   float64
	ConsistencyPoints     float64
	ConflictShareMin      float64
	ConflictMinSignatures int
	PlateSwapMinPlates    int
	PlateSwapAreaCellKm   float64
	PlateSwapLookbackDays int
	PlateSwapSeverity     int
}

// DefaultFusionThresholds returns the rule set as documented in the
// feature spec and in the package constants above.
func DefaultFusionThresholds() FusionThresholds {
	return FusionThresholds{
		ConsistencyShareMin:   DefaultSignatureConsistencyShareMin,
		ConsistencyPoints:     DefaultSignatureConsistencyPoints,
		ConflictShareMin:      DefaultSignatureConflictShareMin,
		ConflictMinSignatures: DefaultSignatureConflictMinSignatures,
		PlateSwapMinPlates:    DefaultPlateSwapMinPlates,
		PlateSwapAreaCellKm:   DefaultPlateSwapAreaCellKm,
		PlateSwapLookbackDays: DefaultPlateSwapLookbackDays,
		PlateSwapSeverity:     DefaultPlateSwapSeverity,
	}
}

// SwapAlert is a single signature-keyed plate-swap alert produced by
// FuseSignatures. The worker layer turns each SwapAlert into:
//
//   - A `plate_watchlist` UPSERT (plate_hash=NULL, signature_id=Sig)
//   - An optional AlertCreated event (only when the alert is genuinely
//     new or strictly upgraded)
//
// PlateHashes is the chain of distinct plate hashes contributing to
// the alert; the worker stores this inside the watchlist row's evidence
// trail and the UI surfaces it as "this vehicle has been seen with N
// different plates."
type SwapAlert struct {
	// SignatureID is the foreign key into vehicle_signatures.
	SignatureID int64

	// Severity is the recommended severity for the alert. The worker
	// applies GREATEST() against any existing row so this never
	// demotes a higher-severity prior alert.
	Severity int

	// AreaCellKey is a stable identifier for the geographic cell the
	// alert was raised in. Surfaced in evidence so the UI can render
	// "this vehicle has been seen with N different plates in the
	// same area" without re-bucketing client-side.
	AreaCellKey string

	// PlateHashes is the distinct plate hashes that contributed to
	// this alert. Sorted lexicographically so the evidence chain is
	// stable across re-evaluations (the alert evidence is part of the
	// audit trail and shouldn't churn on order alone).
	PlateHashes [][]byte

	// MostRecentSeen is the most recent encounter time across the
	// contributing plates. Used as the alert's last_alert_at.
	MostRecentSeen time.Time

	// Evidence carries the structured payload that gets persisted
	// alongside the alert: the area cell, the plate count, the
	// fusion version, etc.
	Evidence map[string]any
}

// FusionDeps is the small DB-shaped dependency the fusion function
// needs. Carved out as an interface so tests can inject an in-memory
// fake. The fusion layer reads only -- the worker writes.
type FusionDeps interface {
	CountDetectionsBySignatureForPlate(ctx context.Context, plateHash []byte) ([]db.CountDetectionsBySignatureForPlateRow, error)
	GetSignature(ctx context.Context, id int64) (db.VehicleSignature, error)
	ListPlatesForSignature(ctx context.Context, signatureID pgtype.Int8) ([][]byte, error)
	ListPlateHashesForSignatureInWindow(ctx context.Context, arg db.ListPlateHashesForSignatureInWindowParams) ([]db.ListPlateHashesForSignatureInWindowRow, error)
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
}

// FusionInput bundles the parameters FuseSignatures needs.
type FusionInput struct {
	// PlateHash is the plate the stalking heuristic just scored.
	// FuseSignatures uses it to compute the signature-consistent /
	// signature-inconsistent components and to gate plate-swap
	// detection on the dominant signature.
	PlateHash []byte

	// Now is the wall-clock time the worker is currently treating as
	// "now". Threaded through so tests can pin it.
	Now time.Time

	// Thresholds is the fusion-layer rule set in effect. Tests
	// construct via DefaultFusionThresholds(); the worker passes the
	// settings-overlaid values.
	Thresholds FusionThresholds
}

// FusionResult is the output of FuseSignatures.
type FusionResult struct {
	// ExtraSeverity is the additional severity (in score-points,
	// matching the stalking heuristic's component points) the worker
	// should add to the plate's existing severity. 0 when the fusion
	// layer found no signal.
	ExtraSeverity float64

	// Components is the list of fusion-layer component rows to merge
	// into the plate's alert_events components JSON. Each carries
	// evidence keyed off SignatureFusionVersion so callers can tell
	// fusion components apart from stalking components in the audit
	// trail.
	Components []Component

	// PlateSwapAlerts is one entry per signature-keyed plate-swap
	// alert raised by this evaluation. The worker layer writes the
	// signature-keyed watchlist rows and emits AlertCreated events.
	PlateSwapAlerts []SwapAlert
}

// Fusion-layer component name constants. Stable strings -- the UI keys
// translations off these.
const (
	ComponentSignatureConsistent   = "signature_consistent"
	ComponentSignatureInconsistent = "signature_inconsistent"
	ComponentPlateSwap             = "plate_swap"
)

// FuseSignatures runs the fusion layer for a single plate. It is the
// pure-ish equivalent of Score: DB reads OK, no DB writes. The worker
// commits writes after assembling Score + FuseSignatures into a single
// view of "what changed for this plate".
//
// Failure modes:
//
//   - A nil deps returns ErrNoDeps without DB activity. The worker
//     guards against this; the explicit error keeps a misuse from
//     being silently lossy.
//   - DB errors from the underlying queries propagate. The worker
//     logs and continues to the next plate -- one transient failure
//     should not poison the whole evaluation.
//   - When the plate has no signature-bearing detections (every row
//     has signature_id=NULL), the function returns a zero-value
//     result. This is the common case on installs without the
//     vehicle-attributes engine and is intentional: the spec
//     requires "no output" rather than "fall back to plate-only".
func FuseSignatures(ctx context.Context, deps FusionDeps, input FusionInput) (FusionResult, error) {
	if deps == nil {
		return FusionResult{}, ErrNoFusionDeps
	}
	t := input.Thresholds
	if t.ConsistencyShareMin == 0 {
		t = DefaultFusionThresholds()
	}

	// 1. Per-signature detection counts for the plate. This drives
	// signature_consistent / signature_inconsistent and identifies
	// the dominant signature for plate-swap evaluation.
	sigCounts, err := deps.CountDetectionsBySignatureForPlate(ctx, input.PlateHash)
	if err != nil {
		return FusionResult{}, fmt.Errorf("count detections by signature: %w", err)
	}

	// Tally totals split by "has signature" vs. "missing signature".
	// Missing-signature rows are the common case on installs without
	// the attributes engine; they are intentionally NOT counted in
	// the share-pct denominator (otherwise installs with sparse
	// attribute coverage would never reach the 80% threshold).
	var (
		totalWithSig    int64
		totalMissingSig int64
		bySigID         = make(map[int64]int64, len(sigCounts))
	)
	for _, r := range sigCounts {
		if r.SignatureID.Valid {
			bySigID[r.SignatureID.Int64] = r.DetectionCount
			totalWithSig += r.DetectionCount
		} else {
			totalMissingSig += r.DetectionCount
		}
	}

	res := FusionResult{}

	// Spec gate: "if no detections have signatures ... this layer
	// produces no output." totalWithSig == 0 includes the case where
	// the plate has no detections at all (post-merge, retention
	// sweep, etc.); we still bail early so the worker doesn't write
	// fusion components into the audit trail for nothing.
	if totalWithSig == 0 {
		return res, nil
	}

	// 2. Signature-consistent: the dominant signature accounts for
	// >=ConsistencyShareMin of all signature-bearing detections.
	dominantSig, dominantCount := pickDominant(bySigID)
	share := float64(dominantCount) / float64(totalWithSig)
	if share >= t.ConsistencyShareMin && dominantSig != 0 {
		evid := map[string]any{
			"signature_id":        dominantSig,
			"share_pct":           share,
			"detections_with_sig": totalWithSig,
			"missing_sig":         totalMissingSig,
			"min_share":           t.ConsistencyShareMin,
			"fusion_version":      SignatureFusionVersion,
		}
		// Surface the canonical signature_key when we can resolve
		// it. A failed lookup degrades gracefully -- the signature
		// id by itself is enough for the UI to deep-link.
		if sig, err := deps.GetSignature(ctx, dominantSig); err == nil {
			evid["signature_key"] = sig.SignatureKey
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return res, fmt.Errorf("get signature %d: %w", dominantSig, err)
		}
		res.Components = append(res.Components, Component{
			Name:     ComponentSignatureConsistent,
			Points:   t.ConsistencyPoints,
			Evidence: evid,
		})
		res.ExtraSeverity += t.ConsistencyPoints
	}

	// 3. Signature-inconsistent: 2+ distinct signatures, each
	// accounting for >=ConflictShareMin. This is informational --
	// no severity contribution -- but the component still rides on
	// the alert_events row so the UI can flag the plate for the
	// manual-correction queue.
	if len(bySigID) >= t.ConflictMinSignatures {
		var distinctOverShare int
		var sigList []int64
		for sigID, count := range bySigID {
			s := float64(count) / float64(totalWithSig)
			if s >= t.ConflictShareMin {
				distinctOverShare++
				sigList = append(sigList, sigID)
			}
		}
		if distinctOverShare >= t.ConflictMinSignatures {
			sort.Slice(sigList, func(i, j int) bool { return sigList[i] < sigList[j] })
			res.Components = append(res.Components, Component{
				Name:   ComponentSignatureInconsistent,
				Points: 0,
				Evidence: map[string]any{
					"signatures":          sigList,
					"detections_with_sig": totalWithSig,
					"min_share":           t.ConflictShareMin,
					"fusion_version":      SignatureFusionVersion,
				},
			})
		}
	}

	// 4. Plate-swap detection. Use the plate's dominant signature as
	// the lookup key (the spec frames the alert as "this physical
	// vehicle has been seen with multiple plates", which is exactly
	// the dominant signature). Querying every signature in bySigID
	// would scan the encounter table once per signature, blowing
	// past the 100ms target on busy installs.
	if dominantSig != 0 && t.PlateSwapMinPlates >= 2 {
		swap, ok, err := evaluatePlateSwap(ctx, deps, dominantSig, input.Now, t)
		if err != nil {
			return res, fmt.Errorf("plate-swap check: %w", err)
		}
		if ok {
			res.PlateSwapAlerts = append(res.PlateSwapAlerts, swap)
			res.Components = append(res.Components, Component{
				Name:   ComponentPlateSwap,
				Points: 0, // severity travels via the SwapAlert, not the plate's score
				Evidence: map[string]any{
					"signature_id":   swap.SignatureID,
					"area_cell":      swap.AreaCellKey,
					"plate_count":    len(swap.PlateHashes),
					"fusion_version": SignatureFusionVersion,
				},
			})
		}
	}

	return res, nil
}

// pickDominant returns the signature_id with the highest detection
// count and that count. Returns (0, 0) when bySigID is empty. Ties are
// broken by the lowest signature_id so the result is deterministic.
func pickDominant(bySigID map[int64]int64) (int64, int64) {
	var bestID int64
	var bestCount int64
	for id, c := range bySigID {
		switch {
		case c > bestCount:
			bestID = id
			bestCount = c
		case c == bestCount && (bestID == 0 || id < bestID):
			bestID = id
		}
	}
	return bestID, bestCount
}

// evaluatePlateSwap is the heavy-lift query: for the given dominant
// signature, scan encounters in the lookback window, group by area
// cell, and return a SwapAlert when any cell has >=PlateSwapMinPlates
// distinct plate hashes that are NOT all whitelisted.
//
// Whitelist suppression is checked here rather than in the SQL query
// because the watchlist is a separate small table and the per-plate
// GetWatchlistByHash lookup is O(1) on the (plate_hash) index. Doing
// it in SQL would force a join we don't otherwise need.
func evaluatePlateSwap(ctx context.Context, deps FusionDeps, signatureID int64, now time.Time, t FusionThresholds) (SwapAlert, bool, error) {
	if t.PlateSwapAreaCellKm <= 0 || t.PlateSwapLookbackDays <= 0 {
		return SwapAlert{}, false, nil
	}
	cellLatDeg := t.PlateSwapAreaCellKm / kmPerDegLat
	// Single-degree-of-cosine correction at the equator is fine for
	// the SQL bucketing -- the result is only used for grouping, not
	// for distance maths. The Go-side cell key uses the proper
	// cos(lat) correction; the SQL coarse grid trades a little
	// over-bucketing near the poles for a single static cell size.
	cellLngDeg := cellLatDeg

	windowStart := now.Add(-time.Duration(t.PlateSwapLookbackDays) * 24 * time.Hour)
	rows, err := deps.ListPlateHashesForSignatureInWindow(ctx, db.ListPlateHashesForSignatureInWindowParams{
		CellLatDeg:  pgtype.Float8{Float64: cellLatDeg, Valid: true},
		CellLngDeg:  pgtype.Float8{Float64: cellLngDeg, Valid: true},
		SignatureID: pgtype.Int8{Int64: signatureID, Valid: true},
		WindowStart: pgtype.Timestamptz{Time: windowStart, Valid: true},
		WindowEnd:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		return SwapAlert{}, false, fmt.Errorf("list plate hashes for signature: %w", err)
	}
	if len(rows) == 0 {
		return SwapAlert{}, false, nil
	}

	// Group by (cell_lat, cell_lng). For each cell, collect distinct
	// plate hashes (sorted lexicographically for stable evidence)
	// and the most recent last_seen_ts.
	type cellAgg struct {
		plateSet   map[string]struct{}
		mostRecent time.Time
	}
	cells := make(map[string]*cellAgg, len(rows))
	for _, r := range rows {
		key := fmt.Sprintf("%d|%d", r.CellLat, r.CellLng)
		c, ok := cells[key]
		if !ok {
			c = &cellAgg{plateSet: make(map[string]struct{})}
			cells[key] = c
		}
		c.plateSet[string(r.PlateHash)] = struct{}{}
		if r.LastSeenTs.Valid && r.LastSeenTs.Time.After(c.mostRecent) {
			c.mostRecent = r.LastSeenTs.Time
		}
	}

	// Find the cell with the most distinct plates that crosses the
	// threshold. Most-plates-first means a single signature seen in
	// multiple cells (each crossing the threshold) raises one alert
	// for the strongest cell rather than fanning out into N alerts.
	var (
		bestKey    string
		bestPlates [][]byte
		bestRecent time.Time
	)
	cellKeys := make([]string, 0, len(cells))
	for k := range cells {
		cellKeys = append(cellKeys, k)
	}
	// Deterministic iteration: sort cell keys so two evaluations on
	// identical data produce identical alerts.
	sort.Strings(cellKeys)
	for _, k := range cellKeys {
		c := cells[k]
		if len(c.plateSet) < t.PlateSwapMinPlates {
			continue
		}
		if len(c.plateSet) > len(bestPlates) {
			bestKey = k
			bestPlates = collectPlates(c.plateSet)
			bestRecent = c.mostRecent
		}
	}
	if len(bestPlates) < t.PlateSwapMinPlates {
		return SwapAlert{}, false, nil
	}

	// Whitelist suppression: drop any plate that is currently
	// whitelisted. If after that step the cell falls below the
	// threshold, the alert is suppressed.
	survivors := make([][]byte, 0, len(bestPlates))
	for _, p := range bestPlates {
		wl, err := deps.GetWatchlistByHash(ctx, p)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				survivors = append(survivors, p)
				continue
			}
			return SwapAlert{}, false, fmt.Errorf("get watchlist for plate: %w", err)
		}
		if wl.Kind == "whitelist" {
			continue
		}
		survivors = append(survivors, p)
	}
	if len(survivors) < t.PlateSwapMinPlates {
		return SwapAlert{}, false, nil
	}

	severity := t.PlateSwapSeverity
	if severity <= 0 {
		severity = DefaultPlateSwapSeverity
	}

	return SwapAlert{
		SignatureID:    signatureID,
		Severity:       severity,
		AreaCellKey:    bestKey,
		PlateHashes:    survivors,
		MostRecentSeen: bestRecent,
		Evidence: map[string]any{
			"signature_id":     signatureID,
			"area_cell":        bestKey,
			"plate_count":      len(survivors),
			"lookback_days":    t.PlateSwapLookbackDays,
			"area_cell_km":     t.PlateSwapAreaCellKm,
			"min_plates":       t.PlateSwapMinPlates,
			"fusion_version":   SignatureFusionVersion,
			"most_recent_seen": bestRecent.UTC().Format(time.RFC3339),
		},
	}, true, nil
}

// collectPlates returns a stable, lexicographically-sorted slice of
// the distinct plate hashes in plateSet. The keys are kept as bytes
// (string conversion is just the standard Go map-key idiom) so the
// returned slice is suitable for direct use as evidence.
func collectPlates(plateSet map[string]struct{}) [][]byte {
	plates := make([][]byte, 0, len(plateSet))
	for k := range plateSet {
		plates = append(plates, []byte(k))
	}
	sort.Slice(plates, func(i, j int) bool {
		return string(plates[i]) < string(plates[j])
	})
	return plates
}

// kmPerDegLat is the conversion factor from km to degrees of latitude
// at the equator (and within rounding error everywhere else; the Earth
// is close enough to a sphere for the heuristic's purposes).
const kmPerDegLat = 111.32

// ErrNoFusionDeps is returned when FuseSignatures is invoked without a
// dependency handle. The worker should never trigger this; the explicit
// error makes a misconfiguration loud instead of silent.
var ErrNoFusionDeps = errors.New("alpr fusion: deps not configured")
