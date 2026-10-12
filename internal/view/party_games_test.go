package view

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

type partyGamesStub struct {
	sharedAppIDs func(context.Context, string, ...string) ([]int, error)
	gamesDetails func(context.Context, []int) ([]steamapi.GameDetail, error)
	games        func(context.Context, string, bool, bool) ([]steamapi.Game, error)
	count        func(context.Context, string) (int, error)
}

func (s partyGamesStub) SharedAppIDs(ctx context.Context, first string, rest ...string) ([]int, error) {
	return s.sharedAppIDs(ctx, first, rest...)
}

func (s partyGamesStub) GamesDetails(ctx context.Context, appIDs []int) ([]steamapi.GameDetail, error) {
	return s.gamesDetails(ctx, appIDs)
}

func (s partyGamesStub) Games(ctx context.Context, steamID string, includeAppInfo, includePlayedFree bool) ([]steamapi.Game, error) {
	return s.games(ctx, steamID, includeAppInfo, includePlayedFree)
}

func (s partyGamesStub) GetNumberOfCurrentPlayers(ctx context.Context, id string) (int, error) {
	return s.count(ctx, id)
}

// allMultiplayer is a GamesDetails stub that classifies every requested app ID as
// multiplayer, honoring the <=10-per-call batch limit.
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

// ownedLibrary is a Games stub that returns a known owned library.
func ownedLibrary(games []steamapi.Game) func(context.Context, string, bool, bool) ([]steamapi.Game, error) {
	return func(context.Context, string, bool, bool) ([]steamapi.Game, error) {
		return games, nil
	}
}

func TestBuildSharedGamesConcurrentCountsPreservePairing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := make(chan string, 2)
	release := make(chan struct{})
	go func() {
		defer close(release)
		for range 2 {
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
		}
	}()
	games := []steamapi.Game{{AppID: 42, Name: "First"}, {AppID: 7, Name: "Second"}}
	service := partyGamesStub{
		sharedAppIDs: func(_ context.Context, first string, rest ...string) ([]int, error) {
			if first != "stored-leader" || !reflect.DeepEqual(rest, []string{"stored-member", "stored-third"}) {
				t.Errorf("roster=%s %v", first, rest)
			}
			return []int{42, 7}, nil
		},
		gamesDetails: allMultiplayer,
		games:        ownedLibrary(games),
		count: func(ctx context.Context, id string) (int, error) {
			started <- id
			select {
			case <-release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			switch id {
			case "42":
				return 900, nil
			case "7":
				return 0, nil
			default:
				return 0, fmt.Errorf("unexpected appid %q", id)
			}
		},
	}
	got, messages, err := BuildSharedGames(ctx, service, []db.PartyMember{{SteamID64: "stored-leader"}, {SteamID64: "stored-member"}, {SteamID64: "stored-third"}})
	want := []component.SharedGameCount{{Game: games[0], PlayerCount: 900}, {Game: games[1], PlayerCount: 0}}
	if err != nil || len(messages) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("games=%+v messages=%v err=%v", got, messages, err)
	}
}

func TestBuildSharedGamesNoGamesNamesAndFallback(t *testing.T) {
	service := partyGamesStub{sharedAppIDs: func(context.Context, string, ...string) ([]int, error) {
		return nil, fmt.Errorf("wrapped: %w", &steamapi.NoGamesError{SteamIDs: []string{"named", "unnamed", "unknown"}})
	}}
	games, messages, err := BuildSharedGames(context.Background(), service, []db.PartyMember{{SteamID64: "named", Name: "Alyx"}, {SteamID64: "unnamed"}})
	want := []string{"Shared games cannot be computed.", "no games found for user Alyx. Their list may be private", "no games found for user unnamed. Their list may be private", "no games found for user unknown. Their list may be private"}
	if err != nil || len(games) != 0 || !reflect.DeepEqual(messages, want) {
		t.Fatalf("games=%v messages=%v err=%v", games, messages, err)
	}
}

func TestBuildSharedGamesEmptyAndSoloRoster(t *testing.T) {
	if games, messages, err := BuildSharedGames(context.Background(), partyGamesStub{}, nil); err != nil || len(games) != 0 || len(messages) != 0 {
		t.Fatalf("empty roster: %v %v %v", games, messages, err)
	}
	service := partyGamesStub{sharedAppIDs: func(_ context.Context, first string, rest ...string) ([]int, error) {
		if first != "solo" || len(rest) != 0 {
			t.Errorf("solo roster=%s %v", first, rest)
		}
		return nil, nil
	}}
	if games, messages, err := BuildSharedGames(context.Background(), service, []db.PartyMember{{SteamID64: "solo"}}); err != nil || len(games) != 0 || len(messages) != 0 {
		t.Fatalf("empty intersection: %v %v %v", games, messages, err)
	}
}

func TestBuildSharedGamesServiceErrors(t *testing.T) {
	failure := errors.New("Steam unavailable")
	for _, countsFail := range []bool{false, true} {
		t.Run(fmt.Sprintf("counts=%t", countsFail), func(t *testing.T) {
			service := partyGamesStub{
				sharedAppIDs: func(context.Context, string, ...string) ([]int, error) {
					if !countsFail {
						return nil, failure
					}
					return []int{10}, nil
				},
				gamesDetails: allMultiplayer,
				games:        ownedLibrary([]steamapi.Game{{AppID: 10}}),
				count:        func(context.Context, string) (int, error) { return 0, failure },
			}
			games, messages, err := BuildSharedGames(context.Background(), service, []db.PartyMember{{SteamID64: "solo"}})
			if !errors.Is(err, failure) || len(games) != 0 || len(messages) != 0 {
				t.Fatalf("partial or swallowed failure: %v %v %v", games, messages, err)
			}
		})
	}
}

// TestSharedMultiplayerGamesBoundedClassificationConcurrency proves classification
// runs as bounded concurrent GamesDetails waves (>1, <=10) rather than a serial
// per-game loop or an unbounded fan-out. The stub records concurrent in-flight
// GamesDetails calls and blocks until released, mirroring the count fan-out test.
func TestSharedMultiplayerGamesBoundedClassificationConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 25 intersecting app IDs => 3 chunks of 10/10/5 => 3 concurrent waves.
	appIDs := make([]int, 25)
	owned := make([]steamapi.Game, 25)
	for i := range appIDs {
		appIDs[i] = 1000 + i
		owned[i] = steamapi.Game{AppID: 1000 + i, Name: fmt.Sprintf("Game%d", i), ImgIconURL: fmt.Sprintf("icon%d", i)}
	}
	wantChunks := 3

	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	chunkCalls := 0
	started := make(chan struct{}, wantChunks)
	release := make(chan struct{})

	service := partyGamesStub{
		sharedAppIDs: func(context.Context, string, ...string) ([]int, error) {
			return appIDs, nil
		},
		gamesDetails: func(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
			if len(ids) > 10 {
				return nil, fmt.Errorf("batch too large: %d", len(ids))
			}
			mu.Lock()
			inFlight++
			chunkCalls++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			started <- struct{}{}
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return allMultiplayer(ctx, ids)
		},
		games: ownedLibrary(owned),
		count: func(context.Context, string) (int, error) { return 0, nil },
	}

	// Release once all chunk calls are concurrently in flight; if classification
	// were serial, only one would ever start and this would block until timeout.
	go func() {
		for range wantChunks {
			select {
			case <-started:
			case <-ctx.Done():
				close(release)
				return
			}
		}
		close(release)
	}()

	games, err := sharedMultiplayerGames(ctx, service, "caller", "other")
	if err != nil {
		t.Fatalf("sharedMultiplayerGames err=%v", err)
	}
	if len(games) != len(appIDs) {
		t.Fatalf("want %d games, got %d", len(appIDs), len(games))
	}
	mu.Lock()
	defer mu.Unlock()
	if chunkCalls != wantChunks {
		t.Fatalf("want %d GamesDetails chunk calls, got %d", wantChunks, chunkCalls)
	}
	if maxInFlight <= 1 {
		t.Fatalf("classification was serial: max in-flight=%d (want >1)", maxInFlight)
	}
	if maxInFlight > gamesDetailsMaxConcurrency {
		t.Fatalf("classification unbounded: max in-flight=%d (want <=%d)", maxInFlight, gamesDetailsMaxConcurrency)
	}
}

// TestSharedMultiplayerGamesRichJoin exercises the rich-game join: every shared
// multiplayer game carries a non-empty Name, correct AppID, and the ImgIconURL
// from the caller's owned library (not an ID-only entry). It also verifies only
// multiplayer app IDs survive, in deterministic caller order.
func TestSharedMultiplayerGamesRichJoin(t *testing.T) {
	owned := []steamapi.Game{
		{AppID: 10, Name: "Alpha", ImgIconURL: "alpha-icon"},
		{AppID: 20, Name: "Bravo", ImgIconURL: "bravo-icon"},
		{AppID: 30, Name: "Charlie", ImgIconURL: "charlie-icon"},
	}
	service := partyGamesStub{
		sharedAppIDs: func(context.Context, string, ...string) ([]int, error) {
			return []int{10, 20, 30}, nil
		},
		gamesDetails: func(_ context.Context, ids []int) ([]steamapi.GameDetail, error) {
			details := make([]steamapi.GameDetail, len(ids))
			for i, id := range ids {
				// 20 is single-player; 10 and 30 are multiplayer.
				details[i] = steamapi.GameDetail{AppID: id, Multiplayer: id != 20}
			}
			return details, nil
		},
		games: ownedLibrary(owned),
		count: func(context.Context, string) (int, error) { return 0, nil },
	}

	games, err := sharedMultiplayerGames(context.Background(), service, "caller", "other")
	if err != nil {
		t.Fatalf("sharedMultiplayerGames err=%v", err)
	}
	want := []steamapi.Game{owned[0], owned[2]}
	if !reflect.DeepEqual(games, want) {
		t.Fatalf("rich join mismatch: got %+v want %+v", games, want)
	}
	for _, g := range games {
		if g.Name == "" || g.ImgIconURL == "" || g.AppID == 0 {
			t.Fatalf("ID-only entry, missing rich fields: %+v", g)
		}
	}
}
