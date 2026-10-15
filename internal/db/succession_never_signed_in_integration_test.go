//go:build integration

package db

import (
	"testing"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

// CLM-9: For a member F added through the never-signed-in auto-add path:
// when F is the earliest-joined remaining member, the leader's LeaveParty makes F
// the party leader; and the leader's StepDown targeting F succeeds and makes F the leader.
// Neither path adds an ever_logged_in check.
func TestNeverSignedInMemberSuccession(t *testing.T) {
	t.Run("LeavePartySuccessionToNeverSignedIn", func(t *testing.T) {
		store, pool, ctx := partyTestStore(t)
		leader := partyTestUser(t, store, ctx, "76561198000021001")
		friend, err := store.UpsertFriend(ctx, "76561198000021002", steamapi.Player{PersonaName: "Never signed in"})
		if err != nil {
			t.Fatal(err)
		}
		signedIn := partyTestUser(t, store, ctx, "76561198000021003")

		party, err := store.CreateParty(ctx, leader, "Succession test")
		if err != nil {
			t.Fatal(err)
		}

		if err := store.SendInvite(ctx, party, leader, friend); err != nil {
			t.Fatal(err)
		}

		// Ensure signed-in member joins strictly later
		time.Sleep(10 * time.Millisecond)

		if err := store.SendInvite(ctx, party, leader, signedIn); err != nil {
			t.Fatal(err)
		}
		if err := store.AcceptInvite(ctx, party, signedIn); err != nil {
			t.Fatal(err)
		}

		if err := store.LeaveParty(ctx, party, leader); err != nil {
			t.Fatal(err)
		}

		var gotLeader int64
		if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil {
			t.Fatal(err)
		}
		if gotLeader != friend {
			t.Fatalf("leader_id = %d, want earliest-joined friend %d", gotLeader, friend)
		}
	})

	t.Run("StepDownToNeverSignedIn", func(t *testing.T) {
		store, pool, ctx := partyTestStore(t)
		leader := partyTestUser(t, store, ctx, "76561198000021011")
		friend, err := store.UpsertFriend(ctx, "76561198000021012", steamapi.Player{PersonaName: "Never signed in StepDown"})
		if err != nil {
			t.Fatal(err)
		}

		party, err := store.CreateParty(ctx, leader, "StepDown test")
		if err != nil {
			t.Fatal(err)
		}

		if err := store.SendInvite(ctx, party, leader, friend); err != nil {
			t.Fatal(err)
		}

		if err := store.StepDown(ctx, party, leader, friend); err != nil {
			t.Fatalf("StepDown failed: %v", err)
		}

		var gotLeader int64
		if err := pool.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1`, party).Scan(&gotLeader); err != nil {
			t.Fatal(err)
		}
		if gotLeader != friend {
			t.Fatalf("leader_id = %d, want friend %d", gotLeader, friend)
		}
	})
}
