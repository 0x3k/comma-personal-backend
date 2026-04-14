package config

import (
	"os"
	"testing"
)

// configEnvVars lists every environment variable that Load reads.
// Each subtest unsets all of them before setting test-specific values,
// so no state leaks between cases.
var configEnvVars = []string{
	"DATABASE_URL",
	"STORAGE_PATH",
	"PORT",
	"JWT_SECRET",
	"ALLOWED_DONGLE_IDS",
}

// clearConfigEnv unsets all config-related environment variables.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range configEnvVars {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func TestLoad_AllRequiredSet(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")
	t.Setenv("JWT_SECRET", "supersecret")
	t.Setenv("STORAGE_PATH", "/tmp/storage")
	t.Setenv("PORT", "9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.DatabaseURL != "postgres://localhost:5432/testdb" {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, "postgres://localhost:5432/testdb")
	}
	if cfg.JWTSecret != "supersecret" {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, "supersecret")
	}
	if cfg.StoragePath != "/tmp/storage" {
		t.Errorf("StoragePath = %q, want %q", cfg.StoragePath, "/tmp/storage")
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q, want %q", cfg.Port, "9090")
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JWT_SECRET", "secret")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() returned nil error when DATABASE_URL is missing, want error")
	}
}

func TestLoad_MissingJWTSecret(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() returned nil error when JWT_SECRET is missing, want error")
	}
}

func TestLoad_DefaultPort(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")
	t.Setenv("JWT_SECRET", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want default %q", cfg.Port, "8080")
	}
}

func TestLoad_DefaultStoragePath(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")
	t.Setenv("JWT_SECRET", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.StoragePath != "./data" {
		t.Errorf("StoragePath = %q, want default %q", cfg.StoragePath, "./data")
	}
}

func TestLoad_CustomPortAndStoragePath(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("PORT", "3000")
	t.Setenv("STORAGE_PATH", "/mnt/data/comma")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.Port != "3000" {
		t.Errorf("Port = %q, want %q", cfg.Port, "3000")
	}
	if cfg.StoragePath != "/mnt/data/comma" {
		t.Errorf("StoragePath = %q, want %q", cfg.StoragePath, "/mnt/data/comma")
	}
}

func TestLoad_AllMissing(t *testing.T) {
	clearConfigEnv(t)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() returned nil error when all env vars are missing, want error")
	}
}

func TestLoad_AllowedDongleIDs(t *testing.T) {
	tests := []struct {
		name    string
		envVal  string
		wantIDs []string
	}{
		{
			name:    "single dongle ID",
			envVal:  "abc123",
			wantIDs: []string{"abc123"},
		},
		{
			name:    "multiple dongle IDs",
			envVal:  "abc123,def456,ghi789",
			wantIDs: []string{"abc123", "def456", "ghi789"},
		},
		{
			name:    "whitespace trimmed",
			envVal:  " abc123 , def456 , ghi789 ",
			wantIDs: []string{"abc123", "def456", "ghi789"},
		},
		{
			name:    "empty value",
			envVal:  "",
			wantIDs: nil,
		},
		{
			name:    "trailing comma ignored",
			envVal:  "abc123,",
			wantIDs: []string{"abc123"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")
			t.Setenv("JWT_SECRET", "secret")

			if tt.envVal != "" {
				t.Setenv("ALLOWED_DONGLE_IDS", tt.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned unexpected error: %v", err)
			}

			if len(cfg.AllowedDongleIDs) != len(tt.wantIDs) {
				t.Fatalf("AllowedDongleIDs has %d entries, want %d: got %v",
					len(cfg.AllowedDongleIDs), len(tt.wantIDs), cfg.AllowedDongleIDs)
			}
			for i, want := range tt.wantIDs {
				if cfg.AllowedDongleIDs[i] != want {
					t.Errorf("AllowedDongleIDs[%d] = %q, want %q", i, cfg.AllowedDongleIDs[i], want)
				}
			}
		})
	}
}

func TestIsDongleAllowed(t *testing.T) {
	tests := []struct {
		name     string
		allowed  []string
		dongleID string
		want     bool
	}{
		{
			name:     "empty allowlist permits all",
			allowed:  nil,
			dongleID: "anything",
			want:     true,
		},
		{
			name:     "dongle in allowlist",
			allowed:  []string{"abc123", "def456"},
			dongleID: "abc123",
			want:     true,
		},
		{
			name:     "dongle not in allowlist",
			allowed:  []string{"abc123", "def456"},
			dongleID: "unknown",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{AllowedDongleIDs: tt.allowed}
			got := cfg.IsDongleAllowed(tt.dongleID)
			if got != tt.want {
				t.Errorf("IsDongleAllowed(%q) = %v, want %v", tt.dongleID, got, tt.want)
			}
		})
	}
}
