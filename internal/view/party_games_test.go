package view

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

type partyGamesStub struct {
	shared func(context.Context, string, ...string) ([]steamapi.Game, error)
	count  func(context.Context, string) (int, error)
}

func (s partyGamesStub) SharedGames(ctx context.Context, first string, rest ...string) ([]steamapi.Game, error) {
	return s.shared(ctx, first, rest...)
}

func (s partyGamesStub) GetNumberOfCurrentPlayers(ctx context.Context, id string) (int, error) {
	return s.count(ctx, id)
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
		shared: func(_ context.Context, first string, rest ...string) ([]steamapi.Game, error) {
			if first != "stored-leader" || !reflect.DeepEqual(rest, []string{"stored-member", "stored-third"}) {
				t.Errorf("roster=%s %v", first, rest)
			}
			return games, nil
		},
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
	service := partyGamesStub{shared: func(context.Context, string, ...string) ([]steamapi.Game, error) {
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
	service := partyGamesStub{shared: func(_ context.Context, first string, rest ...string) ([]steamapi.Game, error) {
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
				shared: func(context.Context, string, ...string) ([]steamapi.Game, error) {
					if !countsFail {
						return nil, failure
					}
					return []steamapi.Game{{AppID: 10}}, nil
				},
				count: func(context.Context, string) (int, error) { return 0, failure },
			}
			games, messages, err := BuildSharedGames(context.Background(), service, []db.PartyMember{{SteamID64: "solo"}})
			if !errors.Is(err, failure) || len(games) != 0 || len(messages) != 0 {
				t.Fatalf("partial or swallowed failure: %v %v %v", games, messages, err)
			}
		})
	}
}
