//go:build integration

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/db"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// syncOnAddFakeSteam is the end-to-end fake with an
// optional Games failure per Steam id.
type syncOnAddFakeSteam struct {
	*endToEndFakeSteam
	gamesErr map[string]error
}

func (f *syncOnAddFakeSteam) Games(ctx context.Context, steamID string, a, b bool) ([]steamapi.Game, error) {
	if err := f.gamesErr[steamID]; err != nil {
		return nil, err
	}
	return f.endToEndFakeSteam.Games(ctx, steamID, a, b)
}

const (
	syncOnAddLeaderSteamID = "76561198000000901"
	syncOnAddFriendSteamID = "76561198000000902"
	syncOnAddFriendName    = "Freshly Added Friend"
	syncOnAddSharedAppID   = 4242
	syncOnAddSharedName    = "Shared Co-op Quest"
)

type syncOnAddEnv struct {
	store   *db.Store
	pool    *pgxpool.Pool
	ctx     context.Context
	partyID string
	router  http.Handler
	cookie  *http.Cookie
}

// newSyncOnAddEnv seeds a synced leader owning the shared game, a party they
// lead, and a fake Steam that lists the friend (no users row) with friendGames
// or friendErr as their owned-games result.
func newSyncOnAddEnv(t *testing.T, friendGames []steamapi.Game, friendErr error) *syncOnAddEnv {
	t.Helper()
	store, pool, ctx := inviteIntegrationStore(t)
	leader, err := store.UpsertUser(ctx, syncOnAddLeaderSteamID, steamapi.Player{SteamID: syncOnAddLeaderSteamID, PersonaName: "Party Leader"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreUserLibrary(ctx, leader, []steamapi.Game{{AppID: syncOnAddSharedAppID, Name: syncOnAddSharedName}}); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreGameData(ctx, []db.GameData{{AppID: syncOnAddSharedAppID, HasData: true, Multiplayer: true}}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSyncSuccess(ctx, leader); err != nil {
		t.Fatal(err)
	}
	partyID, err := store.CreateParty(ctx, leader, "Sync On Add Party")
	if err != nil {
		t.Fatal(err)
	}
	fake := &syncOnAddFakeSteam{
		endToEndFakeSteam: &endToEndFakeSteam{
			friends: []steamapi.Friend{{SteamID: syncOnAddFriendSteamID, Relationship: "friend"}},
			players: map[string]steamapi.Player{syncOnAddFriendSteamID: {SteamID: syncOnAddFriendSteamID, PersonaName: syncOnAddFriendName}},
			games:   map[string][]steamapi.Game{syncOnAddFriendSteamID: friendGames},
		},
		gamesErr: map[string]error{syncOnAddFriendSteamID: friendErr},
	}
	config := appConfig{SessionSecret: "sync-on-add-secret"}
	app := application{store: store, config: config, steam: func() view.SteamService { return fake }}
	token, err := store.CreateSession(ctx, leader)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(config.SessionSecret))
	mac.Write([]byte(token))
	return &syncOnAddEnv{
		store: store, pool: pool, ctx: ctx, partyID: partyID, router: app.routes(),
		cookie: &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))},
	}
}

func (e *syncOnAddEnv) add(t *testing.T) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/parties/"+e.partyID+"/invites/send", strings.NewReader("target="+syncOnAddFriendSteamID)).WithContext(e.ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add POST status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func (e *syncOnAddEnv) partyPage(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/parties/"+e.partyID, nil).WithContext(e.ctx)
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("party GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (e *syncOnAddEnv) friendIsMember(t *testing.T) bool {
	t.Helper()
	members, err := e.store.PartyMembers(e.ctx, e.partyID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.SteamID64 == syncOnAddFriendSteamID {
			return true
		}
	}
	return false
}

func (e *syncOnAddEnv) friendState(t *testing.T) db.GameSyncState {
	t.Helper()
	states, err := e.store.GameSyncStates(e.ctx, []string{syncOnAddFriendSteamID})
	if err != nil {
		t.Fatal(err)
	}
	state, ok := states[syncOnAddFriendSteamID]
	if !ok {
		t.Fatalf("no sync state for the added friend")
	}
	return state
}

// CLM-10: after the auto-add, the friend's library is synced, so the next
// party page shows the shared game with no sync message for them.
func TestSyncOnAddSharedGameVisible(t *testing.T) {
	env := newSyncOnAddEnv(t, []steamapi.Game{{AppID: syncOnAddSharedAppID, Name: syncOnAddSharedName}}, nil)
	env.add(t)
	if !env.friendIsMember(t) {
		t.Fatal("friend not a member after add")
	}
	if state := env.friendState(t); state.State != db.GameSyncSynced {
		t.Fatalf("friend sync state=%q want %q", state.State, db.GameSyncSynced)
	}
	body := env.partyPage(t)
	if !strings.Contains(body, syncOnAddSharedName) {
		t.Fatalf("shared game %q missing from party page; body=%s", syncOnAddSharedName, body)
	}
	for _, unwanted := range []string{"games not synced yet", "Shared games cannot be computed."} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("party page contains %q; body=%s", unwanted, body)
		}
	}
}

// CLM-10: an empty owned-games list records private_or_no_games for the friend.
func TestSyncOnAddEmptyLibraryPrivate(t *testing.T) {
	env := newSyncOnAddEnv(t, nil, nil)
	env.add(t)
	if !env.friendIsMember(t) {
		t.Fatal("friend not a member after add")
	}
	if state := env.friendState(t); state.State != db.GameSyncPrivateOrNoGames {
		t.Fatalf("friend sync state=%q want %q", state.State, db.GameSyncPrivateOrNoGames)
	}
	body := env.partyPage(t)
	if strings.Contains(body, syncOnAddSharedName) {
		t.Fatalf("private friend contributed games; body=%s", body)
	}
}

// CLM-11: a failed library fetch keeps the member, records the failure and the
// party page renders the cannot-compute message naming them.
func TestSyncOnAddFailureKeepsMember(t *testing.T) {
	env := newSyncOnAddEnv(t, nil, errors.New("steam owned games unavailable"))
	env.add(t)
	if !env.friendIsMember(t) {
		t.Fatal("friend not a member after a failed sync")
	}
	if state := env.friendState(t); state.LibrarySyncError == "" || state.LibrarySyncErrorAt == nil {
		t.Fatalf("no sync failure recorded: %+v", state)
	}
	body := env.partyPage(t)
	for _, want := range []string{"Shared games cannot be computed.", syncOnAddFriendName} {
		if !strings.Contains(body, want) {
			t.Fatalf("party page missing %q; body=%s", want, body)
		}
	}
	if strings.Contains(body, "games not synced yet for user "+syncOnAddFriendName) {
		t.Fatalf("party page shows not-synced instead of sync-failed; body=%s", body)
	}
}
