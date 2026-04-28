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
