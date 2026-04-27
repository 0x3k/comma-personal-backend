package main

import (
	"context"
	"errors"
	"log"
	"strconv"

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
