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

	// NotifyEmailTo is the destination address(es) the email sender
	// uses for outbound alerts. Comma-separated when multiple. An empty
	// value disables the email sender even when the rest of SMTP is
	// configured. Loaded from ALPR_NOTIFY_EMAIL_TO; ALPR_NOTIFY_EMAIL is
	// the legacy synonym retained for backwards compatibility.
	NotifyEmailTo string

	// NotifySMTPHost is the SMTP relay host. Empty disables the email
	// sender. No default -- the operator must point us at a relay.
	NotifySMTPHost string

	// NotifySMTPPort is the SMTP relay port. Defaults to 587 (the
	// canonical submission port for STARTTLS-protected mail).
	NotifySMTPPort int

	// NotifySMTPUser / NotifySMTPPass are the credentials used to
	// authenticate against the relay. Both empty means anonymous
	// submission (legitimate for an internal relay; risky for a public
	// one). The pair is consumed by net/smtp.PlainAuth.
	NotifySMTPUser string
	NotifySMTPPass string

	// NotifySMTPFrom is the envelope-from / From: header address. When
	// empty the email sender falls back to NotifySMTPUser; if that is
	// also empty the email sender refuses to start (no valid sender).
	NotifySMTPFrom string

	// NotifySMTPTLS selects the TLS mode for the SMTP connection. One
	// of "starttls" (default), "implicit" (TLS from connect), or
	// "plain" (no TLS, discouraged but permitted for local relays).
	NotifySMTPTLS string

	// NotifyWebhookURL is the URL the webhook sender POSTs the alert
	// payload to. Empty disables the webhook sender. ALPR_NOTIFY_WEBHOOK
	// is the legacy synonym retained for backwards compatibility.
	NotifyWebhookURL string

	// NotifyDedupHours is the per-plate dedup window in hours. A
	// repeat alert for the same plate within this window is dropped by
	// the dispatcher to avoid spamming the user during an ongoing
	// situation. Default 12.
	NotifyDedupHours int

	// DashboardURL is the absolute base URL the notification body /
	// payload uses to construct deep links into the operator's
	// dashboard (e.g. <DashboardURL>/alpr/plates/<hash_b64>). Empty is
	// permitted -- in that case the notification still goes out, but
	// the deep link is omitted from the body and reported as an empty
	// string in the webhook payload.
	DashboardURL string
}

// NotifySMTP TLS mode constants. Compared case-insensitively.
const (
	ALPRNotifySMTPTLSStartTLS = "starttls"
	ALPRNotifySMTPTLSImplicit = "implicit"
	ALPRNotifySMTPTLSPlain    = "plain"
)

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
	NotifySMTPPort:         587,
	NotifySMTPTLS:          ALPRNotifySMTPTLSStartTLS,
	NotifyDedupHours:       12,
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

	// Notification channel (email + webhook) wiring. ALPR_NOTIFY_EMAIL_TO
	// is the canonical knob for this feature; the legacy ALPR_NOTIFY_EMAIL
	// is honoured as a fallback so existing deployments do not break.
	cfg.NotifyEmailTo = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_EMAIL_TO"))
	if cfg.NotifyEmailTo == "" {
		cfg.NotifyEmailTo = cfg.NotifyEmail
	}
	cfg.NotifySMTPHost = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_SMTP_HOST"))
	if v, err := envIntALPR("ALPR_NOTIFY_SMTP_PORT", alprDefaults.NotifySMTPPort); err != nil {
		return nil, err
	} else {
		cfg.NotifySMTPPort = v
	}
	cfg.NotifySMTPUser = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_SMTP_USER"))
	cfg.NotifySMTPPass = os.Getenv("ALPR_NOTIFY_SMTP_PASS") // do NOT trim; some passwords legitimately contain whitespace.
	cfg.NotifySMTPFrom = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_SMTP_FROM"))
	cfg.NotifySMTPTLS = strings.ToLower(strings.TrimSpace(envOrDefault("ALPR_NOTIFY_SMTP_TLS", alprDefaults.NotifySMTPTLS)))
	switch cfg.NotifySMTPTLS {
	case ALPRNotifySMTPTLSStartTLS, ALPRNotifySMTPTLSImplicit, ALPRNotifySMTPTLSPlain:
	default:
		return nil, fmt.Errorf("failed to load ALPR config: ALPR_NOTIFY_SMTP_TLS must be one of starttls, implicit, plain (got %q)", cfg.NotifySMTPTLS)
	}
	cfg.NotifyWebhookURL = strings.TrimSpace(os.Getenv("ALPR_NOTIFY_WEBHOOK_URL"))
	if cfg.NotifyWebhookURL == "" {
		cfg.NotifyWebhookURL = cfg.NotifyWebhook
	}
	if v, err := envIntALPR("ALPR_NOTIFY_DEDUP_HOURS", alprDefaults.NotifyDedupHours); err != nil {
		return nil, err
	} else {
		cfg.NotifyDedupHours = v
	}
	cfg.DashboardURL = strings.TrimRight(strings.TrimSpace(os.Getenv("ALPR_DASHBOARD_URL")), "/")

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
