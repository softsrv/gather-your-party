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
	if err := store.LeaveParty(ctx, "invalid-uuid", leader); err == nil {
		t.Fatal("invalid UUID must return an error")
	}
}

// [CLM-1] A party that still has at least one member after someone leaves is
// NOT deleted: the silent last-member delete fires only when no memberships
// remain.
func TestLeavePartyStillPopulatedPartyIsNotDeleted(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000090")
	member := partyTestUser(t, store, ctx, "76561198000000091")
	party, err := store.CreateParty(ctx, leader, "Still populated")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, member); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM parties WHERE id = $1)`, party).Scan(&exists); err != nil || !exists {
		t.Fatalf("party must still exist: exists=%t, err=%v", exists, err)
	}
	var leaderStillMember bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2)`, party, leader).Scan(&leaderStillMember); err != nil || !leaderStillMember {
		t.Fatalf("remaining member's membership must be intact: exists=%t, err=%v", leaderStillMember, err)
	}
}

// [CLM-2] When the last remaining member leaves a party, the leave path
// removes that now-empty party's row so the party no longer exists.
func TestLeavePartyLastMemberDeletesParty(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000092")
	party, err := store.CreateParty(ctx, leader, "Sole member")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leader); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM parties WHERE id = $1)`, party).Scan(&exists); err != nil || exists {
		t.Fatalf("emptied party must be deleted: exists=%t, err=%v", exists, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parties WHERE id = $1`, party).Scan(&count); err != nil || count != 0 {
		t.Fatalf("party count = %d, err=%v, want 0", count, err)
	}
}

// [CLM-3] Deleting an emptied party through the leave path cascades: no
// orphaned memberships, invites, or rejection tallies remain for it.
func TestLeavePartyCascadeLeavesNoOrphans(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000093")
	invitee := partyTestUser(t, store, ctx, "76561198000000094")
	party, err := store.CreateParty(ctx, leader, "Cascade test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO invites (party_id, user_id, inviter_id) VALUES ($1, $2, $3)`, party, invitee, leader); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rejection_tallies (party_id, user_id, count) VALUES ($1, $2, $3)`, party, invitee, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.LeaveParty(ctx, party, leader); err != nil {
		t.Fatal(err)
	}
	var membershipCount, inviteCount, tallyCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, party).Scan(&membershipCount); err != nil || membershipCount != 0 {
		t.Fatalf("orphaned memberships = %d, err=%v", membershipCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invites WHERE party_id = $1`, party).Scan(&inviteCount); err != nil || inviteCount != 0 {
		t.Fatalf("orphaned invites = %d, err=%v", inviteCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rejection_tallies WHERE party_id = $1`, party).Scan(&tallyCount); err != nil || tallyCount != 0 {
		t.Fatalf("orphaned rejection_tallies = %d, err=%v", tallyCount, err)
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

// Invite CLM-4/13: membership is not authority; only the current leader may send.
func TestSendInviteLeaderOnly(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000101")
	member := partyTestUser(t, store, ctx, "76561198000000102")
	target := partyTestUser(t, store, ctx, "76561198000000103")
	party, err := store.CreateParty(ctx, leader, "Invites")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []int64{member, -1} {
		if err := store.SendInvite(ctx, party, actor, target); err == nil {
			t.Fatalf("actor %d sent invite", actor)
		}
		assertInviteCount(t, pool, ctx, party, target, 0)
	}
	if _, found, err := store.ResolveUserID(ctx, "76561198999999999"); err != nil || found {
		t.Fatalf("unknown identity: found=%t err=%v", found, err)
	}
	if err := store.SendInvite(ctx, party, leader, target); err != nil {
		t.Fatal(err)
	}
	assertInviteCount(t, pool, ctx, party, target, 1)
	// A handoff immediately revokes the former leader's send authority.
	if err := store.StepDown(ctx, party, leader, member); err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, leader, target); err == nil {
		t.Fatal("former leader may not send")
	}
}

// Invite CLM-7: simultaneous sends both return safely, leaving one pending row.
func TestSendInviteConcurrentDuplicate(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000101")
	target := partyTestUser(t, store, ctx, "76561198000000102")
	party, err := store.CreateParty(ctx, leader, "Concurrent invites")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- store.SendInvite(ctx, party, leader, target) }()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	assertInviteCount(t, pool, ctx, party, target, 1)
	var inviter int64
	var status string
	if err := pool.QueryRow(ctx, `SELECT inviter_id, status FROM invites WHERE party_id = $1 AND user_id = $2`, party, target).Scan(&inviter, &status); err != nil {
		t.Fatal(err)
	}
	if inviter != leader || status != "pending" {
		t.Fatalf("inviter=%d status=%s", inviter, status)
	}
}

// Invite CLM-8/15: the durable tally is consulted before any insert, at and above 3.
func TestSendInviteRejectionBoundary(t *testing.T) {
	for _, strikes := range []int{0, 2, 3, 4} {
		t.Run(fmt.Sprint(strikes), func(t *testing.T) {
			store, pool, ctx := partyTestStore(t)
			leader := partyTestUser(t, store, ctx, "76561198000000101")
			target := partyTestUser(t, store, ctx, "76561198000000102")
			other := partyTestUser(t, store, ctx, "76561198000000103")
			party, err := store.CreateParty(ctx, leader, "Strike boundary")
			if err != nil {
				t.Fatal(err)
			}
			otherParty, err := store.CreateParty(ctx, leader, "Unrelated tally")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO rejection_tallies (party_id, user_id, count) VALUES ($1,$2,$3), ($1,$4,9), ($5,$2,9)`, party, target, strikes, other, otherParty); err != nil {
				t.Fatal(err)
			}
			err = store.SendInvite(ctx, party, leader, target)
			want := 1
			if strikes >= 3 {
				want = 0
			}
			if (err != nil) != (strikes >= 3) {
				t.Fatalf("strikes=%d err=%v", strikes, err)
			}
			assertInviteCount(t, pool, ctx, party, target, want)
			var got int
			if err := pool.QueryRow(ctx, `SELECT count FROM rejection_tallies WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&got); err != nil || got != strikes {
				t.Fatalf("tally=%d err=%v", got, err)
			}
		})
	}
}

// Invite CLM-9: accepting consumes the invitation and creates a timestamped membership.
func TestAcceptInviteMembership(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000101")
	target := partyTestUser(t, store, ctx, "76561198000000102")
	party, err := store.CreateParty(ctx, leader, "Accept")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, leader, target); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptInvite(ctx, party, target); err != nil {
		t.Fatal(err)
	}
	var joined time.Time
	if err := pool.QueryRow(ctx, `SELECT joined_at FROM memberships WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&joined); err != nil || joined.IsZero() {
		t.Fatalf("joined_at=%v err=%v", joined, err)
	}
	assertInviteStatus(t, pool, ctx, party, target, "accepted")
	if err := store.AcceptInvite(ctx, party, target); err == nil {
		t.Fatal("consumed invite accepted twice")
	}
}

// Invite CLM-10: rejection creates then increments the tally, without membership.
func TestRejectInviteTallyCycles(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000101")
	target := partyTestUser(t, store, ctx, "76561198000000102")
	party, err := store.CreateParty(ctx, leader, "Reject")
	if err != nil {
		t.Fatal(err)
	}
	for cycle := 1; cycle <= 2; cycle++ {
		if err := store.SendInvite(ctx, party, leader, target); err != nil {
			t.Fatal(err)
		}
		if err := store.RejectInvite(ctx, party, target); err != nil {
			t.Fatal(err)
		}
		if err := store.RejectInvite(ctx, party, target); err == nil {
			t.Fatal("consumed invite rejected twice")
		}
		var count, members, pending, rejected int
		if err := pool.QueryRow(ctx, `SELECT count FROM rejection_tallies WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&count); err != nil || count != cycle {
			t.Fatalf("tally=%d want=%d err=%v", count, cycle, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&members); err != nil || members != 0 {
			t.Fatalf("members=%d err=%v", members, err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='pending'), count(*) FILTER (WHERE status='rejected') FROM invites WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&pending, &rejected); err != nil || pending != 0 || rejected != cycle {
			t.Fatalf("pending=%d rejected=%d err=%v", pending, rejected, err)
		}
	}
}

// Invite CLM-11: neither reply can consume another user's invitation or alter tallies.
func TestInviteRepliesRejectWrongUser(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000101")
	target := partyTestUser(t, store, ctx, "76561198000000102")
	stranger := partyTestUser(t, store, ctx, "76561198000000103")
	party, err := store.CreateParty(ctx, leader, "Wrong target")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, leader, target); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO rejection_tallies (party_id,user_id,count) VALUES ($1,$2,1)`, party, target); err != nil {
		t.Fatal(err)
	}
	for _, reply := range []struct {
		name string
		op   func(context.Context, string, int64) error
	}{{"accept", store.AcceptInvite}, {"reject", store.RejectInvite}} {
		t.Run(reply.name, func(t *testing.T) {
			if err := reply.op(ctx, party, stranger); err == nil {
				t.Fatal("wrong target permitted")
			}
			assertInviteStatus(t, pool, ctx, party, target, "pending")
			var members, tallies, total int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id=$1 AND user_id IN ($2,$3)`, party, target, stranger).Scan(&members); err != nil || members != 0 {
				t.Fatalf("members=%d err=%v", members, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*), sum(count) FROM rejection_tallies WHERE party_id=$1`, party).Scan(&tallies, &total); err != nil || tallies != 1 || total != 1 {
				t.Fatalf("tallies=%d total=%d err=%v", tallies, total, err)
			}
		})
	}
}

// Invite CLM-5/6: independently exercise self, member, and pending exclusions.
func TestInviteCandidatesIntersectionAndExclusions(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	ids := make([]int64, 6)
	steamIDs := make([]string, 6)
	for i := range ids {
		steamIDs[i] = fmt.Sprintf("765611980000002%02d", i)
		ids[i] = partyTestUser(t, store, ctx, steamIDs[i])
	}
	leader, member, pending, eligible, nonfriend, rejected := ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]
	party, err := store.CreateParty(ctx, leader, "Picker")
	if err != nil {
		t.Fatal(err)
	}
	// Remove the leader membership so the self exclusion cannot hide behind the member exclusion.
	if _, err := pool.Exec(ctx, `DELETE FROM memberships WHERE party_id=$1 AND user_id=$2`, party, leader); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2)`, party, member); err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, leader, pending); err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, leader, rejected); err != nil {
		t.Fatal(err)
	}
	if err := store.RejectInvite(ctx, party, rejected); err != nil {
		t.Fatal(err)
	}
	// Eligibility is party-scoped: another party's member/pending invite must not exclude.
	other, err := store.CreateParty(ctx, nonfriend, "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2)`, other, eligible); err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, other, nonfriend, eligible); err != nil {
		t.Fatal(err)
	}
	friends := []string{steamIDs[0], steamIDs[1], steamIDs[2], steamIDs[3], steamIDs[5], "76561198999999999", steamIDs[3]}
	got, err := store.InviteCandidates(ctx, party, leader, friends)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (InviteCandidate{UserID: eligible, Name: "Party member"}) || got[1] != (InviteCandidate{UserID: rejected, Name: "Party member"}) {
		t.Fatalf("candidates=%+v", got)
	}
	if got, err := store.InviteCandidates(ctx, party, leader, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty friends: %+v %v", got, err)
	}
	if got, err := store.InviteCandidates(ctx, party, member, friends); err != nil || len(got) != 0 {
		t.Fatalf("nonleader: %+v %v", got, err)
	}
}

func assertInviteCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, party string, target int64, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invites WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&count); err != nil || count != want {
		t.Fatalf("invites=%d want=%d err=%v", count, want, err)
	}
}

func assertInviteStatus(t *testing.T, pool *pgxpool.Pool, ctx context.Context, party string, target int64, want string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM invites WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&status); err != nil || status != want {
		t.Fatalf("status=%s want=%s err=%v", status, want, err)
	}
}

func TestInviteRepliesRollBackOnSecondWriteFailure(t *testing.T) {
	for _, action := range []string{"accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			store, pool, ctx := partyTestStore(t)
			leader := partyTestUser(t, store, ctx, "76561198000000101")
			target := partyTestUser(t, store, ctx, "76561198000000102")
			party, err := store.CreateParty(ctx, leader, "Atomic reply")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SendInvite(ctx, party, leader, target); err != nil {
				t.Fatal(err)
			}
			if action == "accept" {
				if _, err := pool.Exec(ctx, `ALTER TABLE invites ADD CONSTRAINT reject_accept_test CHECK (status <> 'accepted')`); err != nil {
					t.Fatal(err)
				}
				err = store.AcceptInvite(ctx, party, target)
			} else {
				if _, err := pool.Exec(ctx, `ALTER TABLE rejection_tallies ADD CONSTRAINT reject_tally_test CHECK (count < 1)`); err != nil {
					t.Fatal(err)
				}
				err = store.RejectInvite(ctx, party, target)
			}
			if err == nil {
				t.Fatal("injected second write failure must be returned")
			}
			assertInviteStatus(t, pool, ctx, party, target, "pending")
			var members, tallies int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&members); err != nil || members != 0 {
				t.Fatalf("partial membership=%d err=%v", members, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM rejection_tallies WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&tallies); err != nil || tallies != 0 {
				t.Fatalf("partial tally=%d err=%v", tallies, err)
			}
		})
	}
}
