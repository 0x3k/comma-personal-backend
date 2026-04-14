package config

import (
	"fmt"
	"os"
)

// Config holds the application configuration loaded from environment variables.
type Config struct {
	DatabaseURL string
	StoragePath string
	Port        string
	JWTSecret   string
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
