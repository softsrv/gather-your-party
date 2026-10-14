//go:build integration

package db

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
	"reflect"
	"sync"
	"testing"
	"time"

	"gather-your-party/internal/middleware"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// This test requires an empty, disposable database. No application data is used.
func TestPersistence(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name an empty, disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("cannot connect to test database")
	}
	applyTestMigrations(t, ctx, pool, allTestMigrations...)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, "DROP TABLE user_games, games, invites, memberships, parties, sessions, users"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	store := New(pool)
	const verifiedID = "76561198000000001"
	profile := steamapi.Player{
		SteamID: "76561198000000002", PersonaName: "Alyx",
		AvatarSmall: "https://example.com/small", AvatarMedium: "https://example.com/medium", AvatarFull: "https://example.com/full",
	}
	id, err := store.UpsertUser(ctx, verifiedID, profile)
	if err != nil {
		t.Fatal(err)
	}
	var gotID, persona, small, medium, full string
	var created, updated time.Time
	readUser := func() {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full, created_at, updated_at FROM users WHERE id = $1`, id).
			Scan(&gotID, &persona, &small, &medium, &full, &created, &updated)
		if err != nil {
			t.Fatal(err)
		}
		if gotID != verifiedID || persona != profile.PersonaName || small != profile.AvatarSmall || medium != profile.AvatarMedium || full != profile.AvatarFull {
			t.Fatalf("wrong persisted profile: %q %q %q %q %q", gotID, persona, small, medium, full)
		}
	}
	readUser()
	if id <= 0 || created.IsZero() || updated.IsZero() {
		t.Fatal("missing generated id or timestamps")
	}
	originalCreated := created
	if _, err := pool.Exec(ctx, `UPDATE users SET updated_at = '2000-01-01' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	profile.PersonaName = "Barney"
	profile.AvatarSmall += "-new"
	profile.AvatarMedium += "-new"
	profile.AvatarFull += "-new"
	updatedID, err := store.UpsertUser(ctx, verifiedID, profile)
	if err != nil || updatedID != id {
		t.Fatalf("upsert returned id %d, error %v", updatedID, err)
	}
	readUser()
	if !created.Equal(originalCreated) || updated.Before(originalCreated) {
		t.Fatal("upsert did not preserve creation time and refresh update time")
	}
	// Pages read the profile refreshed at sign-in rather than calling Steam.
	stored, found, err := store.UserProfile(ctx, verifiedID)
	if err != nil || !found || stored.SteamID != verifiedID || stored.PersonaName != "Barney" ||
		stored.AvatarSmall != profile.AvatarSmall || stored.AvatarMedium != profile.AvatarMedium || stored.AvatarFull != profile.AvatarFull {
		t.Fatalf("stored profile=%+v found=%v err=%v", stored, found, err)
	}
	if _, found, err := store.UserProfile(ctx, "76561198999999999"); err != nil || found {
		t.Fatalf("unknown profile found=%v err=%v", found, err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one user, got %d, error %v", count, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full) VALUES ($1, '', '', '', '')`, verifiedID)
	assertSQLState(t, err, "23505")

	token, err := store.CreateSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("session token is not a 256-bit hex string")
	}
	secondToken, err := store.CreateSession(ctx, id)
	if err != nil || token == secondToken {
		t.Fatalf("second session did not get a distinct token: %v", err)
	}
	var owner int64
	var expires, sessionCreated time.Time
	if err := pool.QueryRow(ctx, `SELECT users.id, sessions.expires_at, sessions.created_at FROM sessions JOIN users ON sessions.user_id = users.id WHERE token = $1`, token).Scan(&owner, &expires, &sessionCreated); err != nil {
		t.Fatal(err)
	}
	if owner != id || sessionCreated.IsZero() || expires.Sub(sessionCreated) != 7*24*time.Hour {
		t.Fatal("session has wrong owner, creation time, or expiry")
	}
	_, err = pool.Exec(ctx, `INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, now())`, token, id)
	assertSQLState(t, err, "23505")
	invalidToken, err := store.CreateSession(ctx, -1)
	assertSQLState(t, err, "23503")
	if invalidToken != "" {
		t.Fatal("failed insert returned a usable token")
	}
	// Measure against database time so host clock skew cannot hide a bad lifetime.
	assertSevenDays := func() time.Time {
		t.Helper()
		var expiry, now time.Time
		if err := pool.QueryRow(ctx, `SELECT expires_at, now() FROM sessions WHERE token = $1`, token).Scan(&expiry, &now); err != nil {
			t.Fatal(err)
		}
		if remaining := expiry.Sub(now); remaining < 7*24*time.Hour-time.Minute || remaining > 7*24*time.Hour+time.Minute {
			t.Fatalf("session lifetime = %v, want approximately seven days", remaining)
		}
		return expiry
	}
	assertSevenDays()
	if identity, ok, err := store.ResolveSession(ctx, token); err != nil || !ok || identity != verifiedID {
		t.Fatalf("resolve = %q, %t, %v", identity, ok, err)
	}
	assertSevenDays()

	// Exercise the real middleware with the real store, including context identity.
	secret := []byte("integration-test-secret")
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(token))
	cookie := &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	auth := &middleware.Authenticator{Resolver: store, Secret: secret}
	resolveRequest := func(wantIdentity bool) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		requestContext := &middleware.CustomContext{Context: ctx, StartTime: time.Now()}
		if err := auth.LoadSteamId(requestContext, w, r); err != nil {
			t.Fatal(err)
		}
		if wantIdentity {
			if requestContext.Value(middleware.SteamID{}) != verifiedID {
				t.Fatal("verified database identity did not reach request context")
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Value != cookie.Value || cookies[0].MaxAge != 7*24*3600 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
				t.Fatal("authenticated request did not refresh the secure browser cookie")
			}
		} else if requestContext.Value(middleware.SteamID{}) != nil || len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid session reached context or refreshed its cookie")
		}
	}
	var oldExpiry time.Time
	if err := pool.QueryRow(ctx, `UPDATE sessions SET expires_at = now() + interval '1 hour' WHERE token = $1 RETURNING expires_at`, token).Scan(&oldExpiry); err != nil {
		t.Fatal(err)
	}
	resolveRequest(true)
	if refreshed := assertSevenDays(); !refreshed.After(oldExpiry) {
		t.Fatal("near-expiry session did not slide forward")
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token = $1`, token); err != nil {
		t.Fatal(err)
	}
	if identity, ok, err := store.ResolveSession(ctx, token); err != nil || ok || identity != "" {
		t.Fatalf("expired session resolved: %q, %t, %v", identity, ok, err)
	}
	resolveRequest(false)
	var stillExpired bool
	if err := pool.QueryRow(ctx, `SELECT expires_at <= now() FROM sessions WHERE token = $1`, token).Scan(&stillExpired); err != nil || !stillExpired {
		t.Fatal("expired session was revived")
	}
	if err := store.DeleteSession(ctx, secondToken); err != nil {
		t.Fatal(err)
	}
	if identity, ok, err := store.ResolveSession(ctx, secondToken); err != nil || ok || identity != "" {
		t.Fatalf("deleted session resolved: %q, %t, %v", identity, ok, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token = $1`, secondToken).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted session count = %d, error %v", count, err)
	}
	if err := store.DeleteSession(ctx, secondToken); err != nil {
		t.Fatalf("repeated delete must be idempotent: %v", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if failedID, err := store.UpsertUser(cancelled, verifiedID, profile); err == nil || failedID != 0 {
		t.Fatal("cancelled upsert did not return an error and zero id")
	}
	if identity, ok, err := store.ResolveSession(cancelled, token); err == nil || ok || identity != "" {
		t.Fatal("cancelled resolution did not return an error without identity")
	}
	if err := store.DeleteSession(cancelled, token); err == nil {
		t.Fatal("cancelled deletion did not return an error")
	}
}

func assertSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %v", code, err)
	}
}

func gameTestUser(t *testing.T, store *Store, ctx context.Context, steamID string) int64 {
	t.Helper()
	id, err := store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: "Gamer " + steamID})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func steamGames(appIDs ...int) []steamapi.Game {
	games := make([]steamapi.Game, len(appIDs))
	for i, appID := range appIDs {
		games[i] = steamapi.Game{AppID: appID, Name: fmt.Sprintf("Game %d", appID), ImgIconURL: fmt.Sprintf("icon%d", appID)}
	}
	return games
}

func everLoggedIn(t *testing.T, pool *pgxpool.Pool, ctx context.Context, steamID string) bool {
	t.Helper()
	var flag bool
	if err := pool.QueryRow(ctx, `SELECT ever_logged_in FROM users WHERE steam_id_64 = $1`, steamID).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	return flag
}

func storedAppIDs(games []StoredGame) []int {
	ids := make([]int, len(games))
	for i, game := range games {
		ids[i] = game.AppID
	}
	return ids
}

// Games CLM-15: existing rows become true; sign-in sets true; friend refresh never clears it.
func TestEverLoggedInMigrationAndUpserts(t *testing.T) {
	pool, ctx := scopedTestPool(t)
	applyTestMigrations(t, ctx, pool, allTestMigrations[:3]...)
	const existing, signIn, friendOfLoggedIn, newFriend = "76561198000010001", "76561198000010002", "76561198000010003", "76561198000010004"
	if _, err := pool.Exec(ctx, `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full) VALUES ($1, 'Old', '', '', '')`, existing); err != nil {
		t.Fatal(err)
	}
	applyTestMigrations(t, ctx, pool, "0004_games.sql")
	store := New(pool)
	if !everLoggedIn(t, pool, ctx, existing) {
		t.Fatal("row that existed before the migration must read ever_logged_in = true")
	}

	// A friend-created row signs in later: UpsertUser flips it to true.
	if _, err := store.UpsertFriend(ctx, signIn, steamapi.Player{PersonaName: "Friend"}); err != nil {
		t.Fatal(err)
	}
	if everLoggedIn(t, pool, ctx, signIn) {
		t.Fatal("friend-created row must start false")
	}
	if _, err := store.UpsertUser(ctx, signIn, steamapi.Player{PersonaName: "Signed in"}); err != nil {
		t.Fatal(err)
	}
	if !everLoggedIn(t, pool, ctx, signIn) {
		t.Fatal("UpsertUser on conflict must set ever_logged_in = true")
	}

	// A logged-in user refreshed as someone's friend stays true and gets the new profile.
	loggedID := gameTestUser(t, store, ctx, friendOfLoggedIn)
	if !everLoggedIn(t, pool, ctx, friendOfLoggedIn) {
		t.Fatal("UpsertUser insert must set ever_logged_in = true")
	}
	friendID, err := store.UpsertFriend(ctx, friendOfLoggedIn, steamapi.Player{PersonaName: "Refreshed", AvatarMedium: "m"})
	if err != nil || friendID != loggedID {
		t.Fatalf("friend refresh id=%d want %d err=%v", friendID, loggedID, err)
	}
	if !everLoggedIn(t, pool, ctx, friendOfLoggedIn) {
		t.Fatal("UpsertFriend must never change ever_logged_in from true to false")
	}
	if profile, _, err := store.UserProfile(ctx, friendOfLoggedIn); err != nil || profile.PersonaName != "Refreshed" || profile.AvatarMedium != "m" {
		t.Fatalf("friend refresh did not update profile: %+v %v", profile, err)
	}

	newID, err := store.UpsertFriend(ctx, newFriend, steamapi.Player{PersonaName: "New"})
	if err != nil || newID <= 0 {
		t.Fatalf("new friend id=%d err=%v", newID, err)
	}
	if everLoggedIn(t, pool, ctx, newFriend) {
		t.Fatal("UpsertFriend on a new id must create ever_logged_in = false")
	}
}

// Games CLM-22: never_synced by default; a non-empty sync sets synced with a
// timestamp, including from private_or_no_games.
func TestGameSyncStateTransitions(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	const fresh, private = "76561198000011001", "76561198000011002"
	freshID := gameTestUser(t, store, ctx, fresh)
	privateID := gameTestUser(t, store, ctx, private)

	states, err := store.GameSyncStates(ctx, []string{fresh, private, "76561198999999999"})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("unknown ids must be absent: %+v", states)
	}
	if got := states[fresh]; got.State != GameSyncNeverSynced || got.At != nil || got.UserID != freshID || got.SteamID64 != fresh {
		t.Fatalf("fresh state=%+v", got)
	}

	if err := store.StoreUserLibrary(ctx, freshID, steamGames(10, 20)); err != nil {
		t.Fatal(err)
	}
	states, err = store.GameSyncStates(ctx, []string{fresh})
	if err != nil {
		t.Fatal(err)
	}
	if got := states[fresh]; got.State != GameSyncSynced || got.At == nil || got.At.IsZero() {
		t.Fatalf("after sync state=%+v", got)
	}

	if state, err := store.RecordEmptyLibrary(ctx, privateID); err != nil || state != GameSyncPrivateOrNoGames {
		t.Fatalf("empty library state=%q err=%v", state, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET game_sync_state_at = '2000-01-01' WHERE id = $1`, privateID); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreUserLibrary(ctx, privateID, steamGames(30)); err != nil {
		t.Fatal(err)
	}
	states, err = store.GameSyncStates(ctx, []string{private})
	if err != nil {
		t.Fatal(err)
	}
	if got := states[private]; got.State != GameSyncSynced || got.At == nil || got.At.Year() == 2000 {
		t.Fatalf("private user after non-empty sync state=%+v", got)
	}
}

// Games CLM-23: an empty result marks only a gameless user private and never
// deletes stored games.
func TestRecordEmptyLibrary(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	const empty, stocked = "76561198000012001", "76561198000012002"
	emptyID := gameTestUser(t, store, ctx, empty)
	stockedID := gameTestUser(t, store, ctx, stocked)

	state, err := store.RecordEmptyLibrary(ctx, emptyID)
	if err != nil || state != GameSyncPrivateOrNoGames {
		t.Fatalf("gameless state=%q err=%v", state, err)
	}
	states, err := store.GameSyncStates(ctx, []string{empty})
	if err != nil || states[empty].State != GameSyncPrivateOrNoGames || states[empty].At == nil {
		t.Fatalf("stored gameless state=%+v err=%v", states[empty], err)
	}

	if err := store.StoreUserLibrary(ctx, stockedID, steamGames(1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	state, err = store.RecordEmptyLibrary(ctx, stockedID)
	if err != nil || state != GameSyncSynced {
		t.Fatalf("stocked state=%q err=%v", state, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_games WHERE user_id = $1`, stockedID).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("stocked user_games=%d err=%v", rows, err)
	}
	states, err = store.GameSyncStates(ctx, []string{stocked})
	if err != nil || states[stocked].State != GameSyncSynced {
		t.Fatalf("stored stocked state=%+v err=%v", states[stocked], err)
	}
}

// Games CLM-3: concurrent overlapping syncs for one user both succeed and leave
// one games row per app id and one user_games row per pair.
func TestStoreUserLibraryConcurrentOverlap(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	for iteration := 0; iteration < 10; iteration++ {
		if _, err := pool.Exec(ctx, `TRUNCATE user_games, games`); err != nil {
			t.Fatal(err)
		}
		userID := gameTestUser(t, store, ctx, fmt.Sprintf("765611980000130%02d", iteration))
		// Overlapping libraries in opposite orders, so unsorted writes would lock
		// shared rows in conflicting orders.
		first := make([]int, 0, 60)
		second := make([]int, 0, 60)
		for app := 1; app <= 60; app++ {
			first = append(first, 1000+app)
			second = append(second, 1090-app)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, library := range [][]int{first, second} {
			wg.Add(1)
			go func(i int, library []int) {
				defer wg.Done()
				<-start
				errs[i] = store.StoreUserLibrary(ctx, userID, steamGames(library...))
			}(i, library)
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("iteration %d: errors %v / %v", iteration, errs[0], errs[1])
		}
		distinct := make(map[int]bool)
		for _, app := range append(append([]int{}, first...), second...) {
			distinct[app] = true
		}
		var games, distinctGames, pairs, distinctPairs int
		if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT app_id) FROM games`).Scan(&games, &distinctGames); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT (user_id, game_id)) FROM user_games WHERE user_id = $1`, userID).Scan(&pairs, &distinctPairs); err != nil {
			t.Fatal(err)
		}
		if games != len(distinct) || distinctGames != len(distinct) || pairs != len(distinct) || distinctPairs != len(distinct) {
			t.Fatalf("iteration %d: games=%d/%d pairs=%d/%d want %d", iteration, games, distinctGames, pairs, distinctPairs, len(distinct))
		}
	}
}

// Library upserts keep details, dedupe app ids (first position wins) and order by position.
func TestStoreUserLibraryUpsertAndOrder(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	const caller = "76561198000014001"
	callerID := gameTestUser(t, store, ctx, caller)
	if err := store.StoreUserLibrary(ctx, callerID, steamGames(30, 10, 20, 10)); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreGameData(ctx, []GameData{{AppID: 10, HasData: true, Multiplayer: true}}); err != nil {
		t.Fatal(err)
	}
	renamed := steamGames(20, 10, 30)
	renamed[1].Name = "Renamed"
	renamed[1].ImgIconURL = "new-icon"
	if err := store.StoreUserLibrary(ctx, callerID, renamed); err != nil {
		t.Fatal(err)
	}
	got, err := store.UserGames(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	if ids := storedAppIDs(got); !reflect.DeepEqual(ids, []int{20, 10, 30}) {
		t.Fatalf("order=%v", ids)
	}
	if got[1] != (StoredGame{AppID: 10, Name: "Renamed", ImgIconURL: "new-icon", DataFetched: true, HasData: true, Multiplayer: true}) {
		t.Fatalf("upsert must refresh name/icon and keep details: %+v", got[1])
	}
	var games int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM games`).Scan(&games); err != nil || games != 3 {
		t.Fatalf("games=%d err=%v", games, err)
	}
	// Equal positions fall back to app id.
	if _, err := pool.Exec(ctx, `UPDATE user_games SET position = 0 WHERE user_id = $1`, callerID); err != nil {
		t.Fatal(err)
	}
	if got, err := store.UserGames(ctx, caller); err != nil || !reflect.DeepEqual(storedAppIDs(got), []int{10, 20, 30}) {
		t.Fatalf("tie order=%v err=%v", storedAppIDs(got), err)
	}
	if got, err := store.UserGames(ctx, "76561198999999999"); err != nil || len(got) != 0 {
		t.Fatalf("unknown user games=%+v err=%v", got, err)
	}
}

func TestGamesMissingDataAndStoreGameData(t *testing.T) {
	store, _, ctx := partyTestStore(t)
	userID := gameTestUser(t, store, ctx, "76561198000015001")
	if err := store.StoreUserLibrary(ctx, userID, steamGames(1, 2, 3, 4)); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreGameData(ctx, []GameData{{AppID: 2, HasData: true}, {AppID: 4, HasData: false}, {AppID: 99, HasData: true}}); err != nil {
		t.Fatal(err)
	}
	missing, err := store.GamesMissingData(ctx, []int{3, 99, 2, 1, 4, 3})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(missing, []int{3, 1}) {
		t.Fatalf("missing=%v want [3 1]", missing)
	}
	if missing, err := store.GamesMissingData(ctx, nil); err != nil || len(missing) != 0 {
		t.Fatalf("empty input missing=%v err=%v", missing, err)
	}
	got, err := store.UserGames(ctx, "76561198000015001")
	if err != nil || len(got) != 4 {
		t.Fatalf("games=%+v err=%v", got, err)
	}
	if !got[1].DataFetched || !got[1].HasData || got[1].Multiplayer || !got[3].DataFetched || got[3].HasData || got[0].DataFetched {
		t.Fatalf("stored details=%+v", got)
	}
}

func TestSharedStoredGames(t *testing.T) {
	store, _, ctx := partyTestStore(t)
	const caller, alice, bob, stranger = "76561198000016001", "76561198000016002", "76561198000016003", "76561198000016004"
	ids := map[string]int64{}
	for _, steamID := range []string{caller, alice, bob, stranger} {
		ids[steamID] = gameTestUser(t, store, ctx, steamID)
	}
	if err := store.StoreUserLibrary(ctx, ids[caller], steamGames(50, 40, 30, 20, 10)); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreUserLibrary(ctx, ids[alice], steamGames(10, 30, 40, 60)); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreUserLibrary(ctx, ids[bob], steamGames(10, 40, 50)); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		others []string
		want   []int
	}{
		{nil, []int{50, 40, 30, 20, 10}},
		{[]string{alice}, []int{40, 30, 10}},
		{[]string{alice, bob}, []int{40, 10}},
		{[]string{alice, bob, alice}, []int{40, 10}},
		{[]string{alice, stranger}, []int{}},
		{[]string{"76561198999999999"}, []int{}},
	}
	for _, c := range cases {
		got, err := store.SharedStoredGames(ctx, caller, c.others)
		if err != nil {
			t.Fatal(err)
		}
		if ids := storedAppIDs(got); !reflect.DeepEqual(ids, c.want) {
			t.Errorf("others=%v shared=%v want %v", c.others, ids, c.want)
		}
	}
}
