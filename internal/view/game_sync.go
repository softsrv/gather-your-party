package view

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

// gamesDetailsBatchSize is the maximum number of app IDs GamesDetails accepts
// per call (the batched steamapi endpoint errors above this).
const gamesDetailsBatchSize = 10

// gamesDetailsMaxConcurrency bounds the number of GamesDetails batches in
// flight at once.
const gamesDetailsMaxConcurrency = 10

// gameDataStoreTimeout bounds the write of one returned details batch. The
// write runs on a context detached from the caller's cancellation, so a batch
// that returned before a sync budget expired is still persisted.
const gameDataStoreTimeout = 5 * time.Second

// GameDetailsService is the Steam surface needed to fetch game data.
type GameDetailsService interface {
	GamesDetails(context.Context, []int) ([]steamapi.GameDetail, error)
}

// GameSyncService is the Steam surface needed to sync a user's library.
type GameSyncService interface {
	GameDetailsService
	Games(context.Context, string, bool, bool) ([]steamapi.Game, error)
}

// SteamService is every Steam call the shared-games views make.
// *steamapi.Client satisfies it.
type SteamService interface {
	GameSyncService
	Friends(context.Context, string) ([]steamapi.Friend, error)
	Players(context.Context, []string) ([]steamapi.Player, error)
	GetNumberOfCurrentPlayers(context.Context, string) (int, error)
}

// GameDataStore is the store surface needed to fetch and record game data.
type GameDataStore interface {
	GamesMissingData(context.Context, []int) ([]int, error)
	StoreGameData(context.Context, []db.GameData) error
}

// GameSyncStore is the store surface needed to sync a user's library.
type GameSyncStore interface {
	GameDataStore
	StoreUserLibrary(context.Context, int64, []steamapi.Game) error
	RecordEmptyLibrary(context.Context, int64) (string, error)
}

// SyncUserGames fetches the user's owned library from Steam and stores it. An
// empty list is recorded through RecordEmptyLibrary (a user with no stored
// games becomes private_or_no_games). A non-empty list is stored with each
// game's name and icon, then game data is fetched for its games that have none.
func SyncUserGames(ctx context.Context, steam GameSyncService, store GameSyncStore, userID int64, steamID64 string) error {
	games, err := steam.Games(ctx, steamID64, true, true)
	if err != nil {
		return fmt.Errorf("sync games %s: %w", steamID64, err)
	}
	if len(games) == 0 {
		fmt.Printf("sync games: empty owned-games list for %s\n", steamID64)
		if _, err := store.RecordEmptyLibrary(ctx, userID); err != nil {
			return fmt.Errorf("sync games %s: %w", steamID64, err)
		}
		return nil
	}
	if err := store.StoreUserLibrary(ctx, userID, games); err != nil {
		return fmt.Errorf("sync games %s: %w", steamID64, err)
	}
	appIDs := make([]int, len(games))
	for i, game := range games {
		appIDs[i] = game.AppID
	}
	if err := FetchMissingGameData(ctx, steam, store, appIDs); err != nil {
		return fmt.Errorf("sync games %s: %w: %w", steamID64, ErrGameDetails, err)
	}
	return nil
}

// ErrGameDetails marks a SyncUserGames error raised only by the game-data
// fetch after the library was already stored; callers may treat the library
// sync as successful.
var ErrGameDetails = errors.New("fetch game details")

// FetchMissingGameData requests GamesDetails only for the given app ids whose
// games row has no stored data, in batches of gamesDetailsBatchSize with at
// most gamesDetailsMaxConcurrency batches in flight. Each batch is stored as
// soon as it returns: every returned detail is stored with its multiplayer
// flag, and every requested id the response omitted is stored as having no
// data and not multiplayer. A batch that errors or times out stores nothing, so
// a later call requests its ids again. One batch's failure never stops or
// undoes another's; the returned error joins every batch failure.
func FetchMissingGameData(ctx context.Context, steam GameDetailsService, store GameDataStore, appIDs []int) error {
	if len(appIDs) == 0 {
		return nil
	}
	missing, err := store.GamesMissingData(ctx, appIDs)
	if err != nil {
		return fmt.Errorf("fetch game data: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}

	batches := make([][]int, 0, (len(missing)+gamesDetailsBatchSize-1)/gamesDetailsBatchSize)
	for start := 0; start < len(missing); start += gamesDetailsBatchSize {
		end := min(start+gamesDetailsBatchSize, len(missing))
		batches = append(batches, missing[start:end])
	}

	errs := make([]error, len(batches))
	slots := make(chan struct{}, gamesDetailsMaxConcurrency)
	var wg sync.WaitGroup
	for i, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A panic in this goroutine would end the process; it is this
			// batch's failure instead, so no data is stored for its ids and a
			// later call requests them again.
			defer recoverToError(&errs[i], "fetch game data")
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				errs[i] = fmt.Errorf("fetch game data: %w", ctx.Err())
				return
			}
			defer func() { <-slots }()
			// Each worker owns its slot; completion order cannot swap errors.
			errs[i] = fetchAndStoreBatch(ctx, steam, store, batch)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// recoverToError is deferred at the top of a goroutine: it turns a panic into
// *dst (and logs it) so the panic cannot terminate the process, which a recover
// in the spawning goroutine would not prevent.
func recoverToError(dst *error, what string) {
	if rec := recover(); rec != nil {
		*dst = fmt.Errorf("%s: panic: %v", what, rec)
		fmt.Printf("%s\n", *dst)
	}
}

func fetchAndStoreBatch(ctx context.Context, steam GameDetailsService, store GameDataStore, batch []int) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fetch game data: %w", err)
	}
	details, err := steam.GamesDetails(ctx, batch)
	if err != nil {
		return fmt.Errorf("fetch game data: details: %w", err)
	}
	byAppID := make(map[int]steamapi.GameDetail, len(details))
	for _, detail := range details {
		byAppID[detail.AppID] = detail
	}
	data := make([]db.GameData, len(batch))
	for i, appID := range batch {
		detail, ok := byAppID[appID]
		data[i] = db.GameData{AppID: appID, HasData: ok, Multiplayer: ok && detail.Multiplayer}
	}
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gameDataStoreTimeout)
	defer cancel()
	if err := store.StoreGameData(storeCtx, data); err != nil {
		return fmt.Errorf("fetch game data: %w", err)
	}
	return nil
}
