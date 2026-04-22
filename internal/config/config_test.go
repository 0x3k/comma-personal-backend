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
	"ALLOWED_SERIALS",
	"SESSION_SECRET",
	"ADMIN_USERNAME",
	"ADMIN_PASSWORD",
	"RETENTION_DAYS",
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
	t.Setenv("STORAGE_PATH", "/tmp/storage")
	t.Setenv("PORT", "9090")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}

	if cfg.DatabaseURL != "postgres://localhost:5432/testdb" {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, "postgres://localhost:5432/testdb")
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

	_, err := Load()
	if err == nil {
		t.Fatal("Load() returned nil error when DATABASE_URL is missing, want error")
	}
}

func TestLoad_DefaultPort(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")

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

func TestLoad_AllowedSerials(t *testing.T) {
	tests := []struct {
		name        string
		envVal      string
		wantSerials []string
	}{
		{
			name:        "single serial",
			envVal:      "SERIAL001",
			wantSerials: []string{"SERIAL001"},
		},
		{
			name:        "multiple serials",
			envVal:      "SERIAL001,SERIAL002,SERIAL003",
			wantSerials: []string{"SERIAL001", "SERIAL002", "SERIAL003"},
		},
		{
			name:        "whitespace trimmed",
			envVal:      " SERIAL001 , SERIAL002 , SERIAL003 ",
			wantSerials: []string{"SERIAL001", "SERIAL002", "SERIAL003"},
		},
		{
			name:        "empty value",
			envVal:      "",
			wantSerials: nil,
		},
		{
			name:        "trailing comma ignored",
			envVal:      "SERIAL001,",
			wantSerials: []string{"SERIAL001"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")

			if tt.envVal != "" {
				t.Setenv("ALLOWED_SERIALS", tt.envVal)
			}

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned unexpected error: %v", err)
			}

			if len(cfg.AllowedSerials) != len(tt.wantSerials) {
				t.Fatalf("AllowedSerials has %d entries, want %d: got %v",
					len(cfg.AllowedSerials), len(tt.wantSerials), cfg.AllowedSerials)
			}
			for i, want := range tt.wantSerials {
				if cfg.AllowedSerials[i] != want {
					t.Errorf("AllowedSerials[%d] = %q, want %q", i, cfg.AllowedSerials[i], want)
				}
			}
		})
	}
}

func TestUIAuthEnabled(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		want   bool
	}{
		{"empty disables UI auth", "", false},
		{"non-empty enables UI auth", "some-secret", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{SessionSecret: tt.secret}
			if got := cfg.UIAuthEnabled(); got != tt.want {
				t.Errorf("UIAuthEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoad_RetentionDays(t *testing.T) {
	tests := []struct {
		name    string
		envVal  string
		want    int
		wantErr bool
	}{
		{
			name:   "unset defaults to zero",
			envVal: "",
			want:   0,
		},
		{
			name:   "positive integer",
			envVal: "30",
			want:   30,
		},
		{
			name:   "explicit zero",
			envVal: "0",
			want:   0,
		},
		{
			name:   "whitespace trimmed",
			envVal: "  14  ",
			want:   14,
		},
		{
			name:    "non-integer rejected",
			envVal:  "abc",
			wantErr: true,
		},
		{
			name:    "negative rejected",
			envVal:  "-1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")

			if tt.envVal != "" {
				t.Setenv("RETENTION_DAYS", tt.envVal)
			}

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load() = nil error, want error for RETENTION_DAYS=%q", tt.envVal)
				}
				return
			}

			if err != nil {
				t.Fatalf("Load() returned unexpected error: %v", err)
			}
			if cfg.RetentionDays != tt.want {
				t.Errorf("RetentionDays = %d, want %d", cfg.RetentionDays, tt.want)
			}
		})
	}
}

func TestLoad_SessionEnvVars(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/testdb")
	t.Setenv("SESSION_SECRET", "shh")
	t.Setenv("ADMIN_USERNAME", "admin")
	t.Setenv("ADMIN_PASSWORD", "hunter2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned unexpected error: %v", err)
	}
	if cfg.SessionSecret != "shh" {
		t.Errorf("SessionSecret = %q, want %q", cfg.SessionSecret, "shh")
	}
	if cfg.AdminUsername != "admin" {
		t.Errorf("AdminUsername = %q, want %q", cfg.AdminUsername, "admin")
	}
	if cfg.AdminPassword != "hunter2" {
		t.Errorf("AdminPassword = %q, want %q", cfg.AdminPassword, "hunter2")
	}
	if !cfg.UIAuthEnabled() {
		t.Error("UIAuthEnabled() = false, want true when SESSION_SECRET is set")
	}
}

func TestIsSerialAllowed(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		serial  string
		want    bool
	}{
		{
			name:    "empty allowlist permits all",
			allowed: nil,
			serial:  "anything",
			want:    true,
		},
		{
			name:    "serial in allowlist",
			allowed: []string{"SERIAL001", "SERIAL002"},
			serial:  "SERIAL001",
			want:    true,
		},
		{
			name:    "serial not in allowlist",
			allowed: []string{"SERIAL001", "SERIAL002"},
			serial:  "unknown",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{AllowedSerials: tt.allowed}
			got := cfg.IsSerialAllowed(tt.serial)
			if got != tt.want {
				t.Errorf("IsSerialAllowed(%q) = %v, want %v", tt.serial, got, tt.want)
			}
		})
	}
}
