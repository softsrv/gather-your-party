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
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != member {
		t.Fatalf("successor=%d want=%d err=%v", gotLeader, member, err)
	}
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, other).Scan(&gotLeader); err != nil || gotLeader != leader {
		t.Fatalf("other party leadership changed: %d, %v", gotLeader, err)
	}
	if err := store.LeaveParty(ctx, party, member); err != nil {
		t.Fatal(err)
	}
	assertMembership(party, member, false)
	if err := store.LeaveParty(ctx, party, member); err != nil {
		t.Fatal(err)
	}
	// Empty-party cleanup is a separate operation; leaving must keep the party.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM parties WHERE id = $1)`, party).Scan(&exists); err != nil || !exists {
		t.Fatalf("empty party exists=%t err=%v", exists, err)
	}
	if err := store.LeaveParty(ctx, "invalid-uuid", leader); err == nil {
		t.Fatal("invalid UUID must return an error")
	}
}

// CLM-1: handing off leadership preserves the former leader's membership.
func TestStepDownKeepsFormerLeaderMembership(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	member := partyTestUser(t, store, ctx, "76561198000000002")
	party, err := store.CreateParty(ctx, leader, "Handoff")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
		t.Fatal(err)
	}
	if err := store.StepDown(ctx, party, leader, member); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != member {
		t.Fatalf("leader=%d want=%d err=%v", gotLeader, member, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, leader).Scan(&exists); err != nil || !exists {
		t.Fatalf("former leader membership exists=%t err=%v", exists, err)
	}
}

// CLM-2: a chosen member need not be the earliest-joined non-leader.
func TestStepDownAcceptsNonSeniorMember(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	senior := partyTestUser(t, store, ctx, "76561198000000002")
	chosen := partyTestUser(t, store, ctx, "76561198000000003")
	party, err := store.CreateParty(ctx, leader, "Choose any member")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memberships SET joined_at = '2025-01-01T00:00:00Z' WHERE party_id = $1`, party); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1, $2, '2025-01-02T00:00:00Z'), ($1, $3, '2025-01-03T00:00:00Z')`, party, senior, chosen); err != nil {
		t.Fatal(err)
	}
	if err := store.StepDown(ctx, party, leader, chosen); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != chosen {
		t.Fatalf("leader=%d want non-senior=%d err=%v", gotLeader, chosen, err)
	}
}

func TestStepDownRejectsInvalidHandoff(t *testing.T) {
	for _, test := range []struct {
		name       string
		soleMember bool
		actor      string
		target     string
	}{
		// Each guard is isolated: the other preconditions permit the handoff.
		{name: "CLM-3 non-leader actor", actor: "member", target: "member"},
		{name: "CLM-4 sole member targets self", soleMember: true, actor: "leader", target: "leader"},
		{name: "CLM-9 non-member target", actor: "leader", target: "outsider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, pool, ctx := partyTestStore(t)
			leader := partyTestUser(t, store, ctx, "76561198000000001")
			member := partyTestUser(t, store, ctx, "76561198000000002")
			outsider := partyTestUser(t, store, ctx, "76561198000000003")
			party, err := store.CreateParty(ctx, leader, "Rejected handoff")
			if err != nil {
				t.Fatal(err)
			}
			if !test.soleMember {
				if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
					t.Fatal(err)
				}
			}
			// Membership in a different party must not make the outsider eligible.
			if _, err := store.CreateParty(ctx, outsider, "Other party"); err != nil {
				t.Fatal(err)
			}
			users := map[string]int64{"leader": leader, "member": member, "outsider": outsider}
			if err := store.StepDown(ctx, party, users[test.actor], users[test.target]); err == nil {
				t.Fatal("invalid handoff must return an error")
			}
			var gotLeader int64
			if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leader {
				t.Fatalf("rejected handoff changed leader: got=%d want=%d err=%v", gotLeader, leader, err)
			}
		})
	}
}

// CLM-5 and CLM-6: succession selects the earliest-joined remaining member.
func TestLeavePartyLeaderSucceedsToEarliestRemainingMember(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	later := partyTestUser(t, store, ctx, "76561198000000002")
	senior := partyTestUser(t, store, ctx, "76561198000000003")
	party, err := store.CreateParty(ctx, leader, "Automatic succession")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memberships SET joined_at = '2025-01-01T00:00:00Z' WHERE party_id = $1`, party); err != nil {
		t.Fatal(err)
	}
	// Insert the later member first, with the smaller user ID, so neither
	// insertion order nor user ID order substitutes for joined_at ordering.
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1, $2, '2025-01-03T00:00:00Z'), ($1, $3, '2025-01-02T00:00:00Z')`, party, later, senior); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leader); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != senior {
		t.Fatalf("successor=%d want earliest remaining=%d err=%v", gotLeader, senior, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, gotLeader).Scan(&exists); err != nil || !exists {
		t.Fatalf("successor membership exists=%t err=%v", exists, err)
	}
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, leader).Scan(&exists); err != nil || exists {
		t.Fatalf("departed leader membership exists=%t err=%v", exists, err)
	}
}

// CLM-7: a non-leader leaving must not trigger succession.
func TestLeavePartyNonLeaderKeepsLeadership(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	leaver := partyTestUser(t, store, ctx, "76561198000000002")
	senior := partyTestUser(t, store, ctx, "76561198000000003")
	party, err := store.CreateParty(ctx, leader, "No succession")
	if err != nil {
		t.Fatal(err)
	}
	// A leader can be newer than other members after an explicit handoff.
	// Make another remaining member older so unconditional succession fails.
	if _, err := pool.Exec(ctx, `UPDATE memberships SET joined_at = '2025-01-03T00:00:00Z' WHERE party_id = $1`, party); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1, $2, '2025-01-02T00:00:00Z'), ($1, $3, '2025-01-01T00:00:00Z')`, party, leaver, senior); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leaver); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leader {
		t.Fatalf("non-leader departure changed leader: got=%d want=%d err=%v", gotLeader, leader, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, leaver).Scan(&exists); err != nil || exists {
		t.Fatalf("departed member membership exists=%t err=%v", exists, err)
	}
}

func TestLeavePartyRollsBackSuccessionFailure(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000001")
	member := partyTestUser(t, store, ctx, "76561198000000002")
	party, err := store.CreateParty(ctx, leader, "Atomic succession")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
		t.Fatal(err)
	}
	// Force the leadership update to fail AFTER membership removal.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE parties ADD CONSTRAINT reject_test_successor CHECK (leader_id <> %d)`, member)); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leader); err == nil {
		t.Fatal("failed succession must return an error")
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leader {
		t.Fatalf("failed succession changed leader: %d, %v", gotLeader, err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, leader).Scan(&exists); err != nil || !exists {
		t.Fatalf("failed succession removed leader membership: exists=%t err=%v", exists, err)
	}
}
