//go:build integration

package db

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

func partyTestStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
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
	schema := fmt.Sprintf("party_test_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	t.Cleanup(pool.Close)
	for _, path := range []string{"../../migrations/0001_init.sql", "../../migrations/0002_parties.sql"} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	return New(pool), pool, ctx
}

func partyTestUser(t *testing.T, store *Store, ctx context.Context, steamID string) int64 {
	t.Helper()
	id, err := store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: "Party member"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreatePartyLeaderAndSoleMember(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	id, err := store.CreateParty(ctx, leader, "Friday games")
	if err != nil {
		t.Fatal(err)
	}
	var uuid pgtype.UUID
	if err := uuid.Scan(id); err != nil || !uuid.Valid {
		t.Fatalf("invalid UUID: %q", id)
	}
	var gotLeader, member int64
	var name string
	var joined time.Time
	if err := pool.QueryRow(ctx, `SELECT p.leader_id, p.name, m.user_id, m.joined_at FROM parties p JOIN memberships m ON m.party_id = p.id WHERE p.id = $1`, id).Scan(&gotLeader, &name, &member, &joined); err != nil {
		t.Fatal(err)
	}
	if gotLeader != leader || member != leader || name != "Friday games" || joined.IsZero() {
		t.Fatal("creator must be leader and a timestamped member")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("members=%d, err=%v", count, err)
	}
	second, err := store.CreateParty(ctx, leader, "Friday games")
	if err != nil || second == id || second == "" {
		t.Fatalf("fresh party=%q, err=%v", second, err)
	}
}

func TestCreatePartyRollsBackMembershipFailure(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	// Inject a real PostgreSQL failure on the SECOND insert. If the first insert
	// autocommits, the party row survives and this test fails.
	if _, err := pool.Exec(ctx, `ALTER TABLE memberships ADD CONSTRAINT reject_test_membership CHECK (user_id < 0)`); err != nil {
		t.Fatal(err)
	}
	id, err := store.CreateParty(ctx, leader, "Must roll back")
	if err == nil || id != "" {
		t.Fatalf("failed creation returned %q, %v", id, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parties`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial party survived: %d, %v", count, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE memberships DROP CONSTRAINT reject_test_membership`); err != nil {
		t.Fatal(err)
	}
	if id, err := store.CreateParty(ctx, -1, "Unknown creator"); err == nil || id != "" {
		t.Fatal("invalid creator accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if id, err := store.CreateParty(cancelled, leader, "Cancelled"); err == nil || id != "" {
		t.Fatal("cancelled create accepted")
	}
}

func TestLeavePartyOnlyActingMembershipIncludingLeader(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	member := partyTestUser(t, store, ctx, "76561198000000002")
	party, err := store.CreateParty(ctx, leader, "First")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateParty(ctx, leader, "Second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leader); err != nil {
		t.Fatal(err)
	}
	assertMembership := func(partyID string, userID int64, want bool) {
		t.Helper()
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, partyID, userID).Scan(&exists); err != nil || exists != want {
			t.Fatalf("membership exists=%t want=%t err=%v", exists, want, err)
		}
	}
	assertMembership(party, leader, false)
	assertMembership(party, member, true)
	assertMembership(other, leader, true)
	if err := store.LeaveParty(ctx, party, member); err != nil {
		t.Fatal(err)
	}
	assertMembership(party, member, false)
	if err := store.LeaveParty(ctx, party, member); err != nil {
		t.Fatal(err)
	}
	// This slice deliberately does not change leadership or remove empty parties.
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leader {
		t.Fatalf("party or leadership changed: %d, %v", gotLeader, err)
	}
	if err := store.LeaveParty(ctx, "invalid-uuid", leader); err == nil {
		t.Fatal("invalid UUID must return an error")
	}
}
