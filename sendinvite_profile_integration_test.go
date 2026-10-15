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
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

type storedUserRow struct {
	persona, avatarSmall, avatarMedium, avatarFull string
	everLoggedIn                                   bool
	updatedAt                                      time.Time
}

func readUserRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, steamID string) storedUserRow {
	t.Helper()
	var row storedUserRow
	if err := pool.QueryRow(ctx, `SELECT persona_name, avatar_small, avatar_medium, avatar_full, ever_logged_in, updated_at FROM users WHERE steam_id_64 = $1`, steamID).
		Scan(&row.persona, &row.avatarSmall, &row.avatarMedium, &row.avatarFull, &row.everLoggedIn, &row.updatedAt); err != nil {
		t.Fatal(err)
	}
	return row
}

func tableCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) [3]int {
	t.Helper()
	var counts [3]int
	for i, table := range []string{"invites", "memberships", "users"} {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&counts[i]); err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

// postAddAsLeader drives POST /parties/{id}/invites/send through app.routes()
// with a real session for the leader.
func postAddAsLeader(t *testing.T, ctx context.Context, store *db.Store, fakeSteam *endToEndFakeSteam, leaderUserID int64, partyID, target string) *httptest.ResponseRecorder {
	t.Helper()
	config := appConfig{SessionSecret: "send-invite-profile-secret"}
	app := application{store: store, config: config, steam: func() view.SteamService { return fakeSteam }}
	token, err := store.CreateSession(ctx, leaderUserID)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(config.SessionSecret))
	mac.Write([]byte(token))
	req := httptest.NewRequest(http.MethodPost, "/parties/"+partyID+"/invites/send", strings.NewReader("target="+target)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))})
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	return rec
}

// A signed-in friend whom Steam Players omits is invited without their stored
// profile being overwritten by an empty fallback, and stays signed in.
func TestSendInviteKeepsSignedInFriendProfile(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)
	const leaderSteamID = "76561198000000951"
	const friendSteamID = "76561198000000952"

	leaderUserID, err := store.UpsertUser(ctx, leaderSteamID, steamapi.Player{SteamID: leaderSteamID, PersonaName: "Leader"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertUser(ctx, friendSteamID, steamapi.Player{
		SteamID: friendSteamID, PersonaName: "Signed In Friend",
		AvatarSmall: "s.jpg", AvatarMedium: "m.jpg", AvatarFull: "f.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	partyID, err := store.CreateParty(ctx, leaderUserID, "Profile Party")
	if err != nil {
		t.Fatal(err)
	}
	before := readUserRow(t, ctx, pool, friendSteamID)

	fakeSteam := &endToEndFakeSteam{
		friends: []steamapi.Friend{{SteamID: friendSteamID, Relationship: "friend"}},
		players: map[string]steamapi.Player{}, // Players omits the friend
	}
	rec := postAddAsLeader(t, ctx, store, fakeSteam, leaderUserID, partyID, friendSteamID)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	after := readUserRow(t, ctx, pool, friendSteamID)
	if after != before {
		t.Fatalf("signed-in friend's row changed: before=%+v after=%+v", before, after)
	}
	if !after.everLoggedIn {
		t.Fatal("ever_logged_in was demoted")
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invites i JOIN users u ON u.id = i.user_id WHERE i.party_id = $1 AND u.steam_id_64 = $2 AND i.status = 'pending'`, partyID, friendSteamID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending invites for friend = %d, want 1", pending)
	}
}

// A refused add for a friend who already has a pending invite redirects back
// to the party page and writes nothing, including their users row.
func TestSendInvitePendingInviteeRowUntouched(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)
	const leaderSteamID = "76561198000000961"
	const friendSteamID = "76561198000000962"

	leaderUserID, err := store.UpsertUser(ctx, leaderSteamID, steamapi.Player{SteamID: leaderSteamID, PersonaName: "Leader"})
	if err != nil {
		t.Fatal(err)
	}
	friendUserID, err := store.UpsertUser(ctx, friendSteamID, steamapi.Player{
		SteamID: friendSteamID, PersonaName: "Invited Friend",
		AvatarSmall: "s.jpg", AvatarMedium: "m.jpg", AvatarFull: "f.jpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	partyID, err := store.CreateParty(ctx, leaderUserID, "Pending Party")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, partyID, leaderUserID, friendUserID); err != nil {
		t.Fatal(err)
	}
	beforeRow := readUserRow(t, ctx, pool, friendSteamID)
	beforeCounts := tableCounts(t, ctx, pool)

	fakeSteam := &endToEndFakeSteam{
		friends: []steamapi.Friend{{SteamID: friendSteamID, Relationship: "friend"}},
		players: map[string]steamapi.Player{friendSteamID: {
			SteamID: friendSteamID, PersonaName: "Renamed On Steam", AvatarMedium: "new.jpg",
		}},
	}
	rec := postAddAsLeader(t, ctx, store, fakeSteam, leaderUserID, partyID, friendSteamID)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/parties/"+partyID {
		t.Fatalf("status=%d location=%q, want 303 to /parties/%s", rec.Code, rec.Header().Get("Location"), partyID)
	}
	if afterRow := readUserRow(t, ctx, pool, friendSteamID); afterRow != beforeRow {
		t.Fatalf("pending invitee's row changed: before=%+v after=%+v", beforeRow, afterRow)
	}
	if afterCounts := tableCounts(t, ctx, pool); afterCounts != beforeCounts {
		t.Fatalf("invites/memberships/users counts changed: before=%v after=%v", beforeCounts, afterCounts)
	}
}
