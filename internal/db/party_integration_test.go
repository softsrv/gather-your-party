//go:build integration

package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Use a private schema, as in TestLogoutInvalidatesSession, so TestPersistence
// keeps its original migration and cleanup and can never touch these tables.
func TestPartyLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name an empty, disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	defer admin.Close()
	schema := fmt.Sprintf("party_test_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	defer pool.Close()
	for _, path := range []string{"../../migrations/0001_init.sql", "../../migrations/0002_parties.sql"} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	store := New(pool)
	users := make([]int64, 4)
	for i := range users {
		steamID := fmt.Sprintf("7656119800000000%d", i+1)
		id, err := store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: "Member"})
		if err != nil {
			t.Fatal(err)
		}
		resolved, found, err := store.ResolveUserID(ctx, steamID)
		if err != nil || !found || resolved != id {
			t.Fatalf("resolve user = %d, %t, %v", resolved, found, err)
		}
		users[i] = resolved
	}
	create := func(t *testing.T) string {
		t.Helper()
		id, err := store.CreateParty(ctx, "Game night", users[0])
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	assertCount := func(t *testing.T, want int, sql string, args ...any) {
		t.Helper()
		var got int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&got); err != nil || got != want {
			t.Fatalf("count = %d, want %d, error %v", got, want, err)
		}
	}
	assertLeader := func(t *testing.T, partyID string, want int64) {
		t.Helper()
		var leader int64
		if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, partyID).Scan(&leader); err != nil || leader != want {
			t.Fatalf("leader = %d, want %d, error %v", leader, want, err)
		}
	}
	addMembers := func(t *testing.T, id string) {
		t.Helper()
		// Deliberately insert the junior member first; seniority is not row order.
		exec(t, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1, $2, '2024-02-01'), ($1, $3, '2024-01-01')`, id, users[2], users[1])
	}

	t.Run("create sole member and schema", func(t *testing.T) {
		id := create(t)
		assertLeader(t, id, users[0])
		assertCount(t, 1, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
		assertCount(t, 1, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2 AND joined_at IS NOT NULL`, id, users[0])
		second := create(t)
		if id == second {
			t.Fatal("same-named parties must have distinct UUIDs")
		}
		var idType string
		if err := pool.QueryRow(ctx, `SELECT pg_typeof(id)::text FROM parties WHERE id = $1`, id).Scan(&idType); err != nil || idType != "uuid" {
			t.Fatalf("party id type = %q, error %v", idType, err)
		}
		_, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1, $2, NULL)`, id, users[1])
		assertSQLState(t, err, "23502")
		_, err = pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, id, users[0])
		assertSQLState(t, err, "23505")
		_, err = pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, -1)`, id)
		assertSQLState(t, err, "23503")
		failedID, err := store.CreateParty(ctx, "Invalid leader", -1)
		assertSQLState(t, err, "23503")
		if failedID != "" {
			t.Fatal("failed creation returned an id")
		}
		assertCount(t, 0, `SELECT count(*) FROM parties WHERE name = 'Invalid leader'`)
	})

	t.Run("cascade and fresh rejection history", func(t *testing.T) {
		id := create(t)
		exec(t, `INSERT INTO invites (party_id, user_id, inviter_id) VALUES ($1, $2, $3)`, id, users[1], users[0])
		exec(t, `INSERT INTO rejection_tallies (party_id, user_id, count) VALUES ($1, $2, 3)`, id, users[1])
		// Session churn must not erase durable rejection history.
		token, err := store.CreateSession(ctx, users[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteSession(ctx, token); err != nil {
			t.Fatal(err)
		}
		assertCount(t, 3, `SELECT count FROM rejection_tallies WHERE party_id = $1 AND user_id = $2`, id, users[1])
		_, err = pool.Exec(ctx, `INSERT INTO rejection_tallies (party_id, user_id) VALUES ($1, $2)`, id, users[1])
		assertSQLState(t, err, "23505")
		exec(t, `DELETE FROM parties WHERE id = $1`, id)
		for _, table := range []string{"memberships", "invites", "rejection_tallies"} {
			assertCount(t, 0, `SELECT count(*) FROM `+table+` WHERE party_id = $1`, id)
		}
		fresh := create(t)
		if fresh == id {
			t.Fatal("new party reused previous UUID")
		}
		assertCount(t, 0, `SELECT count(*) FROM rejection_tallies WHERE party_id = $1 AND user_id = $2`, fresh, users[1])
	})

	t.Run("leader and member leave with automatic succession", func(t *testing.T) {
		id := create(t)
		otherParty := create(t)
		addMembers(t, id)
		exec(t, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, id, users[3])
		if err := store.LeaveParty(ctx, id, users[0]); err != nil {
			t.Fatal(err)
		}
		assertCount(t, 0, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, users[0])
		assertCount(t, 3, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
		assertCount(t, 1, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, otherParty, users[0])
		assertLeader(t, id, users[1])
		if err := store.LeaveParty(ctx, id, users[2]); err != nil {
			t.Fatal(err)
		}
		assertCount(t, 0, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, users[2])
		assertCount(t, 2, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
		assertLeader(t, id, users[1])
		if err := store.LeaveParty(ctx, id, users[0]); err != nil {
			t.Fatal(err)
		}
		assertCount(t, 2, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
		assertLeader(t, id, users[1])
	})

	t.Run("chosen junior member and refusal cases", func(t *testing.T) {
		id := create(t)
		addMembers(t, id)
		for _, pair := range [][2]int64{{users[1], users[2]}, {users[0], users[3]}} {
			if err := store.StepDown(ctx, id, pair[0], pair[1]); !errors.Is(err, ErrStepDownNotAllowed) {
				t.Fatalf("unauthorized step down = %v", err)
			}
			assertLeader(t, id, users[0])
		}
		if err := store.StepDown(ctx, id, users[0], users[2]); err != nil {
			t.Fatal(err)
		}
		assertLeader(t, id, users[2])
		assertCount(t, 1, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, id, users[0])
		assertCount(t, 3, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
	})

	t.Run("size one refuses step down but permits silent leave", func(t *testing.T) {
		id := create(t)
		if err := store.StepDown(ctx, id, users[0], users[0]); !errors.Is(err, ErrStepDownNotAllowed) {
			t.Fatalf("size-one step down = %v", err)
		}
		assertLeader(t, id, users[0])
		assertCount(t, 1, `SELECT count(*) FROM memberships WHERE party_id = $1`, id)
		exec(t, `INSERT INTO invites (party_id, user_id, inviter_id) VALUES ($1, $2, $3)`, id, users[1], users[0])
		exec(t, `INSERT INTO rejection_tallies (party_id, user_id) VALUES ($1, $2)`, id, users[1])
		if err := store.LeaveParty(ctx, id, users[0]); err != nil {
			t.Fatalf("last leave returned a distinct failure: %v", err)
		}
		assertCount(t, 0, `SELECT count(*) FROM parties WHERE id = $1`, id)
		for _, table := range []string{"memberships", "invites", "rejection_tallies"} {
			assertCount(t, 0, `SELECT count(*) FROM `+table+` WHERE party_id = $1`, id)
		}
		if err := store.LeaveParty(ctx, id, users[0]); err != nil {
			t.Fatal(err)
		}
		if err := store.StepDown(ctx, id, users[0], users[1]); !errors.Is(err, ErrStepDownNotAllowed) {
			t.Fatalf("missing party step down = %v", err)
		}
	})
}
