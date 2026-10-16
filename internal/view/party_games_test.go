package view

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

// memStore is an in-memory SharedGamesListStore double.
type memStore struct {
	mu                sync.Mutex
	nextID            int64
	users             map[string]*memUser
	games             map[int]*db.StoredGame
	storeErr          error
	syncStatesErr     error
	sharedStoredErr   error
	missingDataErr    error
	dataCalls         [][]db.GameData
	syncStatesCalls   int
	sharedStoredCalls int
	missingDataCalls  int
}

type memUser struct {
	id      int64
	state   string
	library []int
	profile steamapi.Player
	// syncedAt is the last successful library sync; syncErr the last failure.
	syncedAt *time.Time
	syncErr  string
	lock     sync.Mutex
}

func newMemStore() *memStore {
	return &memStore{users: map[string]*memUser{}, games: map[int]*db.StoredGame{}}
}

func (s *memStore) addUser(steamID, state string, library ...db.StoredGame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	u := &memUser{id: s.nextID, state: state}
	for _, g := range library {
		if _, ok := s.games[g.AppID]; !ok {
			game := g
			s.games[g.AppID] = &game
		}
		u.library = append(u.library, g.AppID)
	}
	s.users[steamID] = u
}

func (s *memStore) GameSyncStates(_ context.Context, ids []string) (map[string]db.GameSyncState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncStatesCalls++
	if s.syncStatesErr != nil {
		return nil, s.syncStatesErr
	}
	states := map[string]db.GameSyncState{}
	for _, id := range ids {
		if u, ok := s.users[id]; ok {
			states[id] = db.GameSyncState{SteamID64: id, UserID: u.id, State: u.state}
		}
	}
	return states, nil
}

func (s *memStore) SharedStoredGames(_ context.Context, caller string, others []string) ([]db.StoredGame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sharedStoredCalls++
	if s.sharedStoredErr != nil {
		return nil, s.sharedStoredErr
	}
	u, ok := s.users[caller]
	if !ok {
		return nil, nil
	}
	var shared []db.StoredGame
	for _, appID := range u.library {
		all := true
		for _, other := range others {
			o, ok := s.users[other]
			if !ok || !slices.Contains(o.library, appID) {
				all = false
			}
		}
		if all {
			shared = append(shared, *s.games[appID])
		}
	}
	return shared, nil
}

func (s *memStore) GamesMissingData(_ context.Context, appIDs []int) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missingDataCalls++
	if s.missingDataErr != nil {
		return nil, s.missingDataErr
	}
	var missing []int
	for _, id := range appIDs {
		if g, ok := s.games[id]; ok && !g.DataFetched {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

func (s *memStore) StoreGameData(_ context.Context, data []db.GameData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.storeErr != nil {
		return s.storeErr
	}
	s.dataCalls = append(s.dataCalls, data)
	for _, d := range data {
		if g, ok := s.games[d.AppID]; ok {
			g.DataFetched, g.HasData, g.Multiplayer = true, d.HasData, d.Multiplayer
		}
	}
	return nil
}

func (s *memStore) StoreUserLibrary(_ context.Context, userID int64, games []steamapi.Game) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.id != userID {
			continue
		}
		for _, g := range games {
			if existing, ok := s.games[g.AppID]; ok {
				existing.Name, existing.ImgIconURL = g.Name, g.ImgIconURL
			} else {
				s.games[g.AppID] = &db.StoredGame{AppID: g.AppID, Name: g.Name, ImgIconURL: g.ImgIconURL}
			}
			if !slices.Contains(u.library, g.AppID) {
				u.library = append(u.library, g.AppID)
			}
		}
		u.state = db.GameSyncSynced
		return nil
	}
	return errors.New("unknown user")
}

func (s *memStore) RecordEmptyLibrary(_ context.Context, userID int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.users {
		if u.id == userID {
			u.state = db.GameSyncSynced
			if len(u.library) == 0 {
				u.state = db.GameSyncPrivateOrNoGames
			}
			return u.state, nil
		}
	}
	return "", errors.New("unknown user")
}

func (s *memStore) ResolveUserID(_ context.Context, steamID string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[steamID]; ok {
		return u.id, true, nil
	}
	return 0, false, nil
}

func (s *memStore) UpsertFriend(_ context.Context, steamID string, player steamapi.Player) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[steamID]; ok {
		u.profile = player
		return u.id, nil
	}
	s.nextID++
	s.users[steamID] = &memUser{id: s.nextID, state: db.GameSyncNeverSynced, profile: player}
	return s.nextID, nil
}

func (s *memStore) userByID(userID int64) *memUser {
	for _, u := range s.users {
		if u.id == userID {
			return u
		}
	}
	return nil
}

func (s *memStore) LibrarySyncDue(_ context.Context, userID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.userByID(userID)
	return u == nil || u.syncedAt == nil || time.Since(*u.syncedAt) > 24*time.Hour, nil
}

func (s *memStore) RecordSyncSuccess(_ context.Context, userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.userByID(userID); u != nil {
		now := time.Now()
		u.syncedAt, u.syncErr = &now, ""
	}
	return nil
}

func (s *memStore) RecordSyncFailure(_ context.Context, userID int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.userByID(userID); u != nil {
		u.syncErr = reason
	}
	return nil
}

func (s *memStore) WithUserSyncLock(ctx context.Context, userID int64, fn func(context.Context) error) error {
	s.mu.Lock()
	u := s.userByID(userID)
	s.mu.Unlock()
	if u == nil {
		return fn(ctx)
	}
	u.lock.Lock()
	defer u.lock.Unlock()
	return fn(ctx)
}

// steamStub is a SteamService double; unset funcs fail the call.
type steamStub struct {
	gamesDetails      func(context.Context, []int) ([]steamapi.GameDetail, error)
	games             func(context.Context, string, bool, bool) ([]steamapi.Game, error)
	friends           func(context.Context, string) ([]steamapi.Friend, error)
	players           func(context.Context, []string) ([]steamapi.Player, error)
	gamesDetailsCalls *int
}

func (s steamStub) GamesDetails(ctx context.Context, appIDs []int) ([]steamapi.GameDetail, error) {
	if s.gamesDetailsCalls != nil {
		*s.gamesDetailsCalls++
	}
	if s.gamesDetails == nil {
		return nil, errors.New("unexpected GamesDetails call")
	}
	return s.gamesDetails(ctx, appIDs)
}

func (s steamStub) Games(ctx context.Context, steamID string, a, b bool) ([]steamapi.Game, error) {
	if s.games == nil {
		return nil, errors.New("unexpected Games call")
	}
	return s.games(ctx, steamID, a, b)
}

func (s steamStub) Friends(ctx context.Context, id string) ([]steamapi.Friend, error) {
	if s.friends == nil {
		return nil, errors.New("unexpected Friends call")
	}
	return s.friends(ctx, id)
}

func (s steamStub) Players(ctx context.Context, ids []string) ([]steamapi.Player, error) {
	if s.players == nil {
		return nil, errors.New("unexpected Players call")
	}
	return s.players(ctx, ids)
}

// allMultiplayer classifies every requested app ID as multiplayer, honoring the
// <=10-per-call batch limit.
func allMultiplayer(_ context.Context, appIDs []int) ([]steamapi.GameDetail, error) {
	if len(appIDs) > 10 {
		return nil, fmt.Errorf("batch too large: %d", len(appIDs))
	}
	details := make([]steamapi.GameDetail, len(appIDs))
	for i, id := range appIDs {
		details[i] = steamapi.GameDetail{AppID: id, Multiplayer: true}
	}
	return details, nil
}

func stored(appID int, name string, multiplayer bool) db.StoredGame {
	return db.StoredGame{AppID: appID, Name: name, ImgIconURL: name + "-icon", DataFetched: true, HasData: true, Multiplayer: multiplayer}
}

// CLM-9: with stored game data, BuildSharedGames returns the stored shared games
// without issuing any current-player-count request.
func TestBuildSharedGamesReturnsStoredIntersectionWithoutCountRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store := newMemStore()
	library := []db.StoredGame{stored(42, "First", true), stored(5, "Single", false), stored(7, "Second", true)}
	store.addUser("leader", db.GameSyncSynced, library...)
	store.addUser("member", db.GameSyncSynced, library...)

	steam := steamStub{}
	got, messages, err := BuildSharedGames(ctx, steam, store, []db.PartyMember{{SteamID64: "leader"}, {SteamID64: "member"}})
	want := []component.SharedGameCount{
		{Game: steamapi.Game{AppID: 42, Name: "First", ImgIconURL: "First-icon"}},
		{Game: steamapi.Game{AppID: 7, Name: "Second", ImgIconURL: "Second-icon"}},
	}
	if err != nil || len(messages) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("games=%+v messages=%v err=%v", got, messages, err)
	}
}

func TestBuildSharedGamesMessagesNameMembers(t *testing.T) {
	store := newMemStore()
	store.addUser("leader", db.GameSyncSynced, stored(1, "A", true))
	store.addUser("private", db.GameSyncPrivateOrNoGames)
	store.addUser("fresh", db.GameSyncNeverSynced)
	members := []db.PartyMember{{SteamID64: "leader"}, {SteamID64: "private", Name: "Alyx"}, {SteamID64: "fresh", Name: "Gordon"}, {SteamID64: "unknown"}}
	games, messages, err := BuildSharedGames(context.Background(), steamStub{}, store, members)
	want := []string{
		"Shared games cannot be computed.",
		"no games found for user Alyx. Their list may be private",
		"games not synced yet for user Gordon",
		"games not synced yet for user unknown",
	}
	if err != nil || len(games) != 0 || !reflect.DeepEqual(messages, want) {
		t.Fatalf("games=%v messages=%v err=%v", games, messages, err)
	}
}

func TestBuildSharedGamesEmptyAndSoloRoster(t *testing.T) {
	// Empty roster returns nil results with zero store and Steam calls.
	{
		store := newMemStore()
		store.syncStatesErr = errors.New("unexpected GameSyncStates call")
		store.sharedStoredErr = errors.New("unexpected SharedStoredGames call")
		var emptyDetailsCalls int
		emptySteam := steamStub{
			gamesDetailsCalls: &emptyDetailsCalls,
		}
		games, messages, err := BuildSharedGames(context.Background(), emptySteam, store, nil)
		if store.syncStatesCalls != 0 || store.sharedStoredCalls != 0 || store.missingDataCalls != 0 || len(store.dataCalls) != 0 || emptyDetailsCalls != 0 {
			t.Fatalf("empty roster calls: syncStates=%d sharedStored=%d missingData=%d dataCalls=%d details=%d",
				store.syncStatesCalls, store.sharedStoredCalls, store.missingDataCalls, len(store.dataCalls), emptyDetailsCalls)
		}
		if err != nil || games != nil || messages != nil {
			t.Fatalf("empty roster: games=%v messages=%v err=%v", games, messages, err)
		}
	}

	// Solo roster returns (nil, nil, nil) before reading store or calling Steam across leader sync states.
	for _, tc := range []struct {
		name  string
		setup func(*memStore)
	}{
		{
			name: "store and steam that would fail",
			setup: func(s *memStore) {
				s.syncStatesErr = errors.New("unexpected GameSyncStates call")
				s.sharedStoredErr = errors.New("unexpected SharedStoredGames call")
				s.missingDataErr = errors.New("unexpected GamesMissingData call")
				s.storeErr = errors.New("unexpected StoreGameData call")
			},
		},
		{
			name: "synced leader with stored games",
			setup: func(s *memStore) {
				s.addUser("solo", db.GameSyncSynced, stored(99, "Solo", true))
				s.syncStatesErr = errors.New("unexpected GameSyncStates call")
				s.sharedStoredErr = errors.New("unexpected SharedStoredGames call")
				s.missingDataErr = errors.New("unexpected GamesMissingData call")
				s.storeErr = errors.New("unexpected StoreGameData call")
			},
		},
		{
			name: "never synced leader",
			setup: func(s *memStore) {
				s.addUser("solo", db.GameSyncNeverSynced)
				s.syncStatesErr = errors.New("unexpected GameSyncStates call")
				s.sharedStoredErr = errors.New("unexpected SharedStoredGames call")
			},
		},
		{
			name: "failed sync leader",
			setup: func(s *memStore) {
				s.addUser("solo", "sync_failed")
				s.syncStatesErr = errors.New("unexpected GameSyncStates call")
				s.sharedStoredErr = errors.New("unexpected SharedStoredGames call")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			tc.setup(store)
			var detailsCalls int
			steam := steamStub{
				gamesDetailsCalls: &detailsCalls,
			}
			games, messages, err := BuildSharedGames(context.Background(), steam, store, []db.PartyMember{{SteamID64: "solo", Name: "Leader"}})
			if store.syncStatesCalls != 0 || store.sharedStoredCalls != 0 || store.missingDataCalls != 0 || len(store.dataCalls) != 0 || detailsCalls != 0 {
				t.Fatalf("expected zero store and Steam calls, got syncStates=%d sharedStored=%d missingData=%d dataCalls=%d details=%d",
					store.syncStatesCalls, store.sharedStoredCalls, store.missingDataCalls, len(store.dataCalls), detailsCalls)
			}
			if err != nil || games != nil || messages != nil {
				t.Fatalf("expected nil games, messages, error for solo roster: games=%v messages=%v err=%v", games, messages, err)
			}
		})
	}
}

func TestBuildSharedGamesStoreErrorReturned(t *testing.T) {
	failure := errors.New("store unavailable")
	store := newMemStore()
	store.addUser("leader", db.GameSyncSynced, stored(10, "Ten", true))
	store.addUser("member", db.GameSyncSynced, stored(10, "Ten", true))
	store.sharedStoredErr = failure
	games, messages, err := BuildSharedGames(context.Background(), steamStub{}, store, []db.PartyMember{{SteamID64: "leader"}, {SteamID64: "member"}})
	if !errors.Is(err, failure) || len(games) != 0 || len(messages) != 0 {
		t.Fatalf("partial or swallowed failure: %v %v %v", games, messages, err)
	}
}

// FetchMissingGameData runs bounded concurrent batches of at most ten ids.
func TestFetchMissingGameDataBoundedConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store := newMemStore()
	var library []db.StoredGame
	var appIDs []int
	for i := range 25 {
		library = append(library, db.StoredGame{AppID: 1000 + i})
		appIDs = append(appIDs, 1000+i)
	}
	store.addUser("u", db.GameSyncSynced, library...)

	const wantBatches = 3
	var mu sync.Mutex
	inFlight, maxInFlight, calls := 0, 0, 0
	started := make(chan struct{}, wantBatches)
	release := make(chan struct{})
	go func() {
		defer close(release)
		for range wantBatches {
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
		}
	}()
	steam := steamStub{gamesDetails: func(c context.Context, ids []int) ([]steamapi.GameDetail, error) {
		mu.Lock()
		inFlight++
		calls++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		started <- struct{}{}
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return allMultiplayer(c, ids)
	}}
	if err := FetchMissingGameData(ctx, steam, store, appIDs); err != nil {
		t.Fatal(err)
	}
	if calls != wantBatches || maxInFlight <= 1 || maxInFlight > gamesDetailsMaxConcurrency {
		t.Fatalf("calls=%d maxInFlight=%d", calls, maxInFlight)
	}
	if missing, _ := store.GamesMissingData(ctx, appIDs); len(missing) != 0 {
		t.Fatalf("unstored ids: %v", missing)
	}
}

// A failed batch stores nothing; its sibling batch is still stored.
func TestFetchMissingGameDataFailedBatchStoresNothing(t *testing.T) {
	store := newMemStore()
	var library []db.StoredGame
	var appIDs []int
	for i := range 12 {
		library = append(library, db.StoredGame{AppID: i + 1})
		appIDs = append(appIDs, i+1)
	}
	store.addUser("u", db.GameSyncSynced, library...)
	failure := errors.New("details down")
	steam := steamStub{gamesDetails: func(c context.Context, ids []int) ([]steamapi.GameDetail, error) {
		if slices.Contains(ids, 1) {
			return nil, failure
		}
		return allMultiplayer(c, ids)
	}}
	err := FetchMissingGameData(context.Background(), steam, store, appIDs)
	if !errors.Is(err, failure) {
		t.Fatalf("err=%v", err)
	}
	missing, _ := store.GamesMissingData(context.Background(), appIDs)
	if !reflect.DeepEqual(missing, appIDs[:10]) {
		t.Fatalf("missing=%v want first batch only", missing)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written, so a server-side diagnostic can be asserted.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %s", err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %s", err)
	}
	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %s", err)
	}
	return string(captured)
}

// A panicking GamesDetails is that batch's error, not a process crash: the
// panicking batch stores nothing and its sibling is still stored.
func TestFetchMissingGameDataPanicIsBatchError(t *testing.T) {
	store := newMemStore()
	var library []db.StoredGame
	var appIDs []int
	for i := range 12 {
		library = append(library, db.StoredGame{AppID: i + 1})
		appIDs = append(appIDs, i+1)
	}
	store.addUser("u", db.GameSyncSynced, library...)
	steam := steamStub{gamesDetails: func(c context.Context, ids []int) ([]steamapi.GameDetail, error) {
		if slices.Contains(ids, 1) {
			panic("details exploded")
		}
		return allMultiplayer(c, ids)
	}}
	var err error
	out := captureStdout(t, func() {
		err = FetchMissingGameData(context.Background(), steam, store, appIDs)
	})
	if err == nil || !strings.Contains(err.Error(), "panic: details exploded") || !strings.Contains(out, "details exploded") {
		t.Fatalf("err=%v log=%q", err, out)
	}
	missing, _ := store.GamesMissingData(context.Background(), appIDs)
	if !reflect.DeepEqual(missing, appIDs[:10]) {
		t.Fatalf("missing=%v want panicking batch only", missing)
	}
}

// A panic inside a request-path member sync goroutine is logged as that
// member's failure; it does not end the process, and the caller still returns.
func TestSyncMembersPanicIsContained(t *testing.T) {
	store := newMemStore()
	store.addUser("caller", db.GameSyncNeverSynced)
	steam := steamStub{games: func(context.Context, string, bool, bool) ([]steamapi.Game, error) {
		panic("games exploded")
	}}
	h := &SharedGamesList{Store: store, Steam: func() SteamService { return steam }}
	out := captureStdout(t, func() {
		h.syncMembers(context.Background(), steam, "caller", nil, nil)
	})
	if !strings.Contains(out, "shared games: sync games caller: panic: games exploded") {
		t.Fatalf("log=%q", out)
	}
}

func TestSyncUserGamesEmptyRecordsPrivate(t *testing.T) {
	store := newMemStore()
	store.addUser("u", db.GameSyncNeverSynced)
	steam := steamStub{games: func(context.Context, string, bool, bool) ([]steamapi.Game, error) { return nil, nil }}
	if err := SyncUserGames(context.Background(), steam, store, 1, "u"); err != nil {
		t.Fatal(err)
	}
	if store.users["u"].state != db.GameSyncPrivateOrNoGames {
		t.Fatalf("state=%s", store.users["u"].state)
	}
}
