//go:build integration

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	// 1. GET party detail: should show the friend's persona name in the picker, not in members.
	rec1 := doGetParty()
	if rec1.Code != http.StatusOK {
		t.Fatalf("first GET /parties/%s status=%d body=%s", partyID, rec1.Code, rec1.Body.String())
	}
	body1 := rec1.Body.String()
	inviteSection1 := extractSection(t, body1, "Invite friends")
	if !strings.Contains(inviteSection1, friendPersonaName) {
		t.Fatalf("Invite friends section did not render friend %q before add; section=%s", friendPersonaName, inviteSection1)
	}
	membersSection1 := extractSection(t, body1, "Party members")
	if strings.Contains(membersSection1, friendPersonaName) {
		t.Fatalf("Party members section unexpectedly contains friend %q before add; section=%s", friendPersonaName, membersSection1)
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

	// 3. Next party detail GET lists the friend among the members, not in the invite picker.
	rec2 := doGetParty()
	if rec2.Code != http.StatusOK {
		t.Fatalf("second GET /parties/%s status=%d body=%s", partyID, rec2.Code, rec2.Body.String())
	}
	body2 := rec2.Body.String()

	membersSection2 := extractSection(t, body2, "Party members")
	if !strings.Contains(membersSection2, friendPersonaName) {
		t.Fatalf("Party members section missing member %q after add; section=%s", friendPersonaName, membersSection2)
	}
	inviteSection2 := extractSection(t, body2, "Invite friends")
	if strings.Contains(inviteSection2, friendPersonaName) {
		t.Fatalf("Invite friends section unexpectedly contains member %q after add; section=%s", friendPersonaName, inviteSection2)
	}

	// DB check: exactly 1 membership row for (party, friend).
	var membershipCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM memberships m
		JOIN users u ON u.id = m.user_id
		WHERE m.party_id = $1 AND u.steam_id_64 = $2`,
		partyID, friendSteamID).Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if membershipCount != 1 {
		t.Fatalf("memberships count for friend (party=%s, steam_id=%s) is %d, want 1", partyID, friendSteamID, membershipCount)
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

func extractSection(t *testing.T, html, ariaLabel string) string {
	t.Helper()
	marker := `aria-label="` + ariaLabel + `"`
	idx := strings.Index(html, marker)
	if idx == -1 {
		t.Fatalf("section with %s not found in HTML", marker)
	}
	start := strings.LastIndex(html[:idx], "<section")
	if start == -1 {
		t.Fatalf("opening <section not found preceding %s", marker)
	}
	endRel := strings.Index(html[idx:], "</section>")
	if endRel == -1 {
		t.Fatalf("closing </section> not found following %s", marker)
	}
	return html[start : idx+endRel+len("</section>")]
}

// CLM-11: After a never-signed-in friend F is auto-added to a party, F's first sign-in
// (UpsertUser on F's steam_id_64) returns the same users.id the membership references
// and sets ever_logged_in=true. Exactly one users row exists for F, F holds exactly one
// membership in that party, and PartyMembers still lists F.
func TestFirstSignInKeepsMembership(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)

	const leaderSteamID = "76561198000000901"
	const friendSteamID = "76561198000000902"
	const friendInitialPersona = "Never Signed In Friend"
	const friendSignInPersona = "Signed In Friend"

	leaderUserID, err := store.UpsertUser(ctx, leaderSteamID, steamapi.Player{
		SteamID:     leaderSteamID,
		PersonaName: "Party Leader",
	})
	if err != nil {
		t.Fatal(err)
	}

	partyID, err := store.CreateParty(ctx, leaderUserID, "Sign-In Test Party")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create never-signed-in friend row via UpsertFriend (as sync / add does).
	friendUserID, err := store.UpsertFriend(ctx, friendSteamID, steamapi.Player{
		SteamID:     friendSteamID,
		PersonaName: friendInitialPersona,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Auto-add never-signed-in friend to party via store.SendInvite.
	if err := store.SendInvite(ctx, partyID, leaderUserID, friendUserID); err != nil {
		t.Fatalf("SendInvite failed: %v", err)
	}

	// Verify membership exists before first sign-in.
	membersBefore, err := store.PartyMembers(ctx, partyID)
	if err != nil {
		t.Fatal(err)
	}
	var foundBefore bool
	for _, m := range membersBefore {
		if m.UserID == friendUserID {
			foundBefore = true
			break
		}
	}
	if !foundBefore {
		t.Fatalf("friendUserID %d not found in party members before sign-in: %+v", friendUserID, membersBefore)
	}

	// 3. First sign-in by friend: UpsertUser on friendSteamID.
	signedInUserID, err := store.UpsertUser(ctx, friendSteamID, steamapi.Player{
		SteamID:     friendSteamID,
		PersonaName: friendSignInPersona,
	})
	if err != nil {
		t.Fatalf("UpsertUser failed: %v", err)
	}

	// Assert the returned id equals the recorded friendUserID.
	if signedInUserID != friendUserID {
		t.Fatalf("UpsertUser returned id %d, want existing friend id %d", signedInUserID, friendUserID)
	}

	// Assert exactly 1 users row for friendSteamID.
	var userRowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE steam_id_64 = $1`, friendSteamID).Scan(&userRowCount); err != nil {
		t.Fatal(err)
	}
	if userRowCount != 1 {
		t.Fatalf("users count for steam_id_64 %s is %d, want 1", friendSteamID, userRowCount)
	}

	// Assert ever_logged_in is true.
	var everLoggedIn bool
	if err := pool.QueryRow(ctx, `SELECT ever_logged_in FROM users WHERE id = $1`, friendUserID).Scan(&everLoggedIn); err != nil {
		t.Fatal(err)
	}
	if !everLoggedIn {
		t.Fatalf("expected ever_logged_in to be true, got false")
	}

	// Assert exactly 1 membership for (partyID, friendUserID).
	var membershipCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1 AND user_id = $2`, partyID, friendUserID).Scan(&membershipCount); err != nil {
		t.Fatal(err)
	}
	if membershipCount != 1 {
		t.Fatalf("memberships count for (party=%s, user=%d) is %d, want 1", partyID, friendUserID, membershipCount)
	}

	// Assert PartyMembers still lists F.
	membersAfter, err := store.PartyMembers(ctx, partyID)
	if err != nil {
		t.Fatal(err)
	}
	var foundAfter bool
	for _, m := range membersAfter {
		if m.UserID == friendUserID {
			foundAfter = true
			if m.Name != friendSignInPersona {
				t.Fatalf("PartyMember name is %q, want updated persona %q", m.Name, friendSignInPersona)
			}
			break
		}
	}
	if !foundAfter {
		t.Fatalf("friendUserID %d not found in party members after sign-in: %+v", friendUserID, membersAfter)
	}
}
