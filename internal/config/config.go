package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	DatabaseURL    string
	StoragePath    string
	Port           string
	AllowedSerials []string
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
