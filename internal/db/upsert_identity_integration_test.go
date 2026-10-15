//go:build integration

package db

import (
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

// CLM-7: UpsertUser called for a steam_id_64 that already has a users row
// (created with ever_logged_in=false) returns that row's existing id, updates it
// to ever_logged_in=true, and creates no second row.
func TestUpsertUserKeepsFriendRow(t *testing.T) {
	store, pool, ctx := partyTestStore(t)

	const steamID = "76561198000017999"
	friendPlayer := steamapi.Player{
		SteamID:      steamID,
		PersonaName:  "Friend Persona",
		AvatarSmall:  "https://example.com/small.jpg",
		AvatarMedium: "https://example.com/medium.jpg",
		AvatarFull:   "https://example.com/full.jpg",
	}

	id1, err := store.UpsertFriend(ctx, steamID, friendPlayer)
	if err != nil {
		t.Fatalf("UpsertFriend failed: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("UpsertFriend returned invalid id: %d", id1)
	}

	// Verify initial state: ever_logged_in is false.
	var initialEverLoggedIn bool
	if err := pool.QueryRow(ctx, `SELECT ever_logged_in FROM users WHERE id = $1`, id1).Scan(&initialEverLoggedIn); err != nil {
		t.Fatalf("query initial ever_logged_in failed: %v", err)
	}
	if initialEverLoggedIn {
		t.Fatalf("expected initial ever_logged_in to be false, got true")
	}

	userPlayer := steamapi.Player{
		SteamID:      steamID,
		PersonaName:  "Updated Persona",
		AvatarSmall:  "https://example.com/small2.jpg",
		AvatarMedium: "https://example.com/medium2.jpg",
		AvatarFull:   "https://example.com/full2.jpg",
	}

	id2, err := store.UpsertUser(ctx, steamID, userPlayer)
	if err != nil {
		t.Fatalf("UpsertUser failed: %v", err)
	}

	// Assert returned id equals id1.
	if id1 != id2 {
		t.Fatalf("UpsertUser returned id %d, want existing id %d", id2, id1)
	}

	// Assert count of users for this steam_id_64 is exactly 1.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE steam_id_64 = $1`, steamID).Scan(&count); err != nil {
		t.Fatalf("query count failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected count 1 for steam_id_64 %s, got %d", steamID, count)
	}

	// Assert ever_logged_in is now true.
	var finalEverLoggedIn bool
	var personaName string
	if err := pool.QueryRow(ctx, `SELECT ever_logged_in, persona_name FROM users WHERE id = $1`, id1).Scan(&finalEverLoggedIn, &personaName); err != nil {
		t.Fatalf("query final state failed: %v", err)
	}
	if !finalEverLoggedIn {
		t.Fatalf("expected final ever_logged_in to be true, got false")
	}
	if personaName != userPlayer.PersonaName {
		t.Fatalf("expected persona_name to update to %q, got %q", userPlayer.PersonaName, personaName)
	}
}
