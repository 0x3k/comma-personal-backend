package settings

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// ALPR runtime setting keys. These rows live alongside KeyRetentionDays in
// the same `settings` table; their defaults are seeded from the env-var
// ALPRConfig at startup so a later API override does not require a restart
// to take effect.
const (
	// KeyALPREnabled is the master enable/disable for the ALPR pipeline.
	// Default: false. When false, workers and handlers must short-circuit
	// regardless of the feature's other settings.
	KeyALPREnabled = "alpr_enabled"

	// KeyALPRRegion selects the country/jurisdiction-specific plate
	// model. One of "us", "eu", "uk", "other".
	KeyALPRRegion = "alpr_region"

	// KeyALPRFramesPerSecond is the runtime override for the
	// frame-extractor sampling rate. Bounded [0.5, 4] at the API layer.
	KeyALPRFramesPerSecond = "alpr_frames_per_second"

	// KeyALPRConfidenceMin is the runtime override for the minimum OCR
	// confidence to retain a plate read. Bounded [0.5, 0.95] at the API
	// layer.
	KeyALPRConfidenceMin = "alpr_confidence_min"

	// KeyALPRRetentionDaysUnflagged is the retention window in days for
	// plate reads not associated with a flagged event. 0 = never delete.
	KeyALPRRetentionDaysUnflagged = "alpr_retention_days_unflagged"

	// KeyALPRRetentionDaysFlagged is the retention window in days for
	// plate reads on flagged events. 0 = never delete.
	KeyALPRRetentionDaysFlagged = "alpr_retention_days_flagged"

	// KeyALPRNotifyMinSeverity is the minimum event severity that
	// triggers an outbound notification.
	KeyALPRNotifyMinSeverity = "alpr_notify_min_severity"

	// KeyALPRDisclaimerVersion is the disclaimer version the operator
	// last acknowledged. Owned by the alpr-disclaimer-gate feature; this
	// package only reads it to validate the enable precondition.
	KeyALPRDisclaimerVersion = "alpr_disclaimer_version"

	// KeyALPRDisclaimerAckedAt is the RFC3339 timestamp at which the
	// operator acknowledged the current disclaimer. Owned by
	// alpr-disclaimer-gate; used here for the enable precondition only.
	KeyALPRDisclaimerAckedAt = "alpr_disclaimer_acked_at"

	// KeyALPRDisclaimerAckedJurisdiction is the jurisdiction the operator
	// declared when they acknowledged the disclaimer. One of "us", "eu",
	// "uk", "ca", "au", "other". Recorded so the audit trail captures the
	// declared legal context (Illinois BIPA, EU GDPR, UK DPA, etc.) at the
	// time of the ack -- it does NOT change the ALPR pipeline's region
	// selection (that is KeyALPRRegion).
	KeyALPRDisclaimerAckedJurisdiction = "alpr_disclaimer_acked_jurisdiction"

	// KeyALPREncounterGapSeconds is the maximum allowed gap (in seconds)
	// between consecutive plate detections within a single encounter. A
	// gap longer than this value starts a new encounter. Default 60s.
	// Bounded to a positive value at the API layer; the worker also
	// clamps to a sensible minimum so a misconfigured zero cannot collapse
	// every detection into one encounter.
	KeyALPREncounterGapSeconds = "alpr_encounter_gap_seconds"

	// Stalking-detection heuristic tunables. The heuristic worker
	// reads these via *Or() helpers so a missing row falls back to
	// the package defaults in internal/alpr/heuristic. Each key maps
	// 1:1 to a field on the heuristic.Thresholds struct; see the
	// constants in that package for the rule semantics.
	//
	// The heuristic version itself is intentionally NOT a setting --
	// it is a code constant (heuristic.HeuristicVersion) tied to a
	// deploy so two replicas cannot disagree on what "v1.0.0" means.

	// KeyALPRHeuristicLookbackDays is the encounter-history window
	// (in days) the worker pulls when scoring a plate. Default 30.
	KeyALPRHeuristicLookbackDays = "alpr_heuristic_lookback_days"

	// KeyALPRHeuristicTurnsMin is the per-encounter turn count above
	// which within_route_turns starts to score. Default 2.
	KeyALPRHeuristicTurnsMin = "alpr_heuristic_turns_min"

	// KeyALPRHeuristicTurnsPointsCap caps the within_route_turns
	// component so a single chaotic route cannot saturate severity.
	// Default 4.0.
	KeyALPRHeuristicTurnsPointsCap = "alpr_heuristic_turns_points_cap"

	// KeyALPRHeuristicPersistenceMinutesMid is the duration (in
	// minutes) at which within_route_persistence first awards
	// points. Default 8.
	KeyALPRHeuristicPersistenceMinutesMid = "alpr_heuristic_persistence_minutes_mid"

	// KeyALPRHeuristicPersistenceMinutesHigh is the duration (in
	// minutes) at which within_route_persistence promotes to the
	// high tier. Default 15.
	KeyALPRHeuristicPersistenceMinutesHigh = "alpr_heuristic_persistence_minutes_high"

	// KeyALPRHeuristicPersistenceMidPoints is the points awarded for
	// the mid tier of within_route_persistence. Default 1.0.
	KeyALPRHeuristicPersistenceMidPoints = "alpr_heuristic_persistence_mid_points"

	// KeyALPRHeuristicPersistenceHighPoints is the points awarded
	// for the high tier (replaces, does not add to, the mid tier).
	// Default 1.5.
	KeyALPRHeuristicPersistenceHighPoints = "alpr_heuristic_persistence_high_points"

	// KeyALPRHeuristicDistinctRoutesMid is the route count at which
	// cross_route_count first awards points. Default 3.
	KeyALPRHeuristicDistinctRoutesMid = "alpr_heuristic_distinct_routes_min"

	// KeyALPRHeuristicDistinctRoutesMidPoints is the points for the
	// cross_route_count mid tier. Default 1.5.
	KeyALPRHeuristicDistinctRoutesMidPoints = "alpr_heuristic_distinct_routes_mid_points"

	// KeyALPRHeuristicDistinctRoutesHigh is the route count at which
	// cross_route_count promotes to the high tier (which stacks on
	// top of the mid tier). Default 5.
	KeyALPRHeuristicDistinctRoutesHigh = "alpr_heuristic_distinct_routes_high"

	// KeyALPRHeuristicDistinctRoutesHighPoints is the additional
	// points for the cross_route_count high tier. Default 1.0.
	KeyALPRHeuristicDistinctRoutesHighPoints = "alpr_heuristic_distinct_routes_high_points"

	// KeyALPRHeuristicDistinctAreasMin is the minimum number of
	// distinct geo cells required for cross_route_geo_spread to
	// fire. Default 2.
	KeyALPRHeuristicDistinctAreasMin = "alpr_heuristic_distinct_areas_min"

	// KeyALPRHeuristicDistinctAreasPoints is the points awarded
	// when the distinct-areas threshold is met. Default 2.0.
	KeyALPRHeuristicDistinctAreasPoints = "alpr_heuristic_distinct_areas_points"

	// KeyALPRHeuristicAreaCellKm is the side length (in km) of the
	// geo grid cell used by cross_route_geo_spread. Default 5.0.
	KeyALPRHeuristicAreaCellKm = "alpr_heuristic_area_cell_km"

	// KeyALPRHeuristicTimingWindowHours is the time window (in
	// hours) used by cross_route_timing. Default 24.0.
	KeyALPRHeuristicTimingWindowHours = "alpr_heuristic_timing_window_hours"

	// KeyALPRHeuristicTimingPoints is the points awarded when the
	// timing window threshold is met. Default 1.0.
	KeyALPRHeuristicTimingPoints = "alpr_heuristic_timing_points"

	// KeyALPRHeuristicSeverityBucketSev2 is the lower edge of the
	// severity-2 bucket -- a TotalScore at or above this value
	// produces severity >= 2. Default 2.0. Validated monotonically
	// against the higher tiers at the API layer.
	KeyALPRHeuristicSeverityBucketSev2 = "alpr_heuristic_severity_bucket_sev2"

	// KeyALPRHeuristicSeverityBucketSev3 is the lower edge of
	// severity 3. Default 4.0. Must satisfy sev3 >= sev2.
	KeyALPRHeuristicSeverityBucketSev3 = "alpr_heuristic_severity_bucket_sev3"

	// KeyALPRHeuristicSeverityBucketSev4 is the lower edge of
	// severity 4. Default 6.0. Must satisfy sev4 >= sev3.
	KeyALPRHeuristicSeverityBucketSev4 = "alpr_heuristic_severity_bucket_sev4"

	// KeyALPRHeuristicSeverityBucketSev5 is the lower edge of
	// severity 5. Default 8.0. Must satisfy sev5 >= sev4.
	KeyALPRHeuristicSeverityBucketSev5 = "alpr_heuristic_severity_bucket_sev5"

	// KeyALPRHeuristicPersistenceMinutesMin is the alias key surfaced
	// by the tuning UI for the lower (mid) persistence threshold.
	// The internal heuristic still reads
	// KeyALPRHeuristicPersistenceMinutesMid; this alias keeps the
	// settings-key namespace readable in the tuning surface ("min" =
	// the smallest persistence above which any points are awarded).
	KeyALPRHeuristicPersistenceMinutesMin = KeyALPRHeuristicPersistenceMinutesMid

	// --- Signature-fusion layer (alpr-signature-fusion-heuristic).
	//
	// The fusion-layer keys live behind the alpr_signature_* prefix
	// so they are visually distinct from the stalking-heuristic
	// alpr_heuristic_* keys. Operators tune the two layers
	// independently; a misconfigured fusion knob never bleeds into
	// stalking severity.

	// KeyALPRSignatureConsistencyShareMin is the minimum share of a
	// plate's signature-bearing detections that must agree on a
	// single signature for the signature_consistent component to
	// fire (and contribute the corroboration severity bump).
	// Default 0.80.
	KeyALPRSignatureConsistencyShareMin = "alpr_signature_consistency_share_min"

	// KeyALPRSignatureConsistencyPoints is the severity bump
	// applied when the consistency threshold is met. Default 0.5.
	KeyALPRSignatureConsistencyPoints = "alpr_signature_consistency_points"

	// KeyALPRSignatureConflictShareMin is the minimum share each
	// distinct signature must hold for the signature_inconsistent
	// component to fire. Default 0.20.
	KeyALPRSignatureConflictShareMin = "alpr_signature_conflict_share_min"

	// KeyALPRSignatureConflictMinSignatures is the minimum number
	// of distinct signatures (each above the share threshold)
	// required for signature_inconsistent. Default 2.
	KeyALPRSignatureConflictMinSignatures = "alpr_signature_conflict_min_signatures"

	// KeyALPRSignatureSwapMinPlates is the minimum number of
	// distinct plate hashes a single signature must show inside one
	// area cell for the plate-swap alert to fire. Default 3.
	KeyALPRSignatureSwapMinPlates = "alpr_signature_swap_min_plates"

	// KeyALPRSignatureSwapAreaCellKm is the side length (in km) of
	// the area cell used by plate-swap detection. Default 5.0.
	KeyALPRSignatureSwapAreaCellKm = "alpr_signature_swap_area_cell_km"

	// KeyALPRSignatureSwapLookbackDays is the encounter-history
	// window (in days) the fusion layer scans for plate-swap
	// detection. Default 14.
	KeyALPRSignatureSwapLookbackDays = "alpr_signature_swap_lookback_days"

	// KeyALPRSignatureSwapSeverity is the severity assigned to a
	// fresh signature-keyed plate-swap alert. Default 4.
	KeyALPRSignatureSwapSeverity = "alpr_signature_swap_severity"
)

// GetBool returns the boolean value for the given key. Stored values are
// "true"/"false"; any other content is reported as a parse error. A missing
// key returns ErrNotFound so callers can supply their own default.
func (s *Store) GetBool(ctx context.Context, key string) (bool, error) {
	raw, err := s.Get(ctx, key)
	if err != nil {
		return false, err
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, err
	}
	return v, nil
}

// SetBool writes the boolean value for key as "true" or "false".
func (s *Store) SetBool(ctx context.Context, key string, value bool) error {
	return s.Set(ctx, key, strconv.FormatBool(value))
}

// GetFloat returns the float64 value for the given key. A missing key
// returns ErrNotFound.
func (s *Store) GetFloat(ctx context.Context, key string) (float64, error) {
	raw, err := s.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// SetFloat writes the float64 value for key.
func (s *Store) SetFloat(ctx context.Context, key string, value float64) error {
	return s.Set(ctx, key, strconv.FormatFloat(value, 'g', -1, 64))
}

// BoolOr returns the stored bool for key or def if the row is missing or
// unparseable. Mirrors RetentionDays(envDefault) semantics.
func (s *Store) BoolOr(ctx context.Context, key string, def bool) (bool, error) {
	v, err := s.GetBool(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return def, nil
		}
		return def, err
	}
	return v, nil
}

// IntOr returns the stored int for key or def if the row is missing or
// unparseable.
func (s *Store) IntOr(ctx context.Context, key string, def int) (int, error) {
	v, err := s.GetInt(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return def, nil
		}
		return def, err
	}
	return v, nil
}

// FloatOr returns the stored float for key or def if the row is missing or
// unparseable.
func (s *Store) FloatOr(ctx context.Context, key string, def float64) (float64, error) {
	v, err := s.GetFloat(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return def, nil
		}
		return def, err
	}
	return v, nil
}

// StringOr returns the stored string for key or def if the row is missing.
// An underlying database error is propagated.
func (s *Store) StringOr(ctx context.Context, key, def string) (string, error) {
	v, err := s.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return def, nil
		}
		return def, err
	}
	return v, nil
}

// GetTime returns the parsed RFC3339 time stored under key. A missing key
// returns ErrNotFound; an unparseable value returns the parse error.
func (s *Store) GetTime(ctx context.Context, key string) (time.Time, error) {
	raw, err := s.Get(ctx, key)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// TimeOrZero returns the parsed time stored under key or the zero value
// when the row is missing or unparseable. The boolean indicates whether a
// valid time was loaded; callers serializing JSON null can use it to pick
// between a value and null.
func (s *Store) TimeOrZero(ctx context.Context, key string) (time.Time, bool, error) {
	t, err := s.GetTime(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	return t, true, nil
}
