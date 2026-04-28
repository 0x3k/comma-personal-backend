package main

import (
	"context"
	"errors"
	"log"
	"strconv"

	alprcrypto "comma-personal-backend/internal/alpr/crypto"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/settings"
)

// seedALPRDefaults pushes env-derived ALPR defaults into the settings table
// on first boot. The master flag (alpr_enabled) is intentionally NOT
// seeded -- its absence already means "off", and seeding it would force
// every fresh deployment to write a row before any operator interaction.
//
// Each seed is best-effort: a transient failure logs a warning and the
// next startup will retry. This keeps boot fast and avoids fail-fast on a
// degraded database that is still healthy enough to serve reads.
func seedALPRDefaults(store *settings.Store, cfg *config.ALPRConfig) {
	if cfg == nil {
		return
	}
	ctx := context.Background()

	if err := seedStringIfMissing(ctx, store, settings.KeyALPRRegion, cfg.Region); err != nil {
		log.Printf("warning: failed to seed alpr_region setting: %v", err)
	}
	if err := seedFloatIfMissing(ctx, store, settings.KeyALPRFramesPerSecond, cfg.FramesPerSecond); err != nil {
		log.Printf("warning: failed to seed alpr_frames_per_second setting: %v", err)
	}
	if err := seedFloatIfMissing(ctx, store, settings.KeyALPRConfidenceMin, cfg.ConfidenceMin); err != nil {
		log.Printf("warning: failed to seed alpr_confidence_min setting: %v", err)
	}
	if err := store.SeedIntIfMissing(ctx, settings.KeyALPRRetentionDaysUnflagged, cfg.RetentionDaysUnflagged); err != nil {
		log.Printf("warning: failed to seed alpr_retention_days_unflagged setting: %v", err)
	}
	if err := store.SeedIntIfMissing(ctx, settings.KeyALPRRetentionDaysFlagged, cfg.RetentionDaysFlagged); err != nil {
		log.Printf("warning: failed to seed alpr_retention_days_flagged setting: %v", err)
	}
	if err := store.SeedIntIfMissing(ctx, settings.KeyALPRNotifyMinSeverity, cfg.NotifyMinSeverity); err != nil {
		log.Printf("warning: failed to seed alpr_notify_min_severity setting: %v", err)
	}
}

// checkALPRKeyring inspects ALPR_ENCRYPTION_KEY (carried on cfg) and
// returns an error if the key is set but the keyring cannot be loaded
// or fails a round-trip self-check.
//
// If ALPR_ENCRYPTION_KEY is unset (the common case for users not opting
// in to ALPR) this is a silent no-op so disabled installs never see a
// startup error. The malformed-but-set case is already warned about by
// config.LoadALPR (which logs but does not abort); this function escalates
// it to a fatal at the call site because the operator has clearly opted
// in but the configuration is unusable -- failing fast is friendlier
// than corrupting data with ciphertext nothing can ever decrypt.
//
// Split out from verifyALPRKeyring so the error path is unit-testable
// without invoking os.Exit via log.Fatalf.
func checkALPRKeyring(cfg *config.ALPRConfig) error {
	if cfg == nil || cfg.EncryptionKeyB64 == "" {
		return nil
	}
	k, err := alprcrypto.LoadKeyring(cfg.EncryptionKeyB64)
	if err != nil {
		return err
	}
	return alprcrypto.VerifyRoundtrip(k)
}

// verifyALPRKeyring runs at startup, before any handler or worker reads
// or writes plate ciphertext. Aborts the process on any error.
func verifyALPRKeyring(cfg *config.ALPRConfig) {
	if err := checkALPRKeyring(cfg); err != nil {
		log.Fatalf("ALPR_ENCRYPTION_KEY self-check failed: %v", err)
	}
}

// logALPRStartup emits one info line summarising the active ALPR config
// when alpr_enabled is true. When the flag is absent or false the function
// is a no-op so the expected opt-in default produces no log noise.
func logALPRStartup(store *settings.Store, cfg *config.ALPRConfig) {
	enabled, err := store.BoolOr(context.Background(), settings.KeyALPREnabled, false)
	if err != nil {
		log.Printf("warning: failed to read alpr_enabled at startup: %v", err)
		return
	}
	if !enabled {
		return
	}
	if cfg == nil {
		log.Printf("alpr enabled (config not loaded)")
		return
	}
	log.Printf("alpr enabled: engine_url=%s region=%s fps=%v confidence_min=%v retention_unflagged=%dd retention_flagged=%dd encryption_key_configured=%v",
		cfg.EngineURL,
		cfg.Region,
		cfg.FramesPerSecond,
		cfg.ConfidenceMin,
		cfg.RetentionDaysUnflagged,
		cfg.RetentionDaysFlagged,
		cfg.EncryptionKeyConfigured(),
	)
}

// seedStringIfMissing inserts the given string value for key only when the
// row is absent. Mirrors settings.SeedIntIfMissing but for string values
// (the settings package does not expose a string seed directly because the
// only existing caller -- retention -- is an int).
func seedStringIfMissing(ctx context.Context, store *settings.Store, key, value string) error {
	if _, err := store.Get(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, settings.ErrNotFound) {
		return err
	}
	return store.Set(ctx, key, value)
}

// seedFloatIfMissing inserts the float value only when the row is absent.
func seedFloatIfMissing(ctx context.Context, store *settings.Store, key string, value float64) error {
	if _, err := store.Get(ctx, key); err == nil {
		return nil
	} else if !errors.Is(err, settings.ErrNotFound) {
		return err
	}
	return store.Set(ctx, key, strconv.FormatFloat(value, 'g', -1, 64))
}
