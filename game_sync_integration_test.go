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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// gameSyncSteam is a view.SteamService double at the Steam boundary; the store,
// router, session and templates are real. Every call is recorded.
type gameSyncSteam struct {
	t            *testing.T
	mu           sync.Mutex
	friends      []string
	libraries    map[string][]steamapi.Game
	gamesErr     map[string]error
	details      func(context.Context, []int) ([]steamapi.GameDetail, error)
	gamesCalls   []string
	detailsCalls [][]int
	countCalls   []string
}

func (s *gameSyncSteam) Games(_ context.Context, steamID string, appInfo, freeGames bool) ([]steamapi.Game, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !appInfo || !freeGames {
		s.t.Error("owned-games options missing")
	}
	s.gamesCalls = append(s.gamesCalls, steamID)
	if err := s.gamesErr[steamID]; err != nil {
		return nil, err
	}
	return s.libraries[steamID], nil
}

func (s *gameSyncSteam) GamesDetails(ctx context.Context, appIDs []int) ([]steamapi.GameDetail, error) {
	s.mu.Lock()
	s.detailsCalls = append(s.detailsCalls, slices.Clone(appIDs))
	details := s.details
	s.mu.Unlock()
	if len(appIDs) > 10 {
		s.t.Errorf("details batch of %d exceeds 10", len(appIDs))
	}
	if details == nil {
		s.t.Errorf("unexpected GamesDetails call for %v", appIDs)
		return nil, errors.New("unexpected GamesDetails call")
	}
	return details(ctx, appIDs)
}

func (s *gameSyncSteam) Friends(context.Context, string) ([]steamapi.Friend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	friends := make([]steamapi.Friend, len(s.friends))
	for i, id := range s.friends {
		friends[i] = steamapi.Friend{SteamID: id, Relationship: "friend"}
	}
	return friends, nil
}

func (s *gameSyncSteam) Players(_ context.Context, ids []string) ([]steamapi.Player, error) {
	players := make([]steamapi.Player, len(ids))
	for i, id := range ids {
		players[i] = steamapi.Player{SteamID: id, PersonaName: "Persona " + id[len(id)-3:]}
	}
	return players, nil
}

func (s *gameSyncSteam) GetNumberOfCurrentPlayers(_ context.Context, appID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.countCalls = append(s.countCalls, appID)
	id, _ := strconv.Atoi(appID)
	return id * 10, nil
}

func (s *gameSyncSteam) requested() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int
	for _, call := range s.detailsCalls {
		ids = append(ids, call...)
	}
	return ids
}

func (s *gameSyncSteam) gamesCalled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.gamesCalls, id)
}

// multiplayerDetails classifies the listed app ids as multiplayer and every
// other requested id as single-player.
func multiplayerDetails(multiplayer ...int) func(context.Context, []int) ([]steamapi.GameDetail, error) {
	return func(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
		details := make([]steamapi.GameDetail, len(ids))
		for i, id := range ids {
			details[i] = steamapi.GameDetail{AppID: id, Multiplayer: slices.Contains(multiplayer, id)}
		}
		return details, nil
	}
}

func failDetails(t *testing.T) func(context.Context, []int) ([]steamapi.GameDetail, error) {
	return func(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
		t.Fatalf("GamesDetails called for %v although every game's data is stored", ids)
		return nil, nil
	}
}

type syncFixture struct {
	t     *testing.T
	store *db.Store
	pool  *pgxpool.Pool
	ctx   context.Context
	steam *gameSyncSteam
	app   application
}

const (
	callerID = "76561198000000501"
	friendA  = "76561198000000502"
	friendB  = "76561198000000503"
	stranger = "76561198000000599"
)

func newSyncFixture(t *testing.T) *syncFixture {
	store, pool, ctx := inviteIntegrationStore(t)
	steam := &gameSyncSteam{t: t, libraries: map[string][]steamapi.Game{}, gamesErr: map[string]error{}}
	f := &syncFixture{t: t, store: store, pool: pool, ctx: ctx, steam: steam}
	f.app = application{store: store, config: appConfig{SessionSecret: "game-sync-test-secret"}, steam: func() view.SteamService { return steam }}
	return f
}

func (f *syncFixture) user(steamID string) int64 {
	id, err := f.store.UpsertUser(f.ctx, steamID, steamapi.Player{PersonaName: "Persona " + steamID[len(steamID)-3:]})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// seed stores a user's library and marks the listed games' data stored.
func (f *syncFixture) seed(steamID string, games []steamapi.Game) int64 {
	id := f.user(steamID)
	if err := f.store.StoreUserLibrary(f.ctx, id, games); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *syncFixture) storeData(data ...db.GameData) {
	if err := f.store.StoreGameData(f.ctx, data); err != nil {
		f.t.Fatal(err)
	}
}

func (f *syncFixture) cookie(userID int64) *http.Cookie {
	token, err := f.store.CreateSession(f.ctx, userID)
	if err != nil {
		f.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(f.app.config.SessionSecret))
	mac.Write([]byte(token))
	return &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
}

// sharedGames POSTs Get shared games as the caller with the given friendIDs.
func (f *syncFixture) sharedGames(userID int64, friendIDs ...string) string {
	form := make([]string, len(friendIDs))
	for i, id := range friendIDs {
		form[i] = "friendID=" + id
	}
	r := httptest.NewRequest(http.MethodPost, "/frag/shared-games", strings.NewReader(strings.Join(form, "&"))).WithContext(f.ctx)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(f.cookie(userID))
	w := httptest.NewRecorder()
	f.app.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		f.t.Fatalf("shared games status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// partyPage renders the party page for a party of the given members (first is
// leader and viewer).
func (f *syncFixture) partyPage(users ...int64) string {
	party, err := f.store.CreateParty(f.ctx, users[0], "Sync party")
	if err != nil {
		f.t.Fatal(err)
	}
	for _, u := range users[1:] {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2)`, party, u); err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/parties/"+party, nil).WithContext(f.ctx)
	r.AddCookie(f.cookie(users[0]))
	w := httptest.NewRecorder()
	f.app.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		f.t.Fatalf("party page status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

var (
	sharedGameRE = regexp.MustCompile(`(?s)<li data-name="[^"]*"[^>]*>(.*?)</li>`)
	partyGameRE  = regexp.MustCompile(`(?s)<li data-appid="([0-9]+)"[^>]*>(.*?)</li>`)
	gameTitleRE  = regexp.MustCompile(`title="([^"]*)"`)
	gameIconRE   = regexp.MustCompile(`/images/apps/([0-9]+)/([^"]+)\.jpg`)
)

type renderedGame struct {
	AppID int
	Name  string
	Icon  string
}

// sharedRendered parses Get shared games: name and icon (with app id).
func sharedRendered(t *testing.T, body string) []renderedGame {
	t.Helper()
	var games []renderedGame
	for _, m := range sharedGameRE.FindAllStringSubmatch(body, -1) {
		g := renderedGame{}
		if title := gameTitleRE.FindStringSubmatch(m[1]); title != nil {
			g.Name = title[1]
		}
		if icon := gameIconRE.FindStringSubmatch(m[1]); icon != nil {
			g.AppID, _ = strconv.Atoi(icon[1])
			g.Icon = icon[2]
		}
		games = append(games, g)
	}
	return games
}

// partyRendered parses the party page: app id and name.
func partyRendered(t *testing.T, body string) []renderedGame {
	t.Helper()
	var games []renderedGame
	for _, m := range partyGameRE.FindAllStringSubmatch(body, -1) {
		id, _ := strconv.Atoi(m[1])
		g := renderedGame{AppID: id}
		if title := gameTitleRE.FindStringSubmatch(m[2]); title != nil {
			g.Name = title[1]
		}
		games = append(games, g)
	}
	return games
}

func (f *syncFixture) userGames(steamID string) map[int]db.StoredGame {
	games, err := f.store.UserGames(f.ctx, steamID)
	if err != nil {
		f.t.Fatal(err)
	}
	m := map[int]db.StoredGame{}
	for _, g := range games {
		m[g.AppID] = g
	}
	return m
}

func game(id int) steamapi.Game {
	return steamapi.Game{AppID: id, Name: fmt.Sprintf("Game %d", id), ImgIconURL: fmt.Sprintf("icon%d", id)}
}

// CLM-5: a sync stores names and icons from the owned-games response, and the
// shared result builds from those stored values with no library or details call.
func TestSyncStoresNamesAndIconsUsedByShared(t *testing.T) {
	f := newSyncFixture(t)
	caller := f.user(callerID)
	friend := f.user(friendA)
	lib := []steamapi.Game{{AppID: 10, Name: "Stored Alpha", ImgIconURL: "alphaicon"}, {AppID: 20, Name: "Stored Beta", ImgIconURL: "betaicon"}}
	f.steam.libraries[callerID], f.steam.libraries[friendA] = lib, lib
	f.steam.details = multiplayerDetails(10, 20)
	if err := view.SyncUserGames(f.ctx, f.steam, f.store, caller, callerID); err != nil {
		t.Fatal(err)
	}
	if err := view.SyncUserGames(f.ctx, f.steam, f.store, friend, friendA); err != nil {
		t.Fatal(err)
	}
	stored := f.userGames(friendA)
	if stored[10].Name != "Stored Alpha" || stored[10].ImgIconURL != "alphaicon" || stored[20].ImgIconURL != "betaicon" {
		t.Fatalf("stored=%+v", stored)
	}
	// From here Steam may not serve any library or details call.
	f.steam.details = failDetails(t)
	party := partyRendered(t, f.partyPage(caller, friend))
	if want := []renderedGame{{AppID: 10, Name: "Stored Alpha"}, {AppID: 20, Name: "Stored Beta"}}; !slices.Equal(party, want) {
		t.Fatalf("party games=%+v", party)
	}
	f.steam.friends = []string{friendA}
	f.steam.gamesErr[callerID] = errors.New("library down") // caller sync fails; stored data serves
	got := sharedRendered(t, f.sharedGames(caller, friendA))
	want := []renderedGame{{10, "Stored Alpha", "alphaicon"}, {20, "Stored Beta", "betaicon"}}
	if !slices.Equal(got, want) {
		t.Fatalf("shared games=%+v want %+v", got, want)
	}
}

// CLM-6: details are requested only for games without stored data, once.
func TestSyncRequestsDetailsOnlyOnce(t *testing.T) {
	f := newSyncFixture(t)
	u := f.user(callerID)
	f.steam.libraries[callerID] = []steamapi.Game{game(1), game(2), game(3)}
	f.steam.details = multiplayerDetails(2)
	for range 2 {
		if err := view.SyncUserGames(f.ctx, f.steam, f.store, u, callerID); err != nil {
			t.Fatal(err)
		}
	}
	counts := map[int]int{}
	for _, id := range f.steam.requested() {
		counts[id]++
	}
	if !maps(counts, map[int]int{1: 1, 2: 1, 3: 1}) {
		t.Fatalf("requested counts=%v", counts)
	}
	stored := f.userGames(callerID)
	if !stored[2].DataFetched || !stored[2].Multiplayer || !stored[1].DataFetched || stored[1].Multiplayer {
		t.Fatalf("stored=%+v", stored)
	}
}

func maps(a, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// CLM-7: an erroring batch stores nothing and is requested again later.
func TestSyncFailedBatchStoresNothingAndRetries(t *testing.T) {
	f := newSyncFixture(t)
	u := f.user(callerID)
	var lib []steamapi.Game
	for i := 1; i <= 12; i++ {
		lib = append(lib, game(i))
	}
	f.steam.libraries[callerID] = lib
	f.steam.details = func(ctx context.Context, ids []int) ([]steamapi.GameDetail, error) {
		if slices.Contains(ids, 11) {
			return nil, errors.New("batch failed")
		}
		return multiplayerDetails(ids...)(ctx, ids)
	}
	if err := view.SyncUserGames(f.ctx, f.steam, f.store, u, callerID); err == nil {
		t.Fatal("failed batch not reported")
	}
	stored := f.userGames(callerID)
	for i := 1; i <= 12; i++ {
		if stored[i].DataFetched != (i <= 10) {
			t.Fatalf("game %d data fetched=%v after first sync", i, stored[i].DataFetched)
		}
	}
	f.steam.detailsCalls = nil
	f.steam.details = multiplayerDetails(11, 12)
	if err := view.SyncUserGames(f.ctx, f.steam, f.store, u, callerID); err != nil {
		t.Fatal(err)
	}
	if got := f.steam.requested(); !slices.Equal(got, []int{11, 12}) {
		t.Fatalf("second sync requested %v", got)
	}
	stored = f.userGames(callerID)
	if !stored[11].DataFetched || !stored[11].Multiplayer || !stored[12].Multiplayer {
		t.Fatalf("retry not stored: %+v", stored)
	}
}

// CLM-8: an id omitted from a successful batch is stored no-data/not
// multiplayer, never requested again, and never shared.
func TestSyncOmittedIDStoredAsNoData(t *testing.T) {
	f := newSyncFixture(t)
	caller := f.user(callerID)
	friend := f.user(friendA)
	lib := []steamapi.Game{game(1), game(2)}
	f.steam.libraries[callerID], f.steam.libraries[friendA] = lib, lib
	f.steam.details = func(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
		var out []steamapi.GameDetail
		if slices.Contains(ids, 1) {
			out = append(out, steamapi.GameDetail{AppID: 1, Multiplayer: true})
		}
		return out, nil
	}
	if err := view.SyncUserGames(f.ctx, f.steam, f.store, caller, callerID); err != nil {
		t.Fatal(err)
	}
	stored := f.userGames(callerID)
	if b := stored[2]; !b.DataFetched || b.HasData || b.Multiplayer {
		t.Fatalf("omitted id stored as %+v", b)
	}
	f.steam.detailsCalls = nil
	if err := view.SyncUserGames(f.ctx, f.steam, f.store, friend, friendA); err != nil {
		t.Fatal(err)
	}
	if got := f.steam.requested(); len(got) != 0 {
		t.Fatalf("second sync requested %v", got)
	}
	f.steam.friends = []string{friendA}
	got := sharedRendered(t, f.sharedGames(caller, friendA))
	if len(got) != 1 || got[0].AppID != 1 {
		t.Fatalf("shared=%+v want only game 1", got)
	}
	if got := partyRendered(t, f.partyPage(caller, friend)); len(got) != 1 || got[0].AppID != 1 {
		t.Fatalf("party=%+v want only game 1", got)
	}
	if got := f.steam.requested(); len(got) != 0 {
		t.Fatalf("views requested details %v", got)
	}
}

// sharedPair seeds caller and friendA with the same library, both synced, and
// marks friendA a current Steam friend.
func (f *syncFixture) sharedPair(lib []steamapi.Game) (int64, int64) {
	caller := f.seed(callerID, lib)
	friend := f.seed(friendA, lib)
	f.steam.libraries[callerID] = lib
	f.steam.libraries[friendA] = lib
	f.steam.friends = []string{friendA}
	return caller, friend
}

// CLM-9: with all shared data stored, neither view calls GamesDetails; a game
// without stored data does go through the double.
func TestViewsUseStoredMultiplayerData(t *testing.T) {
	f := newSyncFixture(t)
	lib := []steamapi.Game{game(1), game(2)}
	caller, friend := f.sharedPair(lib)
	f.storeData(db.GameData{AppID: 1, HasData: true, Multiplayer: true}, db.GameData{AppID: 2, HasData: true, Multiplayer: true})
	f.steam.details = failDetails(t)
	if got := sharedRendered(t, f.sharedGames(caller, friendA)); len(got) != 2 {
		t.Fatalf("shared=%+v", got)
	}
	if got := partyRendered(t, f.partyPage(caller, friend)); len(got) != 2 {
		t.Fatalf("party=%+v", got)
	}

	// Falsifiability: a shared game with no stored data reaches the double.
	lib = append(lib, game(3))
	if err := f.store.StoreUserLibrary(f.ctx, friend, lib); err != nil {
		t.Fatal(err)
	}
	f.steam.libraries[callerID] = lib
	f.steam.details = multiplayerDetails(3)
	got := sharedRendered(t, f.sharedGames(caller, friendA))
	if req := f.steam.requested(); !slices.Equal(req, []int{3}) || len(got) != 3 {
		t.Fatalf("requested=%v shared=%+v", req, got)
	}
	f.steam.details = failDetails(t)
	if got := partyRendered(t, f.partyPage(caller, friend)); len(got) != 3 {
		t.Fatalf("party=%+v", got)
	}
}

// CLM-9 (party page): a shared game without stored data is fetched by the page.
func TestPartyPageFetchesOnlyUnstoredData(t *testing.T) {
	f := newSyncFixture(t)
	caller, friend := f.sharedPair([]steamapi.Game{game(1), game(2)})
	f.storeData(db.GameData{AppID: 1, HasData: true, Multiplayer: true})
	f.steam.details = multiplayerDetails(2)
	got := partyRendered(t, f.partyPage(caller, friend))
	if req := f.steam.requested(); !slices.Equal(req, []int{2}) || len(got) != 2 {
		t.Fatalf("requested=%v party=%+v", req, got)
	}
}

// CLM-10: a game whose fetch fails is left out; the rest renders with no error.
func TestFailedFetchOmitsGameWithoutError(t *testing.T) {
	for _, viewName := range []string{"shared", "party"} {
		t.Run(viewName, func(t *testing.T) {
			f := newSyncFixture(t)
			caller, friend := f.sharedPair([]steamapi.Game{game(1), game(2)})
			f.storeData(db.GameData{AppID: 1, HasData: true, Multiplayer: true})
			f.steam.details = func(context.Context, []int) ([]steamapi.GameDetail, error) {
				return nil, errors.New("private details failure")
			}
			var body string
			var got []renderedGame
			if viewName == "shared" {
				body = f.sharedGames(caller, friendA)
				got = sharedRendered(t, body)
			} else {
				body = f.partyPage(caller, friend)
				got = partyRendered(t, body)
			}
			if len(got) != 1 || got[0].AppID != 1 || got[0].Name != "Game 1" {
				t.Fatalf("games=%+v", got)
			}
			if strings.Contains(body, `role="alert"`) || strings.Contains(body, "private details failure") {
				t.Fatalf("error shown: %s", body)
			}
			if f.userGames(callerID)[2].DataFetched {
				t.Fatal("failed fetch stored data")
			}
		})
	}
}

// CLM-12: both views return exactly the multiplayer intersection, in caller
// order, with stored names and icons.
func TestViewsReturnStoredMultiplayerIntersectionInCallerOrder(t *testing.T) {
	f := newSyncFixture(t)
	callerLib := []steamapi.Game{game(50), game(10), game(40), game(30), game(20)}
	friendLib := []steamapi.Game{game(10), game(20), game(30), game(50), game(60)}
	caller := f.seed(callerID, callerLib)
	friend := f.seed(friendA, friendLib)
	f.steam.libraries[callerID] = callerLib
	f.steam.friends = []string{friendA}
	f.storeData(
		db.GameData{AppID: 50, HasData: true, Multiplayer: true},
		db.GameData{AppID: 10, HasData: true, Multiplayer: false},
		db.GameData{AppID: 40, HasData: true, Multiplayer: true},
		db.GameData{AppID: 30, HasData: true, Multiplayer: true},
		db.GameData{AppID: 20, HasData: true, Multiplayer: true},
		db.GameData{AppID: 60, HasData: true, Multiplayer: true},
	)
	f.steam.details = failDetails(t)
	want := []renderedGame{{50, "Game 50", "icon50"}, {30, "Game 30", "icon30"}, {20, "Game 20", "icon20"}}
	if got := sharedRendered(t, f.sharedGames(caller, friendA)); !slices.Equal(got, want) {
		t.Fatalf("shared=%+v want %+v", got, want)
	}
	party := f.partyPage(caller, friend)
	wantParty := []renderedGame{{AppID: 50, Name: "Game 50"}, {AppID: 30, Name: "Game 30"}, {AppID: 20, Name: "Game 20"}}
	if got := partyRendered(t, party); !slices.Equal(got, wantParty) {
		t.Fatalf("party=%+v want %+v", got, wantParty)
	}
	for _, w := range wantParty {
		if !strings.Contains(party, fmt.Sprintf("%d players online now", w.AppID*10)) {
			t.Errorf("missing live count for %d", w.AppID)
		}
	}
}

// CLM-18: a current friend with no users row is created, synced in-request,
// stored, and reflected in the result.
func TestSharedGamesSyncsNewFriendInRequest(t *testing.T) {
	f := newSyncFixture(t)
	caller := f.user(callerID)
	f.steam.friends = []string{friendA}
	f.steam.libraries[callerID] = []steamapi.Game{game(1), game(2), game(3)}
	f.steam.libraries[friendA] = []steamapi.Game{game(3), game(1)}
	f.steam.details = multiplayerDetails(1, 2, 3)
	got := sharedRendered(t, f.sharedGames(caller, friendA))
	if want := []renderedGame{{1, "Game 1", "icon1"}, {3, "Game 3", "icon3"}}; !slices.Equal(got, want) {
		t.Fatalf("shared=%+v", got)
	}
	states, err := f.store.GameSyncStates(f.ctx, []string{friendA})
	if err != nil || states[friendA].State != db.GameSyncSynced {
		t.Fatalf("friend state=%+v err=%v", states, err)
	}
	if games := f.userGames(friendA); len(games) != 2 {
		t.Fatalf("friend user_games=%+v", games)
	}
	var name string
	var everLoggedIn bool
	if err := f.pool.QueryRow(f.ctx, `SELECT persona_name, ever_logged_in FROM users WHERE steam_id_64=$1`, friendA).Scan(&name, &everLoggedIn); err != nil || name != "Persona 502" || everLoggedIn {
		t.Fatalf("friend row name=%q ever=%v err=%v", name, everLoggedIn, err)
	}
}

// CLM-19: a non-friend id is never synced or created and has no effect; added
// to Friends it passes through.
func TestSharedGamesIgnoresNonFriendIDs(t *testing.T) {
	f := newSyncFixture(t)
	caller, _ := f.sharedPair([]steamapi.Game{game(1), game(2)})
	f.storeData(db.GameData{AppID: 1, HasData: true, Multiplayer: true}, db.GameData{AppID: 2, HasData: true, Multiplayer: true})
	f.steam.libraries[stranger] = []steamapi.Game{game(1)}
	f.steam.details = failDetails(t)
	friendOnly := f.sharedGames(caller, friendA)
	withStranger := f.sharedGames(caller, friendA, stranger)
	if f.steam.gamesCalled(stranger) {
		t.Fatal("non-friend library fetched")
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM users WHERE steam_id_64=$1`, stranger).Scan(&n); err != nil || n != 0 {
		t.Fatalf("non-friend rows=%d err=%v", n, err)
	}
	if !slices.Equal(sharedRendered(t, friendOnly), sharedRendered(t, withStranger)) || len(sharedRendered(t, friendOnly)) != 2 {
		t.Fatalf("results differ: %s\n---\n%s", friendOnly, withStranger)
	}

	f.steam.friends = []string{friendA, stranger}
	got := sharedRendered(t, f.sharedGames(caller, friendA, stranger))
	if !f.steam.gamesCalled(stranger) || len(got) != 1 || got[0].AppID != 1 {
		t.Fatalf("friend id did not pass through: called=%v shared=%+v", f.steam.gamesCalled(stranger), got)
	}
}

// CLM-20: the first batch returns and is stored; the second blocks past the
// budget; the response returns and omits the second batch's games.
func TestSharedGamesBudgetStoresReturnedBatch(t *testing.T) {
	f := newSyncFixture(t)
	const budget = 300 * time.Millisecond
	f.app.sharedGamesSyncBudget = budget
	var lib []steamapi.Game
	for i := 1; i <= 15; i++ {
		lib = append(lib, game(i))
	}
	caller, _ := f.sharedPair(lib)
	f.steam.details = func(ctx context.Context, ids []int) ([]steamapi.GameDetail, error) {
		if slices.Contains(ids, 11) {
			<-ctx.Done() // blocks past the budget
			return nil, ctx.Err()
		}
		return multiplayerDetails(ids...)(ctx, ids)
	}
	start := time.Now()
	got := sharedRendered(t, f.sharedGames(caller, friendA))
	elapsed := time.Since(start)
	if elapsed < budget || elapsed > budget+5*time.Second {
		t.Fatalf("response took %s with budget %s", elapsed, budget)
	}
	stored := f.userGames(callerID)
	for i := 1; i <= 15; i++ {
		if stored[i].DataFetched != (i <= 10) {
			t.Fatalf("game %d data fetched=%v", i, stored[i].DataFetched)
		}
	}
	if len(got) != 10 || got[0].AppID != 1 || got[9].AppID != 10 {
		t.Fatalf("shared=%+v want first batch only", got)
	}
}

// CLM-21: each request re-fetches the caller's library, stores new games with
// data, then builds the result.
func TestSharedGamesResyncsCallerLibrary(t *testing.T) {
	f := newSyncFixture(t)
	caller, _ := f.sharedPair([]steamapi.Game{game(1)})
	if err := f.store.StoreUserLibrary(f.ctx, f.user(friendA), []steamapi.Game{game(1), game(77)}); err != nil {
		t.Fatal(err)
	}
	f.storeData(db.GameData{AppID: 1, HasData: true, Multiplayer: true})
	if _, ok := f.userGames(callerID)[77]; ok {
		t.Fatal("precondition: caller already stores game 77")
	}
	f.steam.libraries[callerID] = []steamapi.Game{game(1), game(77)}
	f.steam.details = multiplayerDetails(77)
	got := sharedRendered(t, f.sharedGames(caller, friendA))
	if !f.steam.gamesCalled(callerID) {
		t.Fatal("caller library not fetched")
	}
	if x := f.userGames(callerID)[77]; !x.DataFetched || !x.Multiplayer {
		t.Fatalf("game 77 stored as %+v", x)
	}
	if want := []renderedGame{{1, "Game 1", "icon1"}, {77, "Game 77", "icon77"}}; !slices.Equal(got, want) {
		t.Fatalf("shared=%+v", got)
	}
}

// CLM-24: private_or_no_games and never_synced members get distinct messages.
func TestSharedGamesMessagesDistinguishPrivateAndNotSynced(t *testing.T) {
	f := newSyncFixture(t)
	caller := f.seed(callerID, []steamapi.Game{game(1)})
	private := f.user(friendA)
	if _, err := f.store.RecordEmptyLibrary(f.ctx, private); err != nil {
		t.Fatal(err)
	}
	notSynced := f.user(friendB)
	f.steam.libraries[callerID] = []steamapi.Game{game(1)}
	f.steam.gamesErr[friendB] = errors.New("library unavailable") // sync fails
	f.steam.friends = []string{friendA, friendB}
	f.steam.details = multiplayerDetails(1)
	privateMsg := "no games found for user Persona 502. Their list may be private"
	notSyncedMsg := "games not synced yet for user Persona 503"
	for name, body := range map[string]string{
		"shared": f.sharedGames(caller, friendA, friendB),
		"party":  f.partyPage(caller, private, notSynced),
	} {
		for _, want := range []string{privateMsg, notSyncedMsg} {
			if strings.Count(body, want) != 1 {
				t.Errorf("%s: want %q once: %s", name, want, body)
			}
		}
		for _, forbidden := range []string{"no games found for user Persona 503", "games not synced yet for user Persona 502", "games not synced yet for user Persona 501", "no games found for user Persona 501"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s: unexpected %q", name, forbidden)
			}
		}
	}
	if !f.steam.gamesCalled(friendB) {
		t.Fatal("never-synced friend sync not attempted")
	}
}
