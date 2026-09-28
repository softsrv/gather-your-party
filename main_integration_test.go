//go:build integration

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Use a separate schema so this test and internal/db's suite can run concurrently.
// TEST_DATABASE_URL must still refer to a disposable test database.
func TestLogoutInvalidatesSession(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name an empty, disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	defer admin.Close()
	schema := fmt.Sprintf("logout_test_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	defer pool.Close()
	migration, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	store := db.New(pool)
	userID, err := store.UpsertUser(ctx, testSteamID, steamapi.Player{PersonaName: "Alyx"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateSession(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	app := application{store: store, config: appConfig{SessionSecret: "integration-test-secret"}}
	auth := &middleware.Authenticator{Resolver: app.store, Secret: []byte(app.config.SessionSecret)}
	mac := hmac.New(sha256.New, []byte(app.config.SessionSecret))
	mac.Write([]byte(token))
	cookie := &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	request := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil).WithContext(ctx)
		r.AddCookie(cookie)
		return r
	}
	before := &middleware.CustomContext{Context: ctx, StartTime: time.Now()}
	if err := auth.LoadSteamId(before, httptest.NewRecorder(), request(http.MethodGet, "/")); err != nil || before.Value(middleware.SteamID{}) != testSteamID {
		t.Fatal("session must authenticate before logout")
	}
	w := httptest.NewRecorder()
	app.handleLogout(w, request(http.MethodPost, "/auth/steam/logout"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatal("logout must redirect home")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "session" || cookies[0].Value != "" || cookies[0].Path != "/" || cookies[0].MaxAge != -1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("logout did not securely clear the browser cookie")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token = $1`, token).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session row count after logout = %d, error %v", count, err)
	}
	if identity, ok, err := store.ResolveSession(ctx, token); err != nil || ok || identity != "" {
		t.Fatal("logged-out token still resolves")
	}
	// Re-present the original signed cookie, not the browser's cleared cookie.
	after := &middleware.CustomContext{Context: ctx, StartTime: time.Now()}
	replay := httptest.NewRecorder()
	if err := auth.LoadSteamId(after, replay, request(http.MethodGet, "/")); err != nil || after.Value(middleware.SteamID{}) != nil || len(replay.Result().Cookies()) != 0 {
		t.Fatal("replaying a logged-out cookie must remain unauthenticated")
	}
}

func inviteIntegrationStore(t *testing.T) (*db.Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name a disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	t.Cleanup(admin.Close)
	schema := pgx.Identifier{fmt.Sprintf("invite_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	t.Cleanup(pool.Close)
	for _, path := range []string{"migrations/0001_init.sql", "migrations/0002_parties.sql"} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	return db.New(pool), pool, ctx
}

// CLM-1/4/14 at the integration rung: real session resolution, router, and database mutations.
func TestInviteRoutesRealStore(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)
	newUser := func(steamID string) int64 {
		t.Helper()
		id, err := store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: "Invite user"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	leader := newUser("76561198000000301")
	target := newUser("76561198000000302")
	member := newUser("76561198000000303")
	party, err := store.CreateParty(ctx, leader, "Router invites")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2)`, party, member); err != nil {
		t.Fatal(err)
	}
	app := application{store: store, config: appConfig{SessionSecret: "invite-integration-secret"}}
	router := app.routes()
	cookieFor := func(userID int64) *http.Cookie {
		t.Helper()
		token, err := store.CreateSession(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(app.config.SessionSecret))
		mac.Write([]byte(token))
		return &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	}
	leaderCookie, targetCookie, memberCookie := cookieFor(leader), cookieFor(target), cookieFor(member)
	request := func(action string, cookie *http.Cookie, body string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/parties/"+party+"/invites/"+action, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s status=%d want=%d body=%s", action, w.Code, want, w.Body.String())
		}
	}
	sendBody := fmt.Sprintf("target=%d&user_id=%d&steamID=forged", target, leader)
	request("send", nil, sendBody, 401)
	request("send", memberCookie, sendBody, 500)
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invites WHERE party_id=$1`, party).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized invites=%d err=%v", count, err)
	}
	request("send", leaderCookie, sendBody, 200)
	for _, action := range []string{"accept", "reject"} {
		request(action, nil, fmt.Sprintf("user_id=%d", target), 401)
		request(action, memberCookie, fmt.Sprintf("user_id=%d", target), 500)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM invites WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM rejection_tallies WHERE party_id=$1`, party).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized tallies=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized memberships=%d err=%v", count, err)
	}
	request("reject", targetCookie, fmt.Sprintf("user_id=%d", leader), 200)
	if err := pool.QueryRow(ctx, `SELECT count FROM rejection_tallies WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&count); err != nil || count != 1 {
		t.Fatalf("tally=%d err=%v", count, err)
	}
	request("send", leaderCookie, sendBody, 200)
	request("accept", targetCookie, fmt.Sprintf("user_id=%d", leader), 200)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id=$1 AND user_id=$2 AND joined_at IS NOT NULL`, party, target).Scan(&count); err != nil || count != 1 {
		t.Fatalf("accepted memberships=%d err=%v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invites WHERE party_id=$1 AND status='pending'`, party).Scan(&count); err != nil || count != 0 {
		t.Fatalf("pending=%d err=%v", count, err)
	}
	// A correctly signed but revoked session is not a verified identity.
	if err := store.DeleteSession(ctx, strings.SplitN(leaderCookie.Value, ".", 2)[0]); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"send", "accept", "reject"} {
		request(action, leaderCookie, sendBody, 401)
	}
}

// partyDetailGuardBypass substitutes only the authorization decision; roster and
// identity queries still execute against the real database. This intervention
// demonstrates that the denied response depends on the membership guard.
type partyDetailGuardBypass struct {
	*db.Store
}

func (s partyDetailGuardBypass) IsMember(context.Context, string, int64) (bool, error) {
	return true, nil
}

// Party detail CLM-2: the same authenticated non-member is denied normally but
// receives real member names/avatars when the guard decision is bypassed.
// Removing the handler's guard makes the first absence assertions fail.
func TestPartyDetailNonMemberGuardRealStore(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)
	profiles := []steamapi.Player{
		{PersonaName: "Private leader", AvatarSmall: "https://example.com/private-leader.jpg"},
		{PersonaName: "Private member", AvatarSmall: "https://example.com/private-member.jpg"},
		{PersonaName: "Outsider", AvatarSmall: "https://example.com/outsider.jpg"},
	}
	ids := make([]int64, len(profiles))
	for i, profile := range profiles {
		var err error
		ids[i], err = store.UpsertUser(ctx, fmt.Sprintf("765611980000006%02d", i), profile)
		if err != nil {
			t.Fatal(err)
		}
	}
	party, err := store.CreateParty(ctx, ids[0], "Private roster")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2)`, party, ids[1]); err != nil {
		t.Fatal(err)
	}
	// Being a member elsewhere must not grant access to this party.
	if _, err := store.CreateParty(ctx, ids[2], "Outsider's party"); err != nil {
		t.Fatal(err)
	}
	config := appConfig{SessionSecret: "party-detail-integration-secret"}
	cookieFor := func(userID int64) *http.Cookie {
		t.Helper()
		token, err := store.CreateSession(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(config.SessionSecret))
		mac.Write([]byte(token))
		return &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	}
	outsiderCookie := cookieFor(ids[2])
	request := func(lookup sessionStore, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		app := application{store: lookup, config: config}
		r := httptest.NewRequest(http.MethodGet, "/parties/"+party, nil).WithContext(ctx)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, r)
		return w
	}
	assertRoster := func(w *httptest.ResponseRecorder, present bool) {
		t.Helper()
		for _, profile := range profiles[:2] {
			for _, value := range []string{profile.PersonaName, profile.AvatarSmall} {
				if strings.Contains(w.Body.String(), value) != present {
					t.Errorf("roster value %q present=%t want=%t: %s", value, !present, present, w.Body.String())
				}
			}
		}
	}
	denied := request(store, outsiderCookie)
	assertRoster(denied, false)
	if denied.Code != http.StatusForbidden {
		t.Errorf("non-member status=%d want=403", denied.Code)
	}
	anonymous := request(store, nil)
	assertRoster(anonymous, false)
	if anonymous.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status=%d want=401", anonymous.Code)
	}
	// Positive control: ordinary members, not just leaders, see the full roster.
	allowed := request(store, cookieFor(ids[1]))
	assertRoster(allowed, true)
	if allowed.Code != http.StatusOK || strings.Contains(allowed.Body.String(), profiles[2].PersonaName) || strings.Contains(allowed.Body.String(), profiles[2].AvatarSmall) {
		t.Errorf("member response=%d %s", allowed.Code, allowed.Body.String())
	}
	bypassed := request(partyDetailGuardBypass{store}, outsiderCookie)
	assertRoster(bypassed, true)
	if bypassed.Code != http.StatusOK {
		t.Errorf("bypassed guard status=%d want=200", bypassed.Code)
	}
}

type pickerIntegrationFriends struct {
	players   []steamapi.Player
	requested string
}

func (s *pickerIntegrationFriends) Friends(_ context.Context, steamID string) ([]steamapi.Player, error) {
	s.requested = steamID
	return s.players, nil
}

// CLM-5/6: drive the exported view builder against the real store, substituting
// only Steam's network boundary with its declared Player response shape.
func TestInvitePickerBuilderRealStore(t *testing.T) {
	store, pool, ctx := inviteIntegrationStore(t)
	steamIDs := []string{"76561198000000401", "76561198000000402", "76561198000000403", "76561198000000404", "76561198000000405"}
	ids := make([]int64, len(steamIDs))
	for i, steamID := range steamIDs {
		id, err := store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: fmt.Sprintf("Stored %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	party, err := store.CreateParty(ctx, ids[0], "View picker")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the leader exclusion independent of the membership exclusion.
	if _, err := pool.Exec(ctx, `DELETE FROM memberships WHERE party_id=$1 AND user_id=$2`, party, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2)`, party, ids[1]); err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, party, ids[0], ids[2]); err != nil {
		t.Fatal(err)
	}
	service := &pickerIntegrationFriends{players: []steamapi.Player{
		{SteamID: steamIDs[0]}, {SteamID: steamIDs[1]}, {SteamID: steamIDs[2]},
		{SteamID: steamIDs[3], PersonaName: "Live name"}, {SteamID: "76561198999999999"},
	}}
	got, err := view.BuildInviteCandidates(ctx, service, store, party, ids[0], steamIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if service.requested != steamIDs[0] || len(got) != 1 || got[0].UserID != ids[3] || got[0].Name != "Stored 3" {
		t.Fatalf("requested=%s candidates=%+v", service.requested, got)
	}
	// A second build must fetch the current roster rather than reuse stale friends.
	service.players = nil
	got, err = view.BuildInviteCandidates(ctx, service, store, party, ids[0], steamIDs[0])
	if err != nil || len(got) != 0 {
		t.Fatalf("empty live roster: %+v %v", got, err)
	}
}
