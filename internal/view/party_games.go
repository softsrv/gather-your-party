package view

import (
	"context"
	"fmt"
	"strconv"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/syncreason"

	"github.com/softsrv/steamapi/steamapi"
	"golang.org/x/sync/errgroup"
)

// notSyncedMessageFormat names a member whose library has never been synced,
// so the shared result cannot include them.
const notSyncedMessageFormat = "games not synced yet for user %s"

// syncFailedMessageFormat names a member whose last library sync failed with
// no successful sync since, and gives the stored reason.
const syncFailedMessageFormat = "%s's game sync failed: %s"

// SharedGamesStore is the store surface the shared-games builders read.
type SharedGamesStore interface {
	GameDataStore
	GameSyncStates(context.Context, []string) (map[string]db.GameSyncState, error)
	SharedStoredGames(context.Context, string, []string) ([]db.StoredGame, error)
}

// PartyGamesService is the Steam surface the party page uses: game data for
// shared games without stored data, and live player counts.
type PartyGamesService interface {
	GameDetailsService
	GetNumberOfCurrentPlayers(context.Context, string) (int, error)
}

// storedSharedGames builds the shared multiplayer games of caller and others
// from stored libraries and stored game data. A member in private_or_no_games
// gets the no-games message. A member never synced (or unknown) gets the
// not-synced message, replaced by the sync-failed message with the stored
// reason when their last sync failed with no success since. A synced member
// takes part from their stored library whatever failure was recorded later.
// When any member has a message the shared result cannot be computed and only
// the messages are returned. Otherwise GamesDetails is
// requested (under fetchCtx) only for shared games with no stored data; a game
// still without data afterwards is left out without an error. Store reads use
// ctx. The result holds only multiplayer games, in caller library order, with
// the stored name and icon.
func storedSharedGames(ctx, fetchCtx context.Context, steam GameDetailsService, store SharedGamesStore, caller string, others []string, names map[string]string) ([]steamapi.Game, []string, error) {
	members := uniqueMembers(caller, others)
	states, err := store.GameSyncStates(ctx, members)
	if err != nil {
		return nil, nil, fmt.Errorf("shared games: sync states: %w", err)
	}
	var messages []string
	for _, steamID := range members {
		name := names[steamID]
		if name == "" {
			name = steamID
		}
		state, ok := states[steamID]
		switch {
		case !ok || state.State == db.GameSyncNeverSynced:
			// No usable library: a recorded sync failure explains why, else
			// the member has simply not been synced yet.
			if ok && syncFailedSinceSuccess(state) {
				messages = append(messages, fmt.Sprintf(syncFailedMessageFormat, name, syncreason.Scrub(state.LibrarySyncError)))
			} else {
				messages = append(messages, fmt.Sprintf(notSyncedMessageFormat, name))
			}
		case state.State == db.GameSyncPrivateOrNoGames:
			messages = append(messages, fmt.Sprintf(noGamesMessageFormat, name))
		}
	}
	if len(messages) > 0 {
		return nil, messages, nil
	}

	shared, err := store.SharedStoredGames(ctx, caller, members[1:])
	if err != nil {
		return nil, nil, fmt.Errorf("shared games: %w", err)
	}
	var missing []int
	for _, game := range shared {
		if !game.DataFetched {
			missing = append(missing, game.AppID)
		}
	}
	if len(missing) > 0 {
		// A failed fetch leaves those games without data; they are omitted below.
		if err := FetchMissingGameData(fetchCtx, steam, store, missing); err != nil {
			fmt.Printf("shared games: fetch game data: %s\n", err)
		}
		if shared, err = store.SharedStoredGames(ctx, caller, members[1:]); err != nil {
			return nil, nil, fmt.Errorf("shared games: %w", err)
		}
	}
	games := make([]steamapi.Game, 0, len(shared))
	for _, game := range shared {
		if game.DataFetched && game.Multiplayer {
			games = append(games, steamapi.Game{AppID: game.AppID, Name: game.Name, ImgIconURL: game.ImgIconURL})
		}
	}
	return games, nil, nil
}

// syncFailedSinceSuccess reports whether the member's last library sync
// failed and no successful sync has been recorded since.
func syncFailedSinceSuccess(state db.GameSyncState) bool {
	if state.LibrarySyncError == "" {
		return false
	}
	return state.LibrarySyncedAt == nil || state.LibrarySyncErrorAt == nil || state.LibrarySyncErrorAt.After(*state.LibrarySyncedAt)
}

// uniqueMembers returns caller followed by the distinct other ids.
func uniqueMembers(caller string, others []string) []string {
	members := []string{caller}
	seen := map[string]bool{caller: true}
	for _, id := range others {
		if !seen[id] {
			seen[id] = true
			members = append(members, id)
		}
	}
	return members
}

// BuildSharedGames builds the party's shared games from the stored libraries of
// the database-loaded roster, without syncing any member, and fetches a live
// player count for each shared game. A member who is private or not yet synced
// becomes a named message; store and player-count failures are returned to the
// caller without partial counts. A roster of fewer than two members has nothing
// to share, so it returns nothing before any store read or Steam call.
func BuildSharedGames(ctx context.Context, steam PartyGamesService, store SharedGamesStore, members []db.PartyMember) ([]component.SharedGameCount, []string, error) {
	if len(members) < 2 {
		return nil, nil, nil
	}
	ids := make([]string, len(members))
	names := make(map[string]string, len(members))
	for i, member := range members {
		ids[i] = member.SteamID64
		names[member.SteamID64] = member.Name
	}
	games, messages, err := storedSharedGames(ctx, ctx, steam, store, ids[0], ids[1:], names)
	if err != nil {
		return nil, nil, fmt.Errorf("party shared games: %w", err)
	}
	if len(messages) > 0 {
		return nil, append([]string{"Shared games cannot be computed."}, messages...), nil
	}
	counts := make([]component.SharedGameCount, len(games))
	fetches, countCtx := errgroup.WithContext(ctx)
	for i, game := range games {
		fetches.Go(func() error {
			count, err := steam.GetNumberOfCurrentPlayers(countCtx, strconv.Itoa(game.AppID))
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
