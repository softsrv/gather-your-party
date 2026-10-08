package view

import (
	"context"
	"fmt"
	"strconv"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
	"golang.org/x/sync/errgroup"
)

type sharedGamesService interface {
	SharedGames(context.Context, string, ...string) ([]steamapi.Game, error)
	GetNumberOfCurrentPlayers(context.Context, string) (int, error)
}

// BuildSharedGames uses only the database-loaded roster to build the party's
// shared games and live counts. A private/empty library becomes a named message;
// other service failures are returned to the caller without partial counts.
func BuildSharedGames(ctx context.Context, service sharedGamesService, members []db.PartyMember) ([]component.SharedGameCount, []string, error) {
	if len(members) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, len(members))
	roster := make([]steamapi.Player, len(members))
	for i, member := range members {
		ids[i] = member.SteamID64
		roster[i] = steamapi.Player{SteamID: member.SteamID64, PersonaName: member.Name}
	}
	games, err := service.SharedGames(ctx, ids[0], ids[1:]...)
	if err != nil {
		if messages, ok := sharedGamesNoGamesMessages(err, roster); ok {
			return nil, append([]string{"Shared games cannot be computed."}, messages...), nil
		}
		return nil, nil, fmt.Errorf("party shared games: %w", err)
	}
	counts := make([]component.SharedGameCount, len(games))
	fetches, countCtx := errgroup.WithContext(ctx)
	for i, game := range games {
		fetches.Go(func() error {
			count, err := service.GetNumberOfCurrentPlayers(countCtx, strconv.Itoa(game.AppID))
			if err != nil {
				return fmt.Errorf("party shared games: player count: %w", err)
			}
			// Each worker owns its slot; completion order cannot swap game counts.
			counts[i] = component.SharedGameCount{Game: game, PlayerCount: count}
			return nil
		})
	}
	if err := fetches.Wait(); err != nil {
		return nil, nil, err
	}
	return counts, nil, nil
}
