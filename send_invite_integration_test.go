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

	"gather-your-party/internal/db"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// inviteSendFixture is the production router over real Postgres; only the
// Steam surface is faked, and the friends it returns are the leader's.
type inviteSendFixture struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	store     *db.Store
	fakeSteam *endToEndFakeSteam
	router    http.Handler
	secret    string
}

func newInviteSendFixture(t *testing.T) *inviteSendFixture {
	store, pool, ctx := inviteIntegrationStore(t)
	f := &inviteSendFixture{t: t, ctx: ctx, pool: pool, store: store, secret: "invite-send-secret",
		fakeSteam: &endToEndFakeSteam{players: map[string]steamapi.Player{}}}
	app := application{store: store, config: appConfig{SessionSecret: f.secret}, steam: func() view.SteamService { return f.fakeSteam }}
	f.router = app.routes()
	return f
}

func (f *inviteSendFixture) signedIn(steamID, name string) int64 {
	f.t.Helper()
	id, err := f.store.UpsertUser(f.ctx, steamID, steamapi.Player{SteamID: steamID, PersonaName: name})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *inviteSendFixture) befriend(steamID, name, avatar string) {
	f.fakeSteam.mu.Lock()
	defer f.fakeSteam.mu.Unlock()
	f.fakeSteam.friends = append(f.fakeSteam.friends, steamapi.Friend{SteamID: steamID, Relationship: "friend"})
	f.fakeSteam.players[steamID] = steamapi.Player{SteamID: steamID, PersonaName: name, AvatarSmall: avatar + "-s", AvatarMedium: avatar + "-m", AvatarFull: avatar + "-f"}
}

func (f *inviteSendFixture) send(actingUserID int64, partyID, target string) *httptest.ResponseRecorder {
	f.t.Helper()
	token, err := f.store.CreateSession(f.ctx, actingUserID)
	if err != nil {
		f.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(f.secret))
	mac.Write([]byte(token))
	r := httptest.NewRequest(http.MethodPost, "/parties/"+partyID+"/invites/send", strings.NewReader("target="+target)).WithContext(f.ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))})
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}

func (f *inviteSendFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// CLM-3: adding a friend with no users row creates exactly one row carrying
// the Steam profile with ever_logged_in=false; adding a signed-in friend
// creates no second row and never demotes it.
func TestAddCreatesSingleFriendRow(t *testing.T) {
	f := newInviteSendFixture(t)
	const leaderSteamID, newFriendSteamID, signedInSteamID = "76561198000000901", "76561198000000902", "76561198000000903"
	leader := f.signedIn(leaderSteamID, "Leader")
	party, err := f.store.CreateParty(f.ctx, leader, "Single row")
	if err != nil {
		t.Fatal(err)
	}
	f.befriend(newFriendSteamID, "New Friend", "https://example.com/new")
	if w := f.send(leader, party, newFriendSteamID); w.Code != http.StatusSeeOther {
		t.Fatalf("add no-row friend status=%d body=%s", w.Code, w.Body.String())
	}
	if n := f.count(`SELECT count(*) FROM users WHERE steam_id_64 = $1`, newFriendSteamID); n != 1 {
		t.Fatalf("users rows for new friend=%d want 1", n)
	}
	var name, small, medium, full string
	var everLoggedIn bool
	if err := f.pool.QueryRow(f.ctx, `SELECT persona_name, avatar_small, avatar_medium, avatar_full, ever_logged_in FROM users WHERE steam_id_64 = $1`, newFriendSteamID).Scan(&name, &small, &medium, &full, &everLoggedIn); err != nil {
		t.Fatal(err)
	}
	if name != "New Friend" || small != "https://example.com/new-s" || medium != "https://example.com/new-m" || full != "https://example.com/new-f" || everLoggedIn {
		t.Fatalf("new friend row name=%q avatars=%q,%q,%q ever_logged_in=%v", name, small, medium, full, everLoggedIn)
	}

	f.signedIn(signedInSteamID, "Signed In")
	f.befriend(signedInSteamID, "Signed In Renamed", "https://example.com/si")
	if w := f.send(leader, party, signedInSteamID); w.Code != http.StatusSeeOther {
		t.Fatalf("add signed-in friend status=%d body=%s", w.Code, w.Body.String())
	}
	if n := f.count(`SELECT count(*) FROM users WHERE steam_id_64 = $1`, signedInSteamID); n != 1 {
		t.Fatalf("users rows for signed-in friend=%d want 1", n)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT ever_logged_in FROM users WHERE steam_id_64 = $1`, signedInSteamID).Scan(&everLoggedIn); err != nil {
		t.Fatal(err)
	}
	if !everLoggedIn {
		t.Fatal("adding a signed-in friend demoted ever_logged_in")
	}
}

// CLM-4: every refusal redirects back to the party page and writes no invite,
// membership or users row. non_friend is the discriminating case: the old
// handler inserted an invite for any target.
func TestSendInviteRefusals(t *testing.T) {
	const leaderSteamID, memberSteamID, friendSteamID, strangerSteamID, pendingSteamID = "76561198000000911", "76561198000000912", "76561198000000913", "76561198000000914", "76561198000000915"
	setup := func(t *testing.T) (*inviteSendFixture, string, int64, int64) {
		f := newInviteSendFixture(t)
		leader := f.signedIn(leaderSteamID, "Leader")
		member := f.signedIn(memberSteamID, "Member")
		party, err := f.store.CreateParty(f.ctx, leader, "Refusals")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, party, member); err != nil {
			t.Fatal(err)
		}
		f.befriend(memberSteamID, "Member", "https://example.com/m")
		f.befriend(friendSteamID, "Friend", "https://example.com/f")
		return f, party, leader, member
	}
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, f *inviteSendFixture, party string, leader, member int64) (actor int64, target string)
	}{
		{name: "non_leader", prepare: func(_ *testing.T, _ *inviteSendFixture, _ string, _, member int64) (int64, string) {
			return member, friendSteamID
		}},
		{name: "non_friend", prepare: func(t *testing.T, f *inviteSendFixture, _ string, leader, _ int64) (int64, string) {
			// The stranger has a users row, so only the friendship check refuses.
			f.signedIn(strangerSteamID, "Stranger")
			return leader, strangerSteamID
		}},
		{name: "already_member", prepare: func(_ *testing.T, _ *inviteSendFixture, _ string, leader, _ int64) (int64, string) {
			return leader, memberSteamID
		}},
		{name: "pending_invite", prepare: func(t *testing.T, f *inviteSendFixture, party string, leader, _ int64) (int64, string) {
			pending := f.signedIn(pendingSteamID, "Pending")
			f.befriend(pendingSteamID, "Pending", "https://example.com/p")
			if _, err := f.pool.Exec(f.ctx, `INSERT INTO invites (party_id, user_id, inviter_id, status) VALUES ($1, $2, $3, 'pending')`, party, pending, leader); err != nil {
				t.Fatal(err)
			}
			return leader, pendingSteamID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, party, leader, member := setup(t)
			actor, target := tc.prepare(t, f, party, leader, member)
			snapshot := func() [3]int {
				return [3]int{
					f.count(`SELECT count(*) FROM invites`),
					f.count(`SELECT count(*) FROM memberships`),
					f.count(`SELECT count(*) FROM users`),
				}
			}
			before := snapshot()
			w := f.send(actor, party, target)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/parties/"+party {
				t.Fatalf("status=%d location=%q body=%s", w.Code, w.Header().Get("Location"), w.Body.String())
			}
			if after := snapshot(); after != before {
				t.Fatalf("rows changed: before=%v after=%v (invites, memberships, users)", before, after)
			}
		})
	}
}
