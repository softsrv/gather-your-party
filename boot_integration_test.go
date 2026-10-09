//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Mirror the existing integration tests' schema isolation, but do not pre-apply
// migrations: the production boot path is responsible for those in these tests.
func bootIntegrationPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name a disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	t.Cleanup(admin.Close)
	schema := pgx.Identifier{fmt.Sprintf("boot_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	// PGOPTIONS scopes all connections opened by connect(), for both URL and
	// keyword/value TEST_DATABASE_URL formats, without rewriting credentials.
	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	t.Setenv("DATABASE_URL", dsn)
	pool, err := connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var currentSchema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&currentSchema); err != nil || (pgx.Identifier{currentSchema}).Sanitize() != schema {
		t.Fatal("test connection must use its isolated schema")
	}
	return ctx, pool
}

func assertBootSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"users", "sessions", "parties", "memberships", "invites", "schema_migrations"} {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s exists = %v, error %v", table, exists, err)
		}
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('rejection_tallies') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("rejection_tallies exists = %v, error %v", exists, err)
	}
}

// CLM-9/10: real startup, with the observation made at the actual serve boundary.
func TestBootConnectsAndMigratesBeforeServing(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	// The deployed binary has no migrations directory beside it.
	t.Chdir(t.TempDir())
	t.Setenv("SESSION_SECRET", "boot-secret")
	t.Setenv("APP_BASE_URL", "https://example.com")
	served := false
	stopped := errors.New("server stopped")
	err := run(ctx, func(app *application) error {
		served = true
		assertBootSchema(t, ctx, pool)
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			t.Fatalf("query after boot = %d, error %v", one, err)
		}
		if app.store == nil || app.config.SessionSecret != "boot-secret" || app.config.AppBaseURL != "https://example.com" {
			t.Fatal("boot did not initialize application")
		}
		return stopped
	})
	if !served || !errors.Is(err, stopped) {
		t.Fatalf("served = %v, error %v", served, err)
	}
}

// CLM-1: the second run leaves exactly the original tracking rows/timestamps.
func TestMigrateSkipsAppliedFiles(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	if err := migrate(ctx, pool, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
	assertBootSchema(t, ctx, pool)
	applied := func() map[string]time.Time {
		t.Helper()
		rows, err := pool.Query(ctx, "SELECT filename, applied_at FROM schema_migrations")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := make(map[string]time.Time)
		for rows.Next() {
			var filename string
			var at time.Time
			if err := rows.Scan(&filename, &at); err != nil {
				t.Fatal(err)
			}
			result[filename] = at
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := applied()
	for _, filename := range []string{"0001_init.sql", "0002_parties.sql", "0003_drop_rejection_tallies.sql"} {
		if before[filename].IsZero() {
			t.Fatalf("missing tracking row for %s", filename)
		}
	}
	if len(before) != 3 {
		t.Fatalf("applied %d migrations, want 3", len(before))
	}
	if err := migrate(ctx, pool, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
	if after := applied(); !reflect.DeepEqual(after, before) {
		t.Fatalf("second migration run changed tracking rows: before %v, after %v", before, after)
	}
	// Prove that the skip guard matters: the unmodified first SQL file collides.
	raw, err := migrationFiles.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, err = conn.Exec(ctx, string(raw))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P07" {
		t.Fatalf("blind migration must fail with duplicate_table, got %v", err)
	}
	// Raw 0001 begins a transaction; clear its failed transaction before release.
	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateOrdersNewFilesAndRollsBackFailure(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	files := fstest.MapFS{
		"migrations/003_fail.sql":   {Data: []byte("BEGIN; CREATE TABLE rolled_back (id int); SELECT 1/0; COMMIT;")},
		"migrations/002_insert.sql": {Data: []byte("INSERT INTO ordered VALUES (1);")},
		"migrations/001_create.sql": {Data: []byte("BEGIN; CREATE TABLE ordered (id int); COMMIT;")},
		"migrations/notes.txt":      {Data: []byte("not SQL")},
	}
	if err := migrate(ctx, pool, files, "migrations"); err == nil {
		t.Fatal("invalid migration must fail")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ordered").Scan(&count); err != nil || count != 1 {
		t.Fatalf("ordered migration count = %d, error %v", count, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('rolled_back') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration left a table: %v, error %v", exists, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 2 {
		t.Fatalf("tracking count = %d, error %v", count, err)
	}
	files["migrations/003_fail.sql"].Data = []byte("CREATE TABLE recovered (id int);")
	if err := migrate(ctx, pool, files, "migrations"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 3 {
		t.Fatalf("retry tracking count = %d, error %v", count, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ordered").Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry reran an applied migration: count %d, error %v", count, err)
	}
}

func TestMigrateRollsBackSQLWhenTrackingFails(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (
		filename text PRIMARY KEY CHECK (filename <> '0001_init.sql'),
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, pool, migrationFiles, "migrations"); err == nil {
		t.Fatal("tracking failure must fail the migration")
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('users') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("SQL committed without tracking: users exists = %v, error %v", exists, err)
	}
}

func TestBootDoesNotServeAfterMigrationFailure(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	t.Setenv("SESSION_SECRET", "boot-secret")
	t.Setenv("APP_BASE_URL", "https://example.com")
	if _, err := pool.Exec(ctx, "CREATE TABLE users (id int)"); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, func(*application) error {
		t.Fatal("failed migrations must prevent serving")
		return nil
	}); err == nil {
		t.Fatal("migration failure must return an error")
	}
}

func TestConnectRequiresWorkingDatabase(t *testing.T) {
	ctx, _ := bootIntegrationPool(t)
	// The real server rejects this startup option; lazy pool creation alone
	// would incorrectly succeed without testing the connection.
	t.Setenv("PGOPTIONS", "-c invalid_boot_setting=1")
	pool, err := connect(ctx)
	if pool != nil {
		pool.Close()
		t.Fatal("connect must not return an unconnected pool")
	}
	if err == nil || err.Error() != "unable to connect to database" {
		t.Fatal("expected credential-free connection failure")
	}
}
