//go:build integration

package db

import (
	"fmt"
	"testing"
)

// [CLM-2] PartyMembers projects users.steam_id_64 from the server-side
// memberships-through-users join: every returned member must carry the
// SteamID64 seeded on its users row, not a value derived from request input.
func TestPartyMembersProjectsSteamID64FromUsersJoin(t *testing.T) {
	store, _, ctx := partyTestStore(t)
	const memberCount = 3
	ids := make([]int64, memberCount)
	steamIDs := make([]string, memberCount)
	for i := range ids {
		steamIDs[i] = fmt.Sprintf("765611980000009%02d", i)
		ids[i] = partyTestUser(t, store, ctx, steamIDs[i])
	}
	party, err := store.CreateParty(ctx, ids[0], "Steam roster")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, ids[0], ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptInvite(ctx, party, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, ids[0], ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := store.AcceptInvite(ctx, party, ids[2]); err != nil {
		t.Fatal(err)
	}

	got, err := store.PartyMembers(ctx, party)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != memberCount {
		t.Fatalf("members=%+v, want %d members", got, memberCount)
	}
	wantByUserID := make(map[int64]string, memberCount)
	for i, id := range ids {
		wantByUserID[id] = steamIDs[i]
	}
	for _, member := range got {
		want, ok := wantByUserID[member.UserID]
		if !ok {
			t.Fatalf("unexpected member user id %d in %+v", member.UserID, got)
		}
		if member.SteamID64 != want {
			t.Errorf("member %d SteamID64=%q, want seeded users.steam_id_64=%q", member.UserID, member.SteamID64, want)
		}
	}
}
