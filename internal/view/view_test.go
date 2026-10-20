package view

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
)

func TestStoredSharedGamesMessagesUseNamesWithSteamIDFallback(t *testing.T) {
	store := newMemStore()
	store.addUser("caller", db.GameSyncSynced, stored(1, "A", true))
	store.addUser("friend-1", db.GameSyncPrivateOrNoGames)
	store.addUser("friend-2", db.GameSyncNeverSynced)
	names := map[string]string{"friend-1": "Alyx"}
	games, got, err := storedSharedGames(context.Background(), context.Background(), steamStub{}, store, "caller", []string{"friend-1", "friend-2", "missing"}, names)
	want := []string{
		"no games found for user Alyx. Their list may be private",
		"games not synced yet for user friend-2",
		"games not synced yet for user missing",
	}
	if err != nil || games != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("games=%v messages=%#v err=%v", games, got, err)
	}
}

func TestStoredSharedGamesOmitsFailedFetchWithoutError(t *testing.T) {
	store := newMemStore()
	library := []db.StoredGame{stored(1, "Stored", true), {AppID: 2, Name: "Unfetched"}}
	store.addUser("caller", db.GameSyncSynced, library...)
	store.addUser("friend", db.GameSyncSynced, library...)
	steam := steamStub{} // GamesDetails errors
	games, messages, err := storedSharedGames(context.Background(), context.Background(), steam, store, "caller", []string{"friend"}, nil)
	if err != nil || len(messages) != 0 || len(games) != 1 || games[0].AppID != 1 {
		t.Fatalf("games=%v messages=%v err=%v", games, messages, err)
	}
}

// CLM-3: a signed-out request to the friends-list handler gets the full sign-in
// page, footer included.
func TestSignedOutFragmentRequestGetsSigninPageWithFooter(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/friends", nil)
	ctx := &middleware.CustomContext{Context: context.Background(), StartTime: time.Now()}

	FriendsList(ctx, rec, req)

	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "/auth/steam", "2026 softsrv", "Not affiliated with Valve"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("signed-out response lacks %q: %s", want, body)
		}
	}
}
