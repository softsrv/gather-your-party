package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLoadConfig_AllFourEnvVars asserts that loadConfig() surfaces all four
// boot-time environment variables (CLM-1).
func TestLoadConfig_AllFourEnvVars(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost/db")
	t.Setenv("APP_BASE_URL", "http://localhost:8080")
	t.Setenv("SESSION_SECRET", "supersecret")
	t.Setenv("STEAM_API_KEY", "AAAA1234BBBB5678")

	cfg := loadConfig()

	if cfg.databaseURL != "postgres://user:pass@localhost/db" {
		t.Errorf("databaseURL = %q, want %q", cfg.databaseURL, "postgres://user:pass@localhost/db")
	}
	if cfg.appBaseURL != "http://localhost:8080" {
		t.Errorf("appBaseURL = %q, want %q", cfg.appBaseURL, "http://localhost:8080")
	}
	if cfg.sessionSecret != "supersecret" {
		t.Errorf("sessionSecret = %q, want %q", cfg.sessionSecret, "supersecret")
	}
	if cfg.steamAPIKey != "AAAA1234BBBB5678" {
		t.Errorf("steamAPIKey = %q, want %q", cfg.steamAPIKey, "AAAA1234BBBB5678")
	}
}

// TestLoadConfig_EmptyByDefault asserts that loadConfig() returns empty strings
// when env vars are not set (so callers can gate on empty values).
func TestLoadConfig_EmptyByDefault(t *testing.T) {
	// Unset all four to ensure a clean environment for this sub-test.
	for _, k := range []string{"DATABASE_URL", "APP_BASE_URL", "SESSION_SECRET", "STEAM_API_KEY"} {
		os.Unsetenv(k)
	}

	cfg := loadConfig()

	if cfg.databaseURL != "" || cfg.appBaseURL != "" || cfg.sessionSecret != "" || cfg.steamAPIKey != "" {
		t.Errorf("expected all empty config fields, got %+v", cfg)
	}
}

// TestPoolWiring_ValidDSN asserts that pgxpool.New returns a non-nil pool and
// no error for a syntactically valid postgres:// DSN (CLM-3).
// pgxpool.New with a valid DSN does lazy-connect — no live DB is required.
func TestPoolWiring_ValidDSN(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@localhost:5432/db")
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v, want nil", err)
	}
	if pool == nil {
		t.Fatal("pgxpool.New() returned nil pool, want non-nil")
	}
	pool.Close()
}
