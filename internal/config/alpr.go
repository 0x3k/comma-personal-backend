package config

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// ALPRConfig holds deployment-time ALPR configuration loaded from environment
// variables. Runtime-toggleable values (the master enable flag, region,
// confidence threshold, retention windows, notify severity) are seeded from
// here into the settings table; later overrides via PUT /v1/settings/alpr
// take precedence at read time.
//
// EncryptionKeyB64 has no default. An empty value means the feature cannot
// be enabled because there is no safe way to store plate text without it.
// EncryptionKeyConfigured() reports whether the key is both set and decodes
// to exactly 32 bytes (a 256-bit AES key).
type ALPRConfig struct {
	// EngineURL is the base URL of the ALPR detection engine. The /health
	// endpoint underneath this URL is probed by the GET status handler.
	EngineURL string

	// Region selects the country/jurisdiction-specific plate format model
	// in the engine. One of "us", "eu", "uk", "other".
	Region string

	// FramesPerSecond is the default sampling rate for the frame-extractor
	// worker. Must fall within [0.5, 4] when overridden at runtime.
	FramesPerSecond float64

	// ConfidenceMin is the minimum OCR confidence to retain a plate read.
	// Must fall within [0.5, 0.95] when overridden at runtime.
	ConfidenceMin float64

	// EncryptionKeyB64 is the base64-encoded 32-byte AES-256 key used to
	// encrypt plate text at rest. Empty disables enabling ALPR entirely.
	EncryptionKeyB64 string

	// RetentionDaysUnflagged is the default retention window for plate
	// reads not associated with a flagged event. 0 means never delete.
	RetentionDaysUnflagged int

	// RetentionDaysFlagged is the default retention window for plate
	// reads on flagged events. 0 means never delete.
	RetentionDaysFlagged int

	// ExtractorConcurrency is the number of frame-extractor workers run
	// in parallel.
	ExtractorConcurrency int

	// DetectorConcurrency is the number of detection workers run in
	// parallel against the engine.
	DetectorConcurrency int

	// NotifyMinSeverity is the minimum event severity that triggers a
	// notification. Severity is an integer scale 1..5.
	NotifyMinSeverity int

	// NotifyEmail, when non-empty, is the destination email for ALPR
	// notifications.
	NotifyEmail string

	// NotifyWebhook, when non-empty, is the URL to POST notification
	// payloads to.
	NotifyWebhook string
}

// alprDefaults is the canonical list of defaults applied when an env var is
// unset. Kept centralised so config and tests use the same source of truth.
var alprDefaults = ALPRConfig{
	EngineURL:              "http://alpr:8081",
	Region:                 "us",
	FramesPerSecond:        2,
	ConfidenceMin:          0.75,
	RetentionDaysUnflagged: 30,
	RetentionDaysFlagged:   365,
	ExtractorConcurrency:   1,
	DetectorConcurrency:    2,
	NotifyMinSeverity:      4,
}

// LoadALPR reads ALPR-related environment variables and returns an
// ALPRConfig. Missing values fall back to alprDefaults. Malformed numeric
// values are rejected with an error so misconfiguration surfaces at
// startup rather than silently using a default.
//
// Note: a malformed ALPR_ENCRYPTION_KEY (set but not valid base64 or fewer
// than 32 bytes) is NOT a fatal error. The key field is loaded as-is, and
// LoadALPR logs a warning. EncryptionKeyConfigured() reports the validity
// after the fact so callers can decide whether to allow enabling.
func LoadALPR() (*ALPRConfig, error) {
	cfg := alprDefaults
	cfg.EngineURL = strings.TrimSpace(envOrDefault("ALPR_ENGINE_URL", alprDefaults.EngineURL))
	cfg.Region = strings.TrimSpace(envOrDefault("ALPR_REGION", alprDefaults.Region))

	if v := strings.TrimSpace(os.Getenv("ALPR_FPS")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to load ALPR config: ALPR_FPS must be a number, got %q", v)
		}
		cfg.FramesPerSecond = f
	}

	if v := strings.TrimSpace(os.Getenv("ALPR_CONFIDENCE_MIN")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to load ALPR config: ALPR_CONFIDENCE_MIN must be a number, got %q", v)
		}
		cfg.ConfidenceMin = f
	}

	if v, err := envIntALPR("ALPR_RETENTION_DAYS_UNFLAGGED", alprDefaults.RetentionDaysUnflagged); err != nil {
		return nil, err
	} else {
		cfg.RetentionDaysUnflagged = v
	}

	if v, err := envIntALPR("ALPR_RETENTION_DAYS_FLAGGED", alprDefaults.RetentionDaysFlagged); err != nil {
		return nil, err
	} else {
		cfg.RetentionDaysFlagged = v
	}

	if v, err := envIntALPR("ALPR_EXTRACTOR_CONCURRENCY", alprDefaults.ExtractorConcurrency); err != nil {
		return nil, err
	} else {
		cfg.ExtractorConcurrency = v
	}

	if v, err := envIntALPR("ALPR_DETECTOR_CONCURRENCY", alprDefaults.DetectorConcurrency); err != nil {
		return nil, err
	} else {
		cfg.DetectorConcurrency = v
	}

	if v, err := envIntALPR("ALPR_NOTIFY_MIN_SEVERITY", alprDefaults.NotifyMinSeverity); err != nil {
		return nil, err
	} else {
		cfg.NotifyMinSeverity = v
	}

	cfg.EncryptionKeyB64 = strings.TrimSpace(os.Getenv("ALPR_ENCRYPTION_KEY"))
	cfg.NotifyEmail = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_EMAIL"))
	cfg.NotifyWebhook = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_WEBHOOK"))

	// Surface a malformed encryption key at startup so the operator can
	// fix it before trying to enable ALPR. Empty is fine -- it just means
	// the feature stays unenableable until a key is provided.
	if cfg.EncryptionKeyB64 != "" && !cfg.EncryptionKeyConfigured() {
		log.Printf("warning: ALPR_ENCRYPTION_KEY is set but is not valid base64 of a 32-byte key; ALPR cannot be enabled until this is fixed")
	}

	return &cfg, nil
}

// EncryptionKeyConfigured reports whether ALPR_ENCRYPTION_KEY decodes to
// exactly 32 bytes (the size of an AES-256 key). Empty or malformed input
// returns false so the PUT handler can refuse to enable ALPR.
func (c *ALPRConfig) EncryptionKeyConfigured() bool {
	if c == nil || c.EncryptionKeyB64 == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(c.EncryptionKeyB64)
	if err != nil {
		// Try unpadded base64 as a convenience.
		raw, err = base64.RawStdEncoding.DecodeString(c.EncryptionKeyB64)
		if err != nil {
			return false
		}
	}
	return len(raw) == 32
}

// envOrDefault returns the trimmed value of name if set, otherwise def.
func envOrDefault(name, def string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	return v
}

// envIntALPR parses an integer env var or returns def. Negative values are
// rejected to match the rest of the config package.
func envIntALPR(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("failed to load ALPR config: %s must be an integer, got %q", name, v)
	}
	if n < 0 {
		return 0, fmt.Errorf("failed to load ALPR config: %s must be >= 0, got %d", name, n)
	}
	return n, nil
}
