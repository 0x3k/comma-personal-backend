package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// alprEnvVars is the ALPR-specific subset of configEnvVars. Kept separate
// because LoadALPR can be called directly without going through Load().
var alprEnvVars = []string{
	"ALPR_ENGINE_URL",
	"ALPR_REGION",
	"ALPR_FPS",
	"ALPR_CONFIDENCE_MIN",
	"ALPR_ENCRYPTION_KEY",
	"ALPR_RETENTION_DAYS_UNFLAGGED",
	"ALPR_RETENTION_DAYS_FLAGGED",
	"ALPR_EXTRACTOR_CONCURRENCY",
	"ALPR_DETECTOR_CONCURRENCY",
	"ALPR_NOTIFY_MIN_SEVERITY",
	"ALPR_NOTIFY_EMAIL",
	"ALPR_NOTIFY_WEBHOOK",
	"ALPR_NOTIFY_EMAIL_TO",
	"ALPR_NOTIFY_SMTP_HOST",
	"ALPR_NOTIFY_SMTP_PORT",
	"ALPR_NOTIFY_SMTP_USER",
	"ALPR_NOTIFY_SMTP_PASS",
	"ALPR_NOTIFY_SMTP_FROM",
	"ALPR_NOTIFY_SMTP_TLS",
	"ALPR_NOTIFY_WEBHOOK_URL",
	"ALPR_NOTIFY_DEDUP_HOURS",
	"ALPR_DASHBOARD_URL",
}

func clearALPREnv(t *testing.T) {
	t.Helper()
	for _, k := range alprEnvVars {
		t.Setenv(k, "")
	}
}

// validKeyB64 returns a base64 string that decodes to exactly 32 bytes,
// the size of an AES-256 key.
func validKeyB64() string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestLoadALPR_DefaultsWhenUnset(t *testing.T) {
	clearALPREnv(t)

	cfg, err := LoadALPR()
	if err != nil {
		t.Fatalf("LoadALPR returned error: %v", err)
	}
	if cfg.EngineURL != "http://alpr:8081" {
		t.Errorf("EngineURL = %q, want default http://alpr:8081", cfg.EngineURL)
	}
	if cfg.Region != "us" {
		t.Errorf("Region = %q, want default us", cfg.Region)
	}
	if cfg.FramesPerSecond != 2 {
		t.Errorf("FramesPerSecond = %v, want 2", cfg.FramesPerSecond)
	}
	if cfg.ConfidenceMin != 0.75 {
		t.Errorf("ConfidenceMin = %v, want 0.75", cfg.ConfidenceMin)
	}
	if cfg.RetentionDaysUnflagged != 30 {
		t.Errorf("RetentionDaysUnflagged = %d, want 30", cfg.RetentionDaysUnflagged)
	}
	if cfg.RetentionDaysFlagged != 365 {
		t.Errorf("RetentionDaysFlagged = %d, want 365", cfg.RetentionDaysFlagged)
	}
	if cfg.ExtractorConcurrency != 1 {
		t.Errorf("ExtractorConcurrency = %d, want 1", cfg.ExtractorConcurrency)
	}
	if cfg.DetectorConcurrency != 2 {
		t.Errorf("DetectorConcurrency = %d, want 2", cfg.DetectorConcurrency)
	}
	if cfg.NotifyMinSeverity != 4 {
		t.Errorf("NotifyMinSeverity = %d, want 4", cfg.NotifyMinSeverity)
	}
	if cfg.EncryptionKeyB64 != "" {
		t.Errorf("EncryptionKeyB64 = %q, want empty default", cfg.EncryptionKeyB64)
	}
	if cfg.EncryptionKeyConfigured() {
		t.Error("EncryptionKeyConfigured() = true with empty key, want false")
	}
}

func TestLoadALPR_OverridesFromEnv(t *testing.T) {
	clearALPREnv(t)
	t.Setenv("ALPR_ENGINE_URL", "http://engine.local:9999")
	t.Setenv("ALPR_REGION", "eu")
	t.Setenv("ALPR_FPS", "3.5")
	t.Setenv("ALPR_CONFIDENCE_MIN", "0.9")
	t.Setenv("ALPR_RETENTION_DAYS_UNFLAGGED", "7")
	t.Setenv("ALPR_RETENTION_DAYS_FLAGGED", "180")
	t.Setenv("ALPR_EXTRACTOR_CONCURRENCY", "4")
	t.Setenv("ALPR_DETECTOR_CONCURRENCY", "8")
	t.Setenv("ALPR_NOTIFY_MIN_SEVERITY", "3")
	t.Setenv("ALPR_NOTIFY_EMAIL", "ops@example.com")
	t.Setenv("ALPR_NOTIFY_WEBHOOK", "https://hooks.example.com/alpr")
	t.Setenv("ALPR_ENCRYPTION_KEY", validKeyB64())

	cfg, err := LoadALPR()
	if err != nil {
		t.Fatalf("LoadALPR returned error: %v", err)
	}
	if cfg.EngineURL != "http://engine.local:9999" {
		t.Errorf("EngineURL = %q", cfg.EngineURL)
	}
	if cfg.Region != "eu" {
		t.Errorf("Region = %q", cfg.Region)
	}
	if cfg.FramesPerSecond != 3.5 {
		t.Errorf("FramesPerSecond = %v", cfg.FramesPerSecond)
	}
	if cfg.ConfidenceMin != 0.9 {
		t.Errorf("ConfidenceMin = %v", cfg.ConfidenceMin)
	}
	if cfg.RetentionDaysUnflagged != 7 {
		t.Errorf("RetentionDaysUnflagged = %d", cfg.RetentionDaysUnflagged)
	}
	if cfg.RetentionDaysFlagged != 180 {
		t.Errorf("RetentionDaysFlagged = %d", cfg.RetentionDaysFlagged)
	}
	if cfg.ExtractorConcurrency != 4 {
		t.Errorf("ExtractorConcurrency = %d", cfg.ExtractorConcurrency)
	}
	if cfg.DetectorConcurrency != 8 {
		t.Errorf("DetectorConcurrency = %d", cfg.DetectorConcurrency)
	}
	if cfg.NotifyMinSeverity != 3 {
		t.Errorf("NotifyMinSeverity = %d", cfg.NotifyMinSeverity)
	}
	if cfg.NotifyEmail != "ops@example.com" {
		t.Errorf("NotifyEmail = %q", cfg.NotifyEmail)
	}
	if cfg.NotifyWebhook != "https://hooks.example.com/alpr" {
		t.Errorf("NotifyWebhook = %q", cfg.NotifyWebhook)
	}
	if !cfg.EncryptionKeyConfigured() {
		t.Error("EncryptionKeyConfigured() = false with valid 32-byte key")
	}
}

func TestLoadALPR_RejectsBadFloats(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
	}{
		{"bad fps", "ALPR_FPS"},
		{"bad confidence", "ALPR_CONFIDENCE_MIN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearALPREnv(t)
			t.Setenv(tt.envVar, "not-a-number")
			if _, err := LoadALPR(); err == nil {
				t.Fatalf("LoadALPR returned nil error for %s=not-a-number, want error", tt.envVar)
			}
		})
	}
}

func TestLoadALPR_RejectsBadInts(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		val    string
	}{
		{"non-integer retention", "ALPR_RETENTION_DAYS_UNFLAGGED", "abc"},
		{"negative retention", "ALPR_RETENTION_DAYS_FLAGGED", "-1"},
		{"non-integer extractor concurrency", "ALPR_EXTRACTOR_CONCURRENCY", "x"},
		{"negative detector concurrency", "ALPR_DETECTOR_CONCURRENCY", "-3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearALPREnv(t)
			t.Setenv(tt.envVar, tt.val)
			if _, err := LoadALPR(); err == nil {
				t.Fatalf("LoadALPR returned nil error for %s=%q, want error", tt.envVar, tt.val)
			}
		})
	}
}

func TestEncryptionKeyConfigured(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"empty", "", false},
		{"not base64", "!!!not-base64!!!", false},
		{"base64 too short", base64.StdEncoding.EncodeToString([]byte("too short")), false},
		{"base64 31 bytes", base64.StdEncoding.EncodeToString(make([]byte, 31)), false},
		{"base64 32 bytes", base64.StdEncoding.EncodeToString(make([]byte, 32)), true},
		{"base64 33 bytes", base64.StdEncoding.EncodeToString(make([]byte, 33)), false},
		{"raw (unpadded) base64 32 bytes", base64.RawStdEncoding.EncodeToString(make([]byte, 32)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ALPRConfig{EncryptionKeyB64: tt.key}
			if got := cfg.EncryptionKeyConfigured(); got != tt.want {
				t.Errorf("EncryptionKeyConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadALPR_MalformedKeyDoesNotFail(t *testing.T) {
	clearALPREnv(t)
	t.Setenv("ALPR_ENCRYPTION_KEY", "not-a-valid-key")
	cfg, err := LoadALPR()
	if err != nil {
		t.Fatalf("LoadALPR returned error for malformed key, want nil: %v", err)
	}
	if cfg.EncryptionKeyConfigured() {
		t.Error("EncryptionKeyConfigured() = true for malformed key")
	}
	// Sanity check: the key value is still preserved as-is so the operator
	// can see what they configured.
	if !strings.Contains(cfg.EncryptionKeyB64, "not-a-valid-key") {
		t.Errorf("EncryptionKeyB64 = %q, want raw value preserved", cfg.EncryptionKeyB64)
	}
}
