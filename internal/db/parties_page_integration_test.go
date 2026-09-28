//go:build integration

package db

import (
	"reflect"
	"sort"
	"testing"
)

// CLM-8: include ordinary memberships, not just leadership, and exclude other parties.
func TestUserParties(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	user := partyTestUser(t, store, ctx, "76561198000000701")
	leader := partyTestUser(t, store, ctx, "76561198000000702")
	empty := partyTestUser(t, store, ctx, "76561198000000703")
	first, err := store.CreateParty(ctx, user, "First party")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateParty(ctx, leader, "Second party")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateParty(ctx, leader, "Unrelated party"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, second, user); err != nil {
		t.Fatal(err)
	}
	// Reverse creation order explicitly, independent of clock precision.
	if _, err := pool.Exec(ctx, `UPDATE parties SET created_at = CASE WHEN id = $1 THEN '2020-01-02'::timestamptz ELSE '2020-01-01'::timestamptz END`, first); err != nil {
		t.Fatal(err)
	}
	want := []UserParty{{PartyID: second, Name: "Second party"}, {PartyID: first, Name: "First party"}}
	got, err := store.UserParties(ctx, user)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parties=%+v want=%+v err=%v", got, want, err)
	}
	// Equal creation times require the party ID tiebreaker.
	if _, err := pool.Exec(ctx, `UPDATE parties SET created_at = '2020-01-01'::timestamptz`); err != nil {
		t.Fatal(err)
	}
	sort.Slice(want, func(i, j int) bool { return want[i].PartyID < want[j].PartyID })
	got, err = store.UserParties(ctx, user)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("tied parties=%+v want=%+v err=%v", got, want, err)
	}
	got, err = store.UserParties(ctx, empty)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty parties=%+v err=%v", got, err)
	}
}

// CLM-3: seed real invite transitions plus another user's pending invite.
func TestUserPendingInvites(t *testing.T) {
	store, _, ctx := partyTestStore(t)
	leader := partyTestUser(t, store, ctx, "76561198000000711")
	user := partyTestUser(t, store, ctx, "76561198000000712")
	other := partyTestUser(t, store, ctx, "76561198000000713")
	empty := partyTestUser(t, store, ctx, "76561198000000714")
	var pending string
	for _, name := range []string{"Pending party", "Accepted party", "Rejected party", "Other user's party"} {
		party, err := store.CreateParty(ctx, leader, name)
		if err != nil {
			t.Fatal(err)
		}
		target := user
		if name == "Other user's party" {
			target = other
		}
		if err := store.SendInvite(ctx, party, leader, target); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "Pending party":
			pending = party
		case "Accepted party":
			if err := store.AcceptInvite(ctx, party, user); err != nil {
				t.Fatal(err)
			}
		case "Rejected party":
			if err := store.RejectInvite(ctx, party, user); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := store.UserPendingInvites(ctx, user)
	want := []UserPendingInvite{{PartyID: pending, Name: "Pending party"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("invites=%+v want=%+v err=%v", got, want, err)
	}
	got, err = store.UserPendingInvites(ctx, empty)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty invites=%+v err=%v", got, err)
	}
}
