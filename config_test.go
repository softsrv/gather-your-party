package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// t.Setenv registers restoration before unsetting, since godotenv deliberately
// leaves even an empty-but-present process variable unchanged.
func unsetBootEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEnvExplicitPath(t *testing.T) {
	t.Chdir(t.TempDir())
	filename := filepath.Join(t.TempDir(), "mounted.env")
	if err := os.WriteFile(filename, []byte("SESSION_SECRET=mounted-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_FILE_PATH", filename)
	unsetBootEnv(t, "SESSION_SECRET")
	loadEnv()
	if got := os.Getenv("SESSION_SECRET"); got != "mounted-secret" {
		t.Fatalf("SESSION_SECRET = %q", got)
	}
}

func TestLoadEnvDefaultFile(t *testing.T) {
	for _, mode := range []string{"unset", "empty"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("ENV_FILE_PATH", "")
			if mode == "unset" {
				unsetBootEnv(t, "ENV_FILE_PATH")
			}
			unsetBootEnv(t, "SESSION_SECRET")
			if err := os.WriteFile(".env", []byte("SESSION_SECRET=local-secret\n"), 0600); err != nil {
				t.Fatal(err)
			}
			loadEnv()
			if got := os.Getenv("SESSION_SECRET"); got != "local-secret" {
				t.Fatalf("SESSION_SECRET = %q", got)
			}
		})
	}
}

func TestLoadEnvMissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetBootEnv(t, "ENV_FILE_PATH")
	unsetBootEnv(t, "SESSION_SECRET")
	loadEnv()
	if _, ok := os.LookupEnv("SESSION_SECRET"); ok {
		t.Fatal("missing .env must not set SESSION_SECRET")
	}
	t.Setenv("ENV_FILE_PATH", "missing.env")
	loadEnv()
}

func TestLoadEnvPreservesEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("SESSION_SECRET=file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_FILE_PATH", "")
	t.Setenv("SESSION_SECRET", "process-secret")
	loadEnv()
	if got := os.Getenv("SESSION_SECRET"); got != "process-secret" {
		t.Fatalf("SESSION_SECRET = %q", got)
	}
}

func TestRunRejectsInvalidConfigBeforeServing(t *testing.T) {
	for _, config := range []appConfig{
		{AppBaseURL: "https://example.com"},
		{SessionSecret: "test-secret", AppBaseURL: "/relative"},
		{SessionSecret: "test-secret", AppBaseURL: "ftp://example.com"},
		{SessionSecret: "test-secret", AppBaseURL: "://invalid"},
	} {
		t.Setenv("SESSION_SECRET", config.SessionSecret)
		t.Setenv("APP_BASE_URL", config.AppBaseURL)
		if err := run(context.Background(), func(*application) error {
			t.Fatal("invalid configuration must not serve")
			return nil
		}); err == nil {
			t.Fatal("invalid configuration must return an error")
		}
	}
}

func TestRunRejectsDatabaseFailureBeforeServing(t *testing.T) {
	t.Setenv("SESSION_SECRET", "test-secret")
	t.Setenv("APP_BASE_URL", "https://example.com")
	t.Setenv("DATABASE_URL", "postgres://user:secret@localhost:invalid/db")
	if err := run(context.Background(), func(*application) error {
		t.Fatal("failed database connection must prevent serving")
		return nil
	}); err == nil {
		t.Fatal("database failure must return an error")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "invalid-port")
	app := application{}
	if err := app.serve(); err == nil {
		t.Fatal("invalid listener address must return an error")
	}
}

func TestConnectRejectsInvalidURLWithoutCredentials(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:secret@localhost:invalid/db")
	pool, err := connect(context.Background())
	if pool != nil {
		pool.Close()
		t.Fatal("invalid URL returned a pool")
	}
	if err == nil || err.Error() != "unable to initialize database pool" {
		t.Fatal("expected credential-free pool configuration error")
	}
}
