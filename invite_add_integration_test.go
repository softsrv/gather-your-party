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
	"sync"
	"testing"

	"gather-your-party/internal/view"

	"github.com/softsrv/steamapi/steamapi"
)

type endToEndFakeSteam struct {
	mu      sync.Mutex
	friends []steamapi.Friend
	players map[string]steamapi.Player
	games   map[string][]steamapi.Game
}

func (f *endToEndFakeSteam) Friends(_ context.Context, _ string) ([]steamapi.Friend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]steamapi.Friend(nil), f.friends...), nil
}

func (f *endToEndFakeSteam) Players(_ context.Context, ids []string) ([]steamapi.Player, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []steamapi.Player
	for _, id := range ids {
		if p, ok := f.players[id]; ok {
			result = append(result, p)
		}
	}
	return result, nil
}

func (f *endToEndFakeSteam) Games(_ context.Context, steamID string, _, _ bool) ([]steamapi.Game, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if games, ok := f.games[steamID]; ok {
		return games, nil
	}
	return nil, nil
}

func (f *endToEndFakeSteam) GamesDetails(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
	details := make([]steamapi.GameDetail, len(ids))
	for i, id := range ids {
		details[i] = steamapi.GameDetail{AppID: id, Multiplayer: true}
	}
	return details, nil
}

func (f *endToEndFakeSteam) GetNumberOfCurrentPlayers(_ context.Context, _ string) (int, error) {
	return 0, errors.New("unexpected player count lookup")
}

// CLM-1: A leader whose Steam friend list contains a friend with no users row
// GETs the party detail page and sees that friend in the Invite friends section.
// POSTing the add for that friend makes the next party detail GET list the friend
// among the members, with no pending invite for them.
func TestAddNeverSignedInFriendEndToEnd(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)

	const leaderSteamID = "76561198000000801"
	const friendSteamID = "76561198000000802"
	const friendPersonaName = "Unregistered Friend"

	leaderUserID, err := store.UpsertUser(ctx, leaderSteamID, steamapi.Player{
		SteamID:     leaderSteamID,
		PersonaName: "Party Leader",
	})
	if err != nil {
		t.Fatal(err)
	}

	partyID, err := store.CreateParty(ctx, leaderUserID, "End-To-End Invite Party")
	if err != nil {
		t.Fatal(err)
	}

	// Verify that the friend has NO users row in the database yet.
	var friendRowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE steam_id_64 = $1`, friendSteamID).Scan(&friendRowCount); err != nil {
		t.Fatal(err)
	}
	if friendRowCount != 0 {
		t.Fatalf("friend already exists in users: count=%d", friendRowCount)
	}

	fakeSteam := &endToEndFakeSteam{
		friends: []steamapi.Friend{
			{SteamID: friendSteamID, Relationship: "friend"},
		},
		players: map[string]steamapi.Player{
			friendSteamID: {
				SteamID:      friendSteamID,
				PersonaName:  friendPersonaName,
				AvatarMedium: "https://example.com/avatar.jpg",
			},
		},
	}

	config := appConfig{SessionSecret: "e2e-invite-secret"}
	app := application{
		store:  store,
		config: config,
		steam: func() view.SteamService {
			return fakeSteam
		},
	}
	router := app.routes()

	token, err := store.CreateSession(ctx, leaderUserID)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(config.SessionSecret))
	mac.Write([]byte(token))
	sessionCookie := &http.Cookie{
		Name:  "session",
		Value: token + "." + hex.EncodeToString(mac.Sum(nil)),
	}

	doGetParty := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/parties/"+partyID, nil).WithContext(ctx)
		req.AddCookie(sessionCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// 1. GET party detail: should show the friend's persona name in the picker.
	rec1 := doGetParty()
	if rec1.Code != http.StatusOK {
		t.Fatalf("first GET /parties/%s status=%d body=%s", partyID, rec1.Code, rec1.Body.String())
	}
	body1 := rec1.Body.String()
	if !strings.Contains(body1, friendPersonaName) {
		t.Fatalf("picker did not render friend persona name %q; body=%s", friendPersonaName, body1)
	}

	// 2. POST add/invite for that friend.
	// Target can be friendSteamID (or target ID once resolved by U3/U4).
	postReq := httptest.NewRequest(
		http.MethodPost,
		"/parties/"+partyID+"/invites/send",
		strings.NewReader("target="+friendSteamID),
	).WithContext(ctx)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("HX-Request", "true")
	postReq.AddCookie(sessionCookie)
	postRec := httptest.NewRecorder()
	router.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusOK && postRec.Code != http.StatusSeeOther {
		t.Fatalf("POST /parties/%s/invites/send returned status=%d body=%s", partyID, postRec.Code, postRec.Body.String())
	}

	// 3. Next party detail GET lists the friend among the members.
	rec2 := doGetParty()
	if rec2.Code != http.StatusOK {
		t.Fatalf("second GET /parties/%s status=%d body=%s", partyID, rec2.Code, rec2.Body.String())
	}
	body2 := rec2.Body.String()

	// Member list (PartyRoster) should contain the friend's name.
	if !strings.Contains(body2, friendPersonaName) {
		t.Fatalf("party detail roster missing member %q after add; body=%s", friendPersonaName, body2)
	}

	// And no pending invite for them.
	var pendingCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM invites i
		JOIN users u ON u.id = i.user_id
		WHERE i.party_id = $1 AND u.steam_id_64 = $2 AND i.status = 'pending'`,
		partyID, friendSteamID).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if pendingCount != 0 {
		t.Fatalf("invites table has pending invite for friend: count=%d", pendingCount)
	}
}
