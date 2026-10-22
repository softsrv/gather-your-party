//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
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
	for _, table := range []string{"users", "sessions", "parties", "memberships", "invites", "games", "user_games", "schema_migrations"} {
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

// Startup no longer reads any Google credential: with GOOGLE_* unset and with
// junk values, boot reaches serve and GET / returns the same sign-in page.
func TestBootIgnoresGoogleCredentials(t *testing.T) {
	boot := func(t *testing.T, google map[string]string) (int, string) {
		t.Helper()
		ctx, _ := bootIntegrationPool(t)
		t.Chdir(t.TempDir())
		t.Setenv("SESSION_SECRET", "boot-secret")
		t.Setenv("APP_BASE_URL", "https://example.com")
		unsetBootEnv(t, "GOOGLE_CLIENT_ID")
		unsetBootEnv(t, "GOOGLE_CLIENT_SECRET")
		for key, value := range google {
			t.Setenv(key, value)
		}
		var status int
		var body string
		stopped := errors.New("server stopped")
		err := run(ctx, func(app *application) error {
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
			status, body = w.Code, w.Body.String()
			return stopped
		})
		if !errors.Is(err, stopped) {
			t.Fatalf("startup did not reach serve: %v", err)
		}
		return status, body
	}
	unsetStatus, unsetBody := boot(t, nil)
	junkStatus, junkBody := boot(t, map[string]string{"GOOGLE_CLIENT_ID": "junk-id", "GOOGLE_CLIENT_SECRET": "junk-secret"})
	if unsetStatus != http.StatusOK || junkStatus != unsetStatus {
		t.Fatalf("GET / status unset=%d junk=%d, want both %d", unsetStatus, junkStatus, http.StatusOK)
	}
	if !strings.Contains(unsetBody, `href="/auth/steam"`) || junkBody != unsetBody {
		t.Fatalf("GET / sign-in page differs or is missing:\nunset: %s\njunk: %s", unsetBody, junkBody)
	}
}

// Removing Google Calendar: a brand-new database gets no
// google_calendar_connections table, and schema_migrations records exactly the
// embedded migrations/*.sql files.
func TestMigrateFreshDatabaseHasNoGoogleCalendarTable(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	if err := migrate(ctx, pool, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('google_calendar_connections') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("google_calendar_connections exists = %v, error %v", exists, err)
	}
	embedded, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(embedded))
	for _, path := range embedded {
		want = append(want, strings.TrimPrefix(path, "migrations/"))
	}
	sort.Strings(want)
	rows, err := pool.Query(ctx, "SELECT filename FROM schema_migrations ORDER BY filename")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("schema_migrations = %v, want embedded files %v", got, want)
	}
	if slices.Contains(got, "0006_google_calendar_connections.sql") {
		t.Fatalf("0006 calendar migration still applied: %v", got)
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
	for _, filename := range []string{"0001_init.sql", "0002_parties.sql", "0003_drop_rejection_tallies.sql", "0004_games.sql", "0005_library_sync.sql"} {
		if before[filename].IsZero() {
			t.Fatalf("missing tracking row for %s", filename)
		}
	}
	if len(before) != 5 {
		t.Fatalf("applied %d migrations, want 5", len(before))
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

// Games CLM-4: the embedded runner creates games unique by app id and
// user_games unique per (user, game).
func TestMigrateCreatesUniqueGameTables(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	if err := migrate(ctx, pool, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
	var userID, gameID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full) VALUES ('76561198000009001', '', '', '', '') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO games (app_id, name) VALUES (440, 'Team Fortress 2') RETURNING id`).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	_, err := pool.Exec(ctx, `INSERT INTO games (app_id, name) VALUES (440, 'Duplicate')`)
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "games_app_id_key" {
		t.Fatalf("duplicate app id must violate games_app_id_key, got %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_games (user_id, game_id) VALUES ($1, $2)`, userID, gameID); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO user_games (user_id, game_id, position) VALUES ($1, $2, 1)`, userID, gameID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("duplicate (user, game) must be rejected, got %v", err)
	}
	var games, owned int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM games), (SELECT count(*) FROM user_games)`).Scan(&games, &owned); err != nil || games != 1 || owned != 1 {
		t.Fatalf("games=%d user_games=%d err=%v", games, owned, err)
	}
}

// Games CLM-15: the real runner upgrades a database whose users signed in
// before 0004 so those rows read ever_logged_in = true; later rows default false.
func TestMigrateMarksExistingUsersLoggedIn(t *testing.T) {
	ctx, pool := bootIntegrationPool(t)
	earlier := fstest.MapFS{}
	for _, name := range []string{"0001_init.sql", "0002_parties.sql", "0003_drop_rejection_tallies.sql"} {
		raw, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		earlier["migrations/"+name] = &fstest.MapFile{Data: raw}
	}
	if err := migrate(ctx, pool, earlier, "migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full) VALUES ('76561198000009101', 'Before', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, pool, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full) VALUES ('76561198000009102', 'After', '', '', '')`); err != nil {
		t.Fatal(err)
	}
	var before, after bool
	var state string
	var stateAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT ever_logged_in, game_sync_state, game_sync_state_at FROM users WHERE steam_id_64 = '76561198000009101'`).Scan(&before, &state, &stateAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT ever_logged_in FROM users WHERE steam_id_64 = '76561198000009102'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !before || after || state != "never_synced" || stateAt != nil {
		t.Fatalf("pre-existing ever_logged_in=%v state=%q at=%v; new row ever_logged_in=%v", before, state, stateAt, after)
	}
}
