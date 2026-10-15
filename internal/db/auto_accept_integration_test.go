//go:build integration

package db

import (
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

func inviteStatusCounts(t *testing.T, store *Store, party string, user int64) (pending, accepted, total int) {
	t.Helper()
	pool := store.pool
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'accepted'),
		       count(*)
		FROM invites WHERE party_id = $1 AND user_id = $2`, party, user).Scan(&pending, &accepted, &total); err != nil {
		t.Fatal(err)
	}
	return pending, accepted, total
}

func memberIDs(t *testing.T, store *Store, party string) []int64 {
	t.Helper()
	members, err := store.PartyMembers(t.Context(), party)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(members))
	for i, m := range members {
		ids[i] = m.UserID
	}
	return ids
}

// CLM-5: adding a friend who has never signed in joins them at once, after the
// previously joined members, with one accepted invite and no pending one.
func TestSendInviteAutoAcceptsNeverSignedIn(t *testing.T) {
	store, _, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000019001")
	member := partyTestUser(t, store, ctx, "76561198000019002")
	friend, err := store.UpsertFriend(ctx, "76561198000019003", steamapi.Player{PersonaName: "Never signed in"})
	if err != nil {
		t.Fatal(err)
	}
	party, err := store.CreateParty(ctx, leader, "Auto accept")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, leader, member); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptInvite(ctx, party, member); err != nil {
		t.Fatal(err)
	}

	if err := store.SendInvite(ctx, party, leader, friend); err != nil {
		t.Fatal(err)
	}

	got := memberIDs(t, store, party)
	if len(got) != 3 || got[0] != leader || got[1] != member || got[2] != friend {
		t.Fatalf("members=%v want [%d %d %d]", got, leader, member, friend)
	}
	pending, accepted, total := inviteStatusCounts(t, store, party, friend)
	if pending != 0 || accepted != 1 || total != 1 {
		t.Fatalf("friend invites pending=%d accepted=%d total=%d, want 0/1/1", pending, accepted, total)
	}
	// The re-checks still hold under the lock: a second add is refused as a member.
	if err := store.SendInvite(ctx, party, leader, friend); err != ErrAlreadyMember {
		t.Fatalf("second add err=%v want ErrAlreadyMember", err)
	}
	if err := store.SendInvite(ctx, party, member, partyTestUserFriend(t, store)); err != ErrNotLeader {
		t.Fatalf("non-leader add err=%v want ErrNotLeader", err)
	}
}

func partyTestUserFriend(t *testing.T, store *Store) int64 {
	t.Helper()
	id, err := store.UpsertFriend(t.Context(), "76561198000019009", steamapi.Player{PersonaName: "Other friend"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// CLM-6: adding a signed-in friend leaves a pending invite and no membership;
// only their own AcceptInvite makes them a member.
func TestSendInviteSignedInStaysPending(t *testing.T) {
	store, _, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000019101")
	signedIn := partyTestUser(t, store, ctx, "76561198000019102")
	party, err := store.CreateParty(ctx, leader, "Pending")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SendInvite(ctx, party, leader, signedIn); err != nil {
		t.Fatal(err)
	}
	if got := memberIDs(t, store, party); len(got) != 1 || got[0] != leader {
		t.Fatalf("members after invite=%v want only leader %d", got, leader)
	}
	pending, accepted, total := inviteStatusCounts(t, store, party, signedIn)
	if pending != 1 || accepted != 0 || total != 1 {
		t.Fatalf("invites pending=%d accepted=%d total=%d, want 1/0/1", pending, accepted, total)
	}

	if err := store.AcceptInvite(ctx, party, signedIn); err != nil {
		t.Fatal(err)
	}
	if got := memberIDs(t, store, party); len(got) != 2 || got[1] != signedIn {
		t.Fatalf("members after accept=%v want [%d %d]", got, leader, signedIn)
	}
	pending, accepted, _ = inviteStatusCounts(t, store, party, signedIn)
	if pending != 0 || accepted != 1 {
		t.Fatalf("after accept pending=%d accepted=%d", pending, accepted)
	}
}
