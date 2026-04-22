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

	// Web UI authentication. Separate from the device-facing JWT auth that
	// protects /v1/* endpoints used by openpilot. SessionSecret gates whether
	// UI auth is enabled at all: if empty, login endpoints are disabled and a
	// warning is logged at startup. AdminUsername/AdminPassword, when both
	// set, bootstrap (or update) a row in ui_users on startup so the operator
	// can log in with env-configured credentials.
	SessionSecret string
	AdminUsername string
	AdminPassword string
}

// Load reads configuration from environment variables and returns a Config.
// It returns an error if any required variable is missing.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		StoragePath:   os.Getenv("STORAGE_PATH"),
		Port:          os.Getenv("PORT"),
		SessionSecret: os.Getenv("SESSION_SECRET"),
		AdminUsername: os.Getenv("ADMIN_USERNAME"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
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

// UIAuthEnabled reports whether the web UI authentication endpoints are
// active. It requires a non-empty SESSION_SECRET; without one, cookie
// signing has no trusted key and login is disabled entirely.
func (c *Config) UIAuthEnabled() bool {
	return c.SessionSecret != ""
}
