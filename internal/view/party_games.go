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

// gamesDetailsBatchSize is the maximum number of app IDs GamesDetails accepts
// per call (the batched steamapi endpoint errors above this).
const gamesDetailsBatchSize = 10

// gamesDetailsMaxConcurrency bounds the number of GamesDetails waves in flight
// at once so classification runs as bounded concurrent waves rather than a
// serial per-game loop or an unbounded fan-out.
const gamesDetailsMaxConcurrency = 10

type sharedGamesService interface {
	SharedAppIDs(context.Context, string, ...string) ([]int, error)
	GamesDetails(context.Context, []int) ([]steamapi.GameDetail, error)
	Games(context.Context, string, bool, bool) ([]steamapi.Game, error)
	GetNumberOfCurrentPlayers(context.Context, string) (int, error)
}

// sharedMultiplayerGames computes the shared MULTIPLAYER games for a caller and
// the other Steam IDs, returning rich []steamapi.Game (Name/AppID/ImgIconURL
// populated) in deterministic caller order. It:
//
//	a. intersects owned libraries via SharedAppIDs (propagating NoGamesError and
//	   any other error unchanged),
//	b. classifies the intersecting app IDs as multiplayer via GamesDetails run in
//	   bounded (<=10-wide, <=10 concurrent) waves, and
//	c. joins the multiplayer app IDs back to the caller's owned library (fetched
//	   once via Games) to carry Name/ImgIconURL.
func sharedMultiplayerGames(ctx context.Context, service sharedGamesService, caller string, others ...string) ([]steamapi.Game, error) {
	appIDs, err := service.SharedAppIDs(ctx, caller, others...)
	if err != nil {
		return nil, err
	}
	if len(appIDs) == 0 {
		return nil, nil
	}

	multiplayer, err := classifyMultiplayer(ctx, service, appIDs)
	if err != nil {
		return nil, err
	}
	if len(multiplayer) == 0 {
		return nil, nil
	}

	owned, err := service.Games(ctx, caller, true, true)
	if err != nil {
		return nil, err
	}
	ownedByAppID := make(map[int]steamapi.Game, len(owned))
	for _, game := range owned {
		ownedByAppID[game.AppID] = game
	}

	// Preserve deterministic caller order from SharedAppIDs.
	games := make([]steamapi.Game, 0, len(multiplayer))
	for _, appID := range appIDs {
		if !multiplayer[appID] {
			continue
		}
		if game, ok := ownedByAppID[appID]; ok {
			games = append(games, game)
			continue
		}
		// The caller owns every intersecting app ID, so this is defensive only.
		games = append(games, steamapi.Game{AppID: appID})
	}
	return games, nil
}

// classifyMultiplayer chunks appIDs into <=10-wide batches and runs GamesDetails
// on each chunk with bounded concurrency (<=10 waves in flight), returning the
// set of app IDs whose GameDetail.Multiplayer is true.
func classifyMultiplayer(ctx context.Context, service sharedGamesService, appIDs []int) (map[int]bool, error) {
	chunks := make([][]int, 0, (len(appIDs)+gamesDetailsBatchSize-1)/gamesDetailsBatchSize)
	for start := 0; start < len(appIDs); start += gamesDetailsBatchSize {
		end := start + gamesDetailsBatchSize
		if end > len(appIDs) {
			end = len(appIDs)
		}
		chunks = append(chunks, appIDs[start:end])
	}

	results := make([][]steamapi.GameDetail, len(chunks))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(gamesDetailsMaxConcurrency)
	for i, chunk := range chunks {
		group.Go(func() error {
			details, err := service.GamesDetails(groupCtx, chunk)
			if err != nil {
				return fmt.Errorf("party shared games: classify: %w", err)
			}
			// Each worker owns its slot; completion order cannot swap results.
			results[i] = details
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	multiplayer := make(map[int]bool)
	for _, details := range results {
		for _, detail := range details {
			if detail.Multiplayer {
				multiplayer[detail.AppID] = true
			}
		}
	}
	return multiplayer, nil
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
	games, err := sharedMultiplayerGames(ctx, service, ids[0], ids[1:]...)
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
