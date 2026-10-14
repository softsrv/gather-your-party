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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// lsSyncElement is the navbar's load-triggered library sync element.
const lsSyncElement = `hx-get="/library/sync"`

// lsLockNamespace and lsLockKey mirror internal/db's user sync lock keys.
const lsLockNamespace int32 = 0x4c696273

func lsLockKey(userID int64) int32 { return int32(userID) ^ int32(userID>>32) }

type lsFixture struct {
	store   *db.Store
	pool    *pgxpool.Pool
	ctx     context.Context
	steam   *loginSteam
	app     *application
	userID  int64
	token   string
	cookie  *http.Cookie
	partyID string
}

const lsSecret = "library-sync-secret"

func lsSignedCookie(token string) *http.Cookie {
	mac := hmac.New(sha256.New, []byte(lsSecret))
	mac.Write([]byte(token))
	return &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
}

// lsNewFixture wires the real store and router with the Steam double, a
// signed-in user with a real session and a party they lead.
func lsNewFixture(t *testing.T) *lsFixture {
	t.Helper()
	store, pool, ctx := inviteIntegrationStore(t)
	steam := &loginSteam{profiles: map[string]steamapi.Player{}}
	f := &lsFixture{store: store, pool: pool, ctx: ctx, steam: steam}
	f.app = lsApp(store, steam)
	var err error
	if f.userID, err = store.UpsertUser(ctx, testSteamID, steamapi.Player{SteamID: testSteamID, PersonaName: "Login user"}); err != nil {
		t.Fatal(err)
	}
	if f.token, err = store.CreateSession(ctx, f.userID); err != nil {
		t.Fatal(err)
	}
	f.cookie = lsSignedCookie(f.token)
	if f.partyID, err = store.CreateParty(ctx, f.userID, "Sync party"); err != nil {
		t.Fatal(err)
	}
	return f
}

func lsApp(store *db.Store, steam view.SteamService) *application {
	return &application{
		store:  store,
		config: appConfig{SessionSecret: lsSecret, AppBaseURL: "https://example.com"},
		steam:  func() view.SteamService { return steam },
	}
}

// lsSecondPool opens another pool on the fixture's schema, as a second
// instance would.
func lsSecondPool(t *testing.T, f *lsFixture) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(f.ctx, f.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func (f *lsFixture) pages() []string {
	return []string{"/", "/parties", "/parties/" + f.partyID, "/parties/" + f.partyID + "/leave"}
}

func lsServe(app *application, ctx context.Context, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, r)
	return w
}

func (f *lsFixture) get(path string) *httptest.ResponseRecorder {
	return lsServe(f.app, f.ctx, path, f.cookie)
}

// sync calls the sync route and returns its body and the Steam sync calls it
// made.
func (f *lsFixture) sync(t *testing.T) (string, int) {
	t.Helper()
	before := f.steam.syncCalls()
	var w *httptest.ResponseRecorder
	captureStdout(t, func() { w = f.get("/library/sync") })
	if w.Code != http.StatusOK {
		t.Fatalf("library sync status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String(), f.steam.syncCalls() - before
}

func (f *lsFixture) syncState(t *testing.T) (*time.Time, *string) {
	t.Helper()
	var at *time.Time
	var reason *string
	if err := f.pool.QueryRow(f.ctx, `SELECT library_synced_at, library_sync_error FROM users WHERE id = $1`, f.userID).Scan(&at, &reason); err != nil {
		t.Fatal(err)
	}
	return at, reason
}

func (f *lsFixture) assertPagesShowSync(t *testing.T, want bool) {
	t.Helper()
	for _, path := range f.pages() {
		var w *httptest.ResponseRecorder
		captureStdout(t, func() { w = f.get(path) })
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "Login user") {
			t.Fatalf("%s status=%d, not served as the user: %s", path, w.Code, body)
		}
		has := strings.Contains(body, lsSyncElement) && strings.Contains(body, `hx-trigger="load"`)
		if has != want {
			t.Fatalf("%s sync element present=%t, want %t", path, has, want)
		}
	}
}

func (f *lsFixture) setStamp(t *testing.T, ago time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET library_synced_at = now() - $2::interval WHERE id = $1`, f.userID, ago); err != nil {
		t.Fatal(err)
	}
}

func lsLibrary(context.Context, string) ([]steamapi.Game, error) {
	return []steamapi.Game{{AppID: 440, Name: "Team Fortress 2"}, {AppID: 570, Name: "Dota 2"}}, nil
}

// CLM-1: every signed-in page carries the load-triggered sync element, and the
// sync route has refreshed friends, stored the library and stamped success by
// the time it returns.
func TestLibrarySyncPagesAndRoute(t *testing.T) {
	f := lsNewFixture(t)
	const friendA, friendB = "76561198000000801", "76561198000000802"
	f.steam.friends = []string{friendA, friendB}
	f.steam.profiles = map[string]steamapi.Player{
		friendA: {SteamID: friendA, PersonaName: "Alyx"},
		friendB: {SteamID: friendB, PersonaName: "Gordon"},
	}
	f.steam.games = lsLibrary
	f.assertPagesShowSync(t, true)

	body, _ := f.sync(t)
	if strings.Contains(body, "Library sync failed") || strings.Contains(body, "loading-spinner") {
		t.Fatalf("sync body=%s", body)
	}
	var friends, games int
	if err := f.pool.QueryRow(f.ctx, `SELECT (SELECT count(*) FROM users WHERE steam_id_64 = ANY($1)), (SELECT count(*) FROM user_games WHERE user_id = $2)`,
		[]string{friendA, friendB}, f.userID).Scan(&friends, &games); err != nil {
		t.Fatal(err)
	}
	if friends != 2 || games != 2 {
		t.Fatalf("friends rows=%d user_games=%d", friends, games)
	}
	if at, _ := f.syncState(t); at == nil {
		t.Fatal("success stamp not set")
	}
	f.assertPagesShowSync(t, false)
}

// CLM-4: only an unset or over-24h stamp is due; a fresh stamp shows no element
// and the route makes no Steam call and writes nothing.
func TestLibrarySyncDueRule(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		f := lsNewFixture(t)
		f.steam.games = lsLibrary
		f.assertPagesShowSync(t, true)
		if _, calls := f.sync(t); calls == 0 {
			t.Fatal("due sync made no Steam call")
		}
		if at, _ := f.syncState(t); at == nil {
			t.Fatal("stamp not set")
		}
	})
	t.Run("fresh", func(t *testing.T) {
		f := lsNewFixture(t)
		f.steam.games = lsLibrary
		f.setStamp(t, time.Hour)
		before, _ := f.syncState(t)
		f.assertPagesShowSync(t, false)
		body, calls := f.sync(t)
		if calls != 0 || len(f.steam.requestedGames()) != 0 || strings.Contains(body, "Library sync failed") {
			t.Fatalf("fresh sync calls=%d body=%s", calls, body)
		}
		after, reason := f.syncState(t)
		var games int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM user_games WHERE user_id = $1`, f.userID).Scan(&games); err != nil {
			t.Fatal(err)
		}
		if !after.Equal(*before) || reason != nil || games != 0 {
			t.Fatalf("fresh sync wrote: stamp %v -> %v reason=%v games=%d", before, after, reason, games)
		}
	})
	t.Run("stale", func(t *testing.T) {
		f := lsNewFixture(t)
		f.steam.games = lsLibrary
		f.setStamp(t, 25*time.Hour)
		f.assertPagesShowSync(t, true)
		if _, calls := f.sync(t); calls == 0 {
			t.Fatal("stale sync made no Steam call")
		}
		if at, _ := f.syncState(t); at == nil || time.Since(*at) > time.Hour {
			t.Fatalf("stamp not refreshed: %v", at)
		}
	})
}

// CLM-5: a friends or library failure leaves the stamp unset and the user due;
// a game details failure alone still succeeds.
func TestLibrarySyncStampOnlyOnSuccess(t *testing.T) {
	cases := map[string]func(*loginSteam){
		"friends": func(s *loginSteam) { s.games = lsLibrary; s.friendsErr = errors.New("friends unavailable") },
		"games": func(s *loginSteam) {
			s.games = func(context.Context, string) ([]steamapi.Game, error) { return nil, errors.New("games unavailable") }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := lsNewFixture(t)
			setup(f.steam)
			if body, _ := f.sync(t); !strings.Contains(body, "Library sync failed") {
				t.Fatalf("body=%s", body)
			}
			if at, _ := f.syncState(t); at != nil {
				t.Fatalf("stamp set after %s failure: %v", name, at)
			}
			// /parties lists invite candidates from Friends; let it render.
			f.steam.friendsErr = nil
			f.assertPagesShowSync(t, true)
		})
	}
	t.Run("details", func(t *testing.T) {
		f := lsNewFixture(t)
		f.steam.games = lsLibrary
		f.steam.gamesDetails = func([]int) ([]steamapi.GameDetail, error) { return nil, errors.New("details unavailable") }
		if body, _ := f.sync(t); strings.Contains(body, "Library sync failed") {
			t.Fatalf("details failure failed the sync: %s", body)
		}
		if at, _ := f.syncState(t); at == nil {
			t.Fatal("stamp not set after a details-only failure")
		}
	})
}

// lsWaitAdvisoryWaiter polls until a session waits on an advisory lock.
func lsWaitAdvisoryWaiter(t *testing.T, f *lsFixture) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no request waited on the user's sync lock")
}

// CLM-6 (route half): two instances on separate pools syncing the same user
// run one sync; the other waits and reuses its result.
func TestLibrarySyncTwoInstancesOneSync(t *testing.T) {
	f := lsNewFixture(t)
	app2 := lsApp(db.New(lsSecondPool(t, f)), f.steam)
	var current, maxConcurrent, total atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.steam.games = func(ctx context.Context, id string) ([]steamapi.Game, error) {
		n := current.Add(1)
		defer current.Add(-1)
		total.Add(1)
		for {
			m := maxConcurrent.Load()
			if n <= m || maxConcurrent.CompareAndSwap(m, n) {
				break
			}
		}
		once.Do(func() { close(entered) })
		<-release
		return lsLibrary(ctx, id)
	}
	bodies := make([]string, 2)
	var wg sync.WaitGroup
	run := func(i int, app *application) {
		defer wg.Done()
		bodies[i] = lsServe(app, f.ctx, "/library/sync", f.cookie).Body.String()
	}
	captureStdout(t, func() {
		wg.Add(1)
		go run(0, f.app)
		<-entered
		wg.Add(1)
		go run(1, app2)
		lsWaitAdvisoryWaiter(t, f)
		close(release)
		wg.Wait()
	})
	if maxConcurrent.Load() != 1 || total.Load() != 1 {
		t.Fatalf("games calls max concurrent=%d total=%d", maxConcurrent.Load(), total.Load())
	}
	for i, body := range bodies {
		if strings.Contains(body, "Library sync failed") || strings.Contains(body, "loading-spinner") {
			t.Fatalf("request %d body=%s", i, body)
		}
	}
}

// CLM-7: a request arriving while another sync holds the lock waits for it,
// makes no Steam call, and returns that sync's success.
func TestLibrarySyncWaitsOnRunningSync(t *testing.T) {
	f := lsNewFixture(t)
	f.steam.games = lsLibrary
	locked := make(chan struct{})
	var releasedAt atomic.Int64
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- f.store.WithUserSyncLock(f.ctx, f.userID, func(ctx context.Context) error {
			close(locked)
			time.Sleep(500 * time.Millisecond)
			if err := f.store.RecordSyncSuccess(ctx, f.userID); err != nil {
				return err
			}
			releasedAt.Store(time.Now().UnixNano())
			return nil
		})
	}()
	<-locked
	body, calls := f.sync(t)
	returned := time.Now().UnixNano()
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("waiting request made %d Steam calls", calls)
	}
	if releasedAt.Load() == 0 || returned < releasedAt.Load() {
		t.Fatal("request returned before the running sync released its lock")
	}
	if strings.Contains(body, "Library sync failed") || strings.Contains(body, "loading-spinner") || !strings.Contains(body, `id="library-sync"`) {
		t.Fatalf("body=%s, want the success fragment", body)
	}
}

// CLM-8: a lock left by a connection that died is freed with it.
func TestLibrarySyncLockFreedByDeadConnection(t *testing.T) {
	f := lsNewFixture(t)
	f.steam.games = lsLibrary
	conn, err := pgx.ConnectConfig(f.ctx, f.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(f.ctx, `SELECT pg_advisory_lock($1, $2)`, lsLockNamespace, lsLockKey(f.userID)); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND classid = $1::int::oid`, lsLockNamespace).Scan(&held); err != nil || held != 1 {
		t.Fatalf("raw connection lock held=%d err=%v", held, err)
	}
	if err := conn.Close(f.ctx); err != nil {
		t.Fatal(err)
	}
	app2 := lsApp(db.New(lsSecondPool(t, f)), f.steam)
	app2.librarySyncTimeout = 10 * time.Second
	var w *httptest.ResponseRecorder
	captureStdout(t, func() { w = lsServe(app2, f.ctx, "/library/sync", f.cookie) })
	if strings.Contains(w.Body.String(), "Library sync failed") {
		t.Fatalf("body=%s", w.Body.String())
	}
	if at, _ := f.syncState(t); at == nil {
		t.Fatal("sync after a dead lock holder recorded no success")
	}
}

// CLM-10: a failure is stored and shown with its reason (the Steam error or the
// timeout) instead of the spinner; a later success clears it.
func TestLibrarySyncFailureShownAndCleared(t *testing.T) {
	f := lsNewFixture(t)
	f.steam.games = func(context.Context, string) ([]steamapi.Game, error) {
		return nil, errors.New("steam owned games 503")
	}
	body, _ := f.sync(t)
	if !strings.Contains(body, "Library sync failed") || !strings.Contains(body, "steam owned games 503") || strings.Contains(body, "loading-spinner") {
		t.Fatalf("failure body=%s", body)
	}
	if _, reason := f.syncState(t); reason == nil || !strings.Contains(*reason, "steam owned games 503") {
		t.Fatalf("stored reason=%v", reason)
	}

	f.app.librarySyncTimeout = 200 * time.Millisecond
	f.steam.games = func(ctx context.Context, _ string) ([]steamapi.Game, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	body, _ = f.sync(t)
	if !strings.Contains(body, "Library sync failed") || !strings.Contains(body, librarySyncTimeoutReason) {
		t.Fatalf("timeout body=%s", body)
	}
	if _, reason := f.syncState(t); reason == nil || *reason != librarySyncTimeoutReason {
		t.Fatalf("stored timeout reason=%v", reason)
	}

	f.app.librarySyncTimeout = 0
	f.steam.games = lsLibrary
	if body, _ = f.sync(t); strings.Contains(body, "Library sync failed") {
		t.Fatalf("success body=%s", body)
	}
	if at, reason := f.syncState(t); at == nil || reason != nil {
		t.Fatalf("after success stamp=%v reason=%v", at, reason)
	}
}

// lsSecretTransportErr is what the Steam client returns when a request fails
// in transport: the request URL, API key included, is part of the error text.
func lsSecretTransportErr() error {
	return fmt.Errorf("steamapi: performing request: %w", &url.Error{
		Op:  "Get",
		URL: "https://api.steampowered.com/ISteamUser/GetFriendList/v1/?key=SECRETKEY123&steamid=1",
		Err: errors.New("dial tcp: connection refused"),
	})
}

// A transport error carrying the Steam API key is never stored or rendered
// with the key, for a failing library fetch or friends refresh.
func TestLibrarySyncFailureRedactsSecret(t *testing.T) {
	cases := map[string]func(*loginSteam){
		"games": func(s *loginSteam) {
			s.games = func(context.Context, string) ([]steamapi.Game, error) { return nil, lsSecretTransportErr() }
		},
		"friends": func(s *loginSteam) { s.games = lsLibrary; s.friendsErr = lsSecretTransportErr() },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := lsNewFixture(t)
			setup(f.steam)
			body, _ := f.sync(t)
			if !strings.Contains(body, "Library sync failed") || !strings.Contains(body, "connection refused") {
				t.Fatalf("failure body=%s", body)
			}
			if strings.Contains(body, "SECRETKEY123") {
				t.Fatalf("response leaks the key: %s", body)
			}
			_, reason := f.syncState(t)
			if reason == nil || !strings.Contains(*reason, "connection refused") || strings.Contains(*reason, "SECRETKEY123") {
				t.Fatalf("stored reason=%v", reason)
			}
		})
	}
}

// An old stored reason that still carries the key is scrubbed when rendered by
// the sync route, and scrubbed again when stored.
func TestLibrarySyncStoredSecretScrubbed(t *testing.T) {
	f := lsNewFixture(t)
	if err := f.store.RecordSyncFailure(f.ctx, f.userID, "boom https://x.test/a?key=SECRETKEY123&b=1"); err != nil {
		t.Fatal(err)
	}
	if _, reason := f.syncState(t); reason == nil || strings.Contains(*reason, "SECRETKEY123") {
		t.Fatalf("stored reason=%v", reason)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET library_sync_error = 'old ?key=SECRETKEY123' WHERE id = $1`, f.userID); err != nil {
		t.Fatal(err)
	}
	f.steam.games = func(context.Context, string) ([]steamapi.Game, error) { return nil, errors.New("again") }
	if body, _ := f.sync(t); strings.Contains(body, "SECRETKEY123") {
		t.Fatalf("body=%s", body)
	}
	var sb strings.Builder
	if err := component.LibrarySyncStatus(true, "old ?key=SECRETKEY123").Render(f.ctx, &sb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sb.String(), "SECRETKEY123") || !strings.Contains(sb.String(), "Library sync failed") {
		t.Fatalf("rendered=%s", sb.String())
	}
}

// CLM-14: a signed cookie for a real session is served as the user on each
// page and the sync route; a missing, tampered or deleted session is not.
func TestLibrarySyncSessionResolution(t *testing.T) {
	f := lsNewFixture(t)
	f.steam.games = lsLibrary
	f.assertPagesShowSync(t, true)

	callsBefore := f.steam.syncCalls() // signed-in /parties lists friends
	tampered := *f.cookie
	last := tampered.Value[len(tampered.Value)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	tampered.Value = tampered.Value[:len(tampered.Value)-1] + string(flip)
	assertSignedOut := func(name string, cookie *http.Cookie) {
		t.Helper()
		for _, path := range append(f.pages(), "/library/sync") {
			var w *httptest.ResponseRecorder
			captureStdout(t, func() { w = lsServe(f.app, f.ctx, path, cookie) })
			body := w.Body.String()
			if strings.Contains(body, "Login user") || strings.Contains(body, lsSyncElement) || strings.Contains(body, `id="library-sync"`) {
				t.Fatalf("%s cookie served %s as the user: status=%d body=%s", name, path, w.Code, body)
			}
			if path != "/" && w.Code == http.StatusOK {
				t.Fatalf("%s cookie got 200 on %s", name, path)
			}
		}
	}
	assertSignedOut("missing", nil)
	assertSignedOut("tampered", &tampered)
	if calls := f.steam.syncCalls() - callsBefore; calls != 0 {
		t.Fatalf("signed-out sync route made %d Steam calls", calls)
	}

	w := f.get("/library/sync")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="library-sync"`) {
		t.Fatalf("valid sync status=%d body=%s", w.Code, w.Body.String())
	}
	if err := f.store.DeleteSession(f.ctx, f.token); err != nil {
		t.Fatal(err)
	}
	assertSignedOut("deleted", f.cookie)
}
