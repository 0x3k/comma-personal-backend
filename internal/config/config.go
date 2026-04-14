package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	DatabaseURL      string
	StoragePath      string
	Port             string
	JWTSecret        string
	AllowedDongleIDs []string
}

// Load reads configuration from environment variables and returns a Config.
// It returns an error if any required variable is missing.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		StoragePath: os.Getenv("STORAGE_PATH"),
		Port:        os.Getenv("PORT"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
	}

	if v := os.Getenv("ALLOWED_DONGLE_IDS"); v != "" {
		for _, id := range strings.Split(v, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				cfg.AllowedDongleIDs = append(cfg.AllowedDongleIDs, id)
			}
		}
	}

	if cfg.StoragePath == "" {
		cfg.StoragePath = "./data"
	}

	if cfg.Port == "" {
		cfg.Port = "8080"
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("failed to load config: DATABASE_URL is required")
	}

	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("failed to load config: JWT_SECRET is required")
	}

	return cfg, nil
}

// IsDongleAllowed reports whether the given dongle_id is permitted to register.
// If no allowlist is configured, all dongle IDs are allowed.
func (c *Config) IsDongleAllowed(dongleID string) bool {
	if len(c.AllowedDongleIDs) == 0 {
		return true
	}
	for _, id := range c.AllowedDongleIDs {
		if id == dongleID {
			return true
		}
	}
	return false
}
