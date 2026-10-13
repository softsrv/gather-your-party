//go:build integration

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
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
	for _, path := range []string{"migrations/0001_init.sql", "migrations/0002_parties.sql", "migrations/0003_drop_rejection_tallies.sql", "migrations/0004_games.sql"} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
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
	for _, path := range []string{"migrations/0001_init.sql", "migrations/0002_parties.sql", "migrations/0003_drop_rejection_tallies.sql", "migrations/0004_games.sql"} {
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unauthorized memberships=%d err=%v", count, err)
	}
	request("reject", targetCookie, fmt.Sprintf("user_id=%d", leader), 200)
	if err := pool.QueryRow(ctx, `SELECT status FROM invites WHERE party_id=$1 AND user_id=$2`, party, target).Scan(&status); err != nil || status != "rejected" {
		t.Fatalf("status=%s err=%v", status, err)
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
	mockEmptyPartyLibraries(t)
	store, pool, ctx := inviteIntegrationStore(t)
	profiles := []steamapi.Player{
		{PersonaName: "Private leader", AvatarMedium: "https://example.com/private-leader.jpg"},
		{PersonaName: "Private member", AvatarMedium: "https://example.com/private-member.jpg"},
		{PersonaName: "Outsider", AvatarMedium: "https://example.com/outsider.jpg"},
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
			for _, value := range []string{profile.PersonaName, profile.AvatarMedium} {
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
	if allowed.Code != http.StatusOK || strings.Contains(allowed.Body.String(), profiles[2].PersonaName) || strings.Contains(allowed.Body.String(), profiles[2].AvatarMedium) {
		t.Errorf("member response=%d %s", allowed.Code, allowed.Body.String())
	}
	bypassed := request(partyDetailGuardBypass{store}, outsiderCookie)
	assertRoster(bypassed, true)
	if bypassed.Code != http.StatusOK {
		t.Errorf("bypassed guard status=%d want=200", bypassed.Code)
	}
}

type pickerIntegrationFriends struct {
	friends   []steamapi.Friend
	requested string
}

func (s *pickerIntegrationFriends) Friends(_ context.Context, steamID string) ([]steamapi.Friend, error) {
	s.requested = steamID
	return s.friends, nil
}

// CLM-5/6: drive the exported view builder against the real store, substituting
// only Steam's network boundary with its declared Friend response shape.
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
	service := &pickerIntegrationFriends{friends: []steamapi.Friend{
		{SteamID: steamIDs[0]}, {SteamID: steamIDs[1]}, {SteamID: steamIDs[2]},
		{SteamID: steamIDs[3]}, {SteamID: "76561198999999999"},
	}}
	got, err := view.BuildInviteCandidates(ctx, service, store, party, ids[0], steamIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if service.requested != steamIDs[0] || len(got) != 1 || got[0].UserID != ids[3] || got[0].Name != "Stored 3" {
		t.Fatalf("requested=%s candidates=%+v", service.requested, got)
	}
	// A second build must fetch the current roster rather than reuse stale friends.
	service.friends = nil
	got, err = view.BuildInviteCandidates(ctx, service, store, party, ids[0], steamIDs[0])
	if err != nil || len(got) != 0 {
		t.Fatalf("empty live roster: %+v %v", got, err)
	}
}

// loginSteam is a view.SteamService double at the Steam boundary for the login
// sync; the store, router, session and OpenID verification are real.
type loginSteam struct {
	mu       sync.Mutex
	friends  []string
	profiles map[string]steamapi.Player
	// games answers owned-games requests; nil returns an empty library.
	games      func(ctx context.Context, steamID string) ([]steamapi.Game, error)
	gamesCalls []string
}

func (s *loginSteam) Games(ctx context.Context, steamID string, _, _ bool) ([]steamapi.Game, error) {
	s.mu.Lock()
	s.gamesCalls = append(s.gamesCalls, steamID)
	games := s.games
	s.mu.Unlock()
	if games == nil {
		return nil, nil
	}
	return games(ctx, steamID)
}

func (s *loginSteam) GamesDetails(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
	details := make([]steamapi.GameDetail, len(ids))
	for i, id := range ids {
		details[i] = steamapi.GameDetail{AppID: id, Multiplayer: true}
	}
	return details, nil
}

func (s *loginSteam) Friends(context.Context, string) ([]steamapi.Friend, error) {
	friends := make([]steamapi.Friend, len(s.friends))
	for i, id := range s.friends {
		friends[i] = steamapi.Friend{SteamID: id, Relationship: "friend", FriendSince: 1700000000}
	}
	return friends, nil
}

// Players answers GetPlayerSummaries for known profiles, omitting unknown ids
// as Steam does.
func (s *loginSteam) Players(_ context.Context, ids []string) ([]steamapi.Player, error) {
	var players []steamapi.Player
	for _, id := range ids {
		if id == testSteamID {
			players = append(players, steamapi.Player{SteamID: id, PersonaName: "Login user", AvatarSmall: "s", AvatarMedium: "m", AvatarFull: "f"})
		} else if p, ok := s.profiles[id]; ok {
			players = append(players, p)
		}
	}
	return players, nil
}

func (s *loginSteam) GetNumberOfCurrentPlayers(context.Context, string) (int, error) {
	return 0, errors.New("unexpected GetNumberOfCurrentPlayers call")
}

func (s *loginSteam) requestedGames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.gamesCalls)
}

type loginFixture struct {
	store *db.Store
	pool  *pgxpool.Pool
	ctx   context.Context
	steam *loginSteam
	app   *application
	done  chan error
}

// newLoginFixture wires the real store and router with the Steam double, an
// OpenID endpoint that answers is_valid, and a login sync completion hook.
func newLoginFixture(t *testing.T, valid bool) *loginFixture {
	t.Helper()
	store, pool, ctx := inviteIntegrationStore(t)
	openid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ns:http://specs.openid.net/auth/2.0\nis_valid:%t\n", valid)
	}))
	t.Cleanup(openid.Close)
	oldEndpoint := steamOpenIDEndpoint
	steamOpenIDEndpoint = openid.URL
	t.Cleanup(func() { steamOpenIDEndpoint = oldEndpoint })
	steam := &loginSteam{profiles: map[string]steamapi.Player{}}
	f := &loginFixture{store: store, pool: pool, ctx: ctx, steam: steam, done: make(chan error, 4)}
	f.app = &application{
		store:         store,
		config:        appConfig{SessionSecret: "login-sync-secret", AppBaseURL: "https://example.com"},
		steam:         func() view.SteamService { return steam },
		loginSyncDone: func(_ string, err error) { f.done <- err },
	}
	return f
}

// callback drives the Steam callback through the router on a request context
// that is cancelled as soon as the response is written. It fails if the
// response does not complete within five seconds.
func (f *loginFixture) callback(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	reqCtx, cancelReq := context.WithCancel(f.ctx)
	r := httptest.NewRequest(http.MethodGet, "/auth/steam/callback?"+callbackParams().Encode(), nil).WithContext(reqCtx)
	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		f.app.routes().ServeHTTP(w, r)
	}()
	select {
	case <-served:
		cancelReq()
	case <-time.After(5 * time.Second):
		cancelReq()
		t.Fatal("callback did not respond while the login sync was running")
	}
	return w
}

func (f *loginFixture) waitSync(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("login sync did not finish")
		return nil
	}
}

// assertSignedIn checks the 303, the cookie attributes and that the cookie's
// token is a stored session of the signed-in user.
func (f *loginFixture) assertSignedIn(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("callback status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%v", cookies)
	}
	c := cookies[0]
	if c.Name != "session" || c.Path != "/" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 7*24*3600 {
		t.Fatalf("session cookie attributes: %+v", c)
	}
	token, sig, ok := strings.Cut(c.Value, ".")
	mac := hmac.New(sha256.New, []byte(f.app.config.SessionSecret))
	mac.Write([]byte(token))
	if !ok || sig != hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("session cookie is not signed with the session secret")
	}
	var steamID string
	var everLoggedIn bool
	if err := f.pool.QueryRow(f.ctx, `SELECT u.steam_id_64, u.ever_logged_in FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token = $1`, token).Scan(&steamID, &everLoggedIn); err != nil {
		t.Fatalf("session row for cookie token: %v", err)
	}
	if steamID != testSteamID || !everLoggedIn {
		t.Fatalf("session user=%s ever_logged_in=%t", steamID, everLoggedIn)
	}
	return c
}

// Login sync CLM-1: the callback answers while the sync is held blocked, and
// the sync's context outlives the callback's request.
func TestLoginSyncDoesNotBlockCallback(t *testing.T) {
	f := newLoginFixture(t, true)
	f.app.loginSyncTimeout = 30 * time.Second
	entered, release := make(chan struct{}), make(chan struct{})
	type seen struct {
		err       error
		remaining time.Duration
	}
	atRelease := make(chan seen, 1)
	f.steam.games = func(ctx context.Context, _ string) ([]steamapi.Game, error) {
		close(entered)
		<-release
		deadline, _ := ctx.Deadline()
		atRelease <- seen{ctx.Err(), time.Until(deadline)}
		return nil, nil
	}
	w := f.callback(t)
	f.assertSignedIn(t, w)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("login sync never requested the library")
	}
	select {
	case err := <-f.done:
		t.Fatalf("sync finished while blocked: %v", err)
	default:
	}
	// The callback's request context is cancelled and its 5s budget would be
	// exceeded long before the sync's own 30s deadline.
	close(release)
	got := <-atRelease
	if got.err != nil {
		t.Fatalf("sync context ended with the callback: %v", got.err)
	}
	if got.remaining <= 5*time.Second {
		t.Fatalf("sync deadline %s is not its own", got.remaining)
	}
	if err := f.waitSync(t); err != nil {
		t.Fatalf("sync error: %v", err)
	}
}

// Login sync CLM-2: an erroring, panicking or never-answering Steam leaves the
// session and redirect intact, and the server keeps serving.
func TestLoginSyncFailuresAreContained(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		f := newLoginFixture(t, true)
		f.steam.games = func(context.Context, string) ([]steamapi.Game, error) {
			return nil, errors.New("steam unavailable")
		}
		var err error
		out := captureStdout(t, func() {
			f.assertSignedIn(t, f.callback(t))
			err = f.waitSync(t)
		})
		if err == nil || !strings.Contains(err.Error(), "steam unavailable") || !strings.Contains(out, "login sync error") {
			t.Fatalf("sync err=%v log=%q", err, out)
		}
	})
	t.Run("panic", func(t *testing.T) {
		f := newLoginFixture(t, true)
		f.steam.games = func(context.Context, string) ([]steamapi.Game, error) {
			panic("steam double exploded")
		}
		var cookie *http.Cookie
		var err error
		out := captureStdout(t, func() {
			cookie = f.assertSignedIn(t, f.callback(t))
			err = f.waitSync(t)
		})
		if err == nil || !strings.Contains(err.Error(), "panic: steam double exploded") || !strings.Contains(out, "steam double exploded") {
			t.Fatalf("sync err=%v log=%q", err, out)
		}
		// The process survived; a later request is served with the session.
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(f.ctx)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		f.app.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Login user") {
			t.Fatalf("later request status=%d body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("deadline", func(t *testing.T) {
		f := newLoginFixture(t, true)
		f.app.loginSyncTimeout = 200 * time.Millisecond
		f.steam.games = func(ctx context.Context, _ string) ([]steamapi.Game, error) {
			<-ctx.Done() // Steam never answers; only the context ends the call.
			return nil, ctx.Err()
		}
		start := time.Now()
		var err error
		captureStdout(t, func() {
			f.assertSignedIn(t, f.callback(t))
			err = f.waitSync(t)
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("sync err=%v, want its own deadline", err)
		}
		if elapsed := time.Since(start); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
			t.Fatalf("sync stopped after %s", elapsed)
		}
	})
}

// Login sync CLM-13: a valid callback with the sync wired stores the session
// and sets the secure cookie; an invalid assertion gets neither and no sync.
func TestLoginCallbackSessionWithSync(t *testing.T) {
	f := newLoginFixture(t, true)
	f.assertSignedIn(t, f.callback(t))
	if err := f.waitSync(t); err != nil {
		t.Fatal(err)
	}

	invalid := newLoginFixture(t, false)
	started := 0
	invalid.app.startLoginSync = func(int64, string) { started++ }
	w := invalid.callback(t)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" || len(w.Result().Cookies()) != 0 || started != 0 {
		t.Fatalf("invalid assertion: status=%d cookies=%v syncs=%d", w.Code, w.Result().Cookies(), started)
	}
	var sessions, users int
	if err := invalid.pool.QueryRow(invalid.ctx, `SELECT (SELECT count(*) FROM sessions), (SELECT count(*) FROM users)`).Scan(&sessions, &users); err != nil || sessions != 0 || users != 0 {
		t.Fatalf("invalid assertion stored sessions=%d users=%d err=%v", sessions, users, err)
	}
}

// Login sync CLM-14 + CLM-17: every friend gets a users row from its Players
// summary (new rows never logged in, a signed-in friend stays signed in), and
// only the logging-in user's library is requested.
func TestLoginSyncFriendsAndOwnLibraryOnly(t *testing.T) {
	f := newLoginFixture(t, true)
	const unknownA, unknownB, knownFriend = "76561198000000701", "76561198000000702", "76561198000000703"
	if _, err := f.store.UpsertUser(f.ctx, knownFriend, steamapi.Player{PersonaName: "Old name"}); err != nil {
		t.Fatal(err)
	}
	f.steam.friends = []string{unknownA, unknownB, knownFriend}
	want := map[string]steamapi.Player{
		unknownA:    {SteamID: unknownA, PersonaName: "Barney", AvatarSmall: "https://a/a.jpg", AvatarMedium: "https://a/a_medium.jpg", AvatarFull: "https://a/a_full.jpg"},
		unknownB:    {SteamID: unknownB, PersonaName: "Eli", AvatarSmall: "https://b/b.jpg", AvatarMedium: "https://b/b_medium.jpg", AvatarFull: "https://b/b_full.jpg"},
		knownFriend: {SteamID: knownFriend, PersonaName: "Kleiner", AvatarSmall: "https://k/k.jpg", AvatarMedium: "https://k/k_medium.jpg", AvatarFull: "https://k/k_full.jpg"},
	}
	f.steam.profiles = want
	f.steam.games = func(context.Context, string) ([]steamapi.Game, error) {
		return []steamapi.Game{{AppID: 440, Name: "Team Fortress 2"}}, nil
	}
	f.assertSignedIn(t, f.callback(t))
	if err := f.waitSync(t); err != nil {
		t.Fatal(err)
	}
	for id, p := range want {
		var got steamapi.Player
		var everLoggedIn bool
		if err := f.pool.QueryRow(f.ctx, `SELECT persona_name, avatar_small, avatar_medium, avatar_full, ever_logged_in FROM users WHERE steam_id_64 = $1`, id).
			Scan(&got.PersonaName, &got.AvatarSmall, &got.AvatarMedium, &got.AvatarFull, &everLoggedIn); err != nil {
			t.Fatalf("friend %s row: %v", id, err)
		}
		got.SteamID = id
		if got != p {
			t.Errorf("friend %s = %+v, want %+v", id, got, p)
		}
		if everLoggedIn != (id == knownFriend) {
			t.Errorf("friend %s ever_logged_in=%t", id, everLoggedIn)
		}
	}
	if got := f.steam.requestedGames(); !slices.Equal(got, []string{testSteamID}) {
		t.Fatalf("owned games requested for %v, want only %s", got, testSteamID)
	}
	if games, err := f.store.UserGames(f.ctx, testSteamID); err != nil || len(games) != 1 {
		t.Fatalf("user library=%v err=%v", games, err)
	}
}
