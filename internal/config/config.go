package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	DatabaseURL    string
	StoragePath    string
	Port           string
	AllowedSerials []string
	// RetentionDays is the default retention window for non-preserved routes
	// in days. 0 means "never delete". At runtime this value is used as a
	// fallback when the settings table does not contain a retention_days
	// override.
	RetentionDays int
}

// Load reads configuration from environment variables and returns a Config.
// It returns an error if any required variable is missing.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		StoragePath: os.Getenv("STORAGE_PATH"),
		Port:        os.Getenv("PORT"),
	}

	if v := os.Getenv("ALLOWED_SERIALS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				cfg.AllowedSerials = append(cfg.AllowedSerials, s)
			}
		}
	}

	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("failed to load config: RETENTION_DAYS must be an integer, got %q", v)
		}
		if n < 0 {
			return nil, fmt.Errorf("failed to load config: RETENTION_DAYS must be >= 0, got %d", n)
		}
		cfg.RetentionDays = n
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

	return cfg, nil
}

// IsSerialAllowed reports whether the given device serial is permitted to
// register. If no allowlist is configured, all serials are allowed.
func (c *Config) IsSerialAllowed(serial string) bool {
	if len(c.AllowedSerials) == 0 {
		return true
	}
	for _, s := range c.AllowedSerials {
		if s == serial {
			return true
		}
	}
	return false
}
