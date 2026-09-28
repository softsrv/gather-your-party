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
	// This slice deliberately does not remove empty parties; the party has no
	// members left so leader_id is untouched (no successor to promote).
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leader {
		t.Fatalf("party or leadership changed: %d, %v", gotLeader, err)
	}
	if err := store.LeaveParty(ctx, "invalid-uuid", leader); err == nil {
		t.Fatal("invalid UUID must return an error")
	}
}

// [CLM-13] When the leader leaves a party with remaining members, leadership
// passes automatically to the earliest-joined remaining member.
func TestLeavePartySuccessionToEarliestJoinedRemainingMember(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000010")
	b := partyTestUser(t, store, ctx, "76561198000000011")
	c := partyTestUser(t, store, ctx, "76561198000000012")
	party, err := store.CreateParty(ctx, leader, "Succession")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1,$2,$3)`, party, b, now.Add(1*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1,$2,$3)`, party, c, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leader); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil {
		t.Fatal(err)
	}
	if gotLeader != b {
		t.Fatalf("leader_id = %d, want earliest-joined remaining member %d", gotLeader, b)
	}
}

// [CLM-14] When a non-leader member leaves, leadership is unchanged.
func TestLeavePartyNonLeaderDeparture(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000020")
	member := partyTestUser(t, store, ctx, "76561198000000021")
	party, err := store.CreateParty(ctx, leader, "Non-leader leaves")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, member); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil {
		t.Fatal(err)
	}
	if gotLeader != leader {
		t.Fatalf("leader_id = %d, want unchanged leader %d", gotLeader, leader)
	}
}

// [CLM-12] joined_at totally orders a party's members by seniority.
func TestMembershipsOrderedByJoinedAtReflectsSeniority(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000030")
	b := partyTestUser(t, store, ctx, "76561198000000031")
	c := partyTestUser(t, store, ctx, "76561198000000032")
	party, err := store.CreateParty(ctx, leader, "Ordering")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1,$2,$3)`, party, c, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1,$2,$3)`, party, b, now.Add(1*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT user_id FROM memberships WHERE party_id = $1 ORDER BY joined_at, user_id`, party)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		order = append(order, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []int64{leader, b, c}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// [CLM-2] StepDown sets leader_id to the target while the former leader
// remains an ordinary member; no membership rows are added or removed.
func TestStepDownTransfersLeadershipKeepsFormerLeaderAsMember(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leaderA := partyTestUser(t, store, ctx, "76561198000000040")
	memberB := partyTestUser(t, store, ctx, "76561198000000041")
	party, err := store.CreateParty(ctx, leaderA, "Step down")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, memberB); err != nil {
		t.Fatal(err)
	}
	var countBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, party).Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.StepDown(ctx, party, leaderA, memberB); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != memberB {
		t.Fatalf("leader_id = %d, err=%v, want %d", gotLeader, err, memberB)
	}
	var stillMember bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, leaderA).Scan(&stillMember); err != nil || !stillMember {
		t.Fatalf("former leader membership missing: exists=%t, err=%v", stillMember, err)
	}
	var countAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, party).Scan(&countAfter); err != nil {
		t.Fatal(err)
	}
	if countAfter != countBefore {
		t.Fatalf("membership count changed: before=%d after=%d", countBefore, countAfter)
	}
}

// [CLM-3] The target of a step-down may be any member, not only the
// earliest-joined one.
func TestStepDownTargetNeedNotBeEarliestJoined(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leaderA := partyTestUser(t, store, ctx, "76561198000000050")
	memberB := partyTestUser(t, store, ctx, "76561198000000051")
	memberC := partyTestUser(t, store, ctx, "76561198000000052")
	party, err := store.CreateParty(ctx, leaderA, "Step down to junior")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1,$2,$3)`, party, memberB, now.Add(1*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id, joined_at) VALUES ($1,$2,$3)`, party, memberC, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.StepDown(ctx, party, leaderA, memberC); err != nil {
		t.Fatal(err)
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != memberC {
		t.Fatalf("leader_id = %d, err=%v, want later-joined member %d", gotLeader, err, memberC)
	}
}

// [CLM-4] Only the current leader can hand off leadership; a non-leader
// member and a non-member are both rejected.
func TestStepDownRejectsNonLeaderActor(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leaderA := partyTestUser(t, store, ctx, "76561198000000060")
	memberB := partyTestUser(t, store, ctx, "76561198000000061")
	outsider := partyTestUser(t, store, ctx, "76561198000000062")
	party, err := store.CreateParty(ctx, leaderA, "Not the leader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, memberB); err != nil {
		t.Fatal(err)
	}
	if err := store.StepDown(ctx, party, memberB, leaderA); err == nil {
		t.Fatal("non-leader member must not be able to step down the leader")
	}
	if err := store.StepDown(ctx, party, outsider, memberB); err == nil {
		t.Fatal("non-member must not be able to step down the leader")
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leaderA {
		t.Fatalf("leader_id = %d, err=%v, want unchanged %d", gotLeader, err, leaderA)
	}
}

// [CLM-5] Step-down is impossible when the leader is the party's sole member.
func TestStepDownRejectsSoleMemberParty(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leaderA := partyTestUser(t, store, ctx, "76561198000000070")
	outsider := partyTestUser(t, store, ctx, "76561198000000071")
	party, err := store.CreateParty(ctx, leaderA, "Solo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StepDown(ctx, party, leaderA, outsider); err == nil {
		t.Fatal("step down with no other member must be rejected")
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leaderA {
		t.Fatalf("leader_id = %d, err=%v, want unchanged %d", gotLeader, err, leaderA)
	}
}

// [CLM-6] The target of a step-down must already be a member of the party.
func TestStepDownRejectsNonMemberTarget(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leaderA := partyTestUser(t, store, ctx, "76561198000000080")
	memberB := partyTestUser(t, store, ctx, "76561198000000081")
	outsider := partyTestUser(t, store, ctx, "76561198000000082")
	party, err := store.CreateParty(ctx, leaderA, "Outsider target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, memberB); err != nil {
		t.Fatal(err)
	}
	var outsiderIsMember bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, outsider).Scan(&outsiderIsMember); err != nil || outsiderIsMember {
		t.Fatalf("outsider unexpectedly a member: %t, %v", outsiderIsMember, err)
	}
	if err := store.StepDown(ctx, party, leaderA, outsider); err == nil {
		t.Fatal("step down to a non-member target must be rejected")
	}
	var gotLeader int64
	if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil || gotLeader != leaderA {
		t.Fatalf("leader_id = %d, err=%v, want unchanged %d", gotLeader, err, leaderA)
	}
}
