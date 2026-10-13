package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/softsrv/steamapi/steamapi"
)

// Game-sync states stored in users.game_sync_state.
const (
	GameSyncNeverSynced      = "never_synced"
	GameSyncSynced           = "synced"
	GameSyncPrivateOrNoGames = "private_or_no_games"
)

// GameSyncState is a user's stored library-sync state. At is nil until the
// first sync records a state.
type GameSyncState struct {
	SteamID64 string
	UserID    int64
	State     string
	At        *time.Time
}

// GameData is the per-app detail fetched after a library sync.
type GameData struct {
	AppID       int
	HasData     bool
	Multiplayer bool
}

// StoredGame is a game from a stored library.
type StoredGame struct {
	AppID       int
	Name        string
	ImgIconURL  string
	DataFetched bool
	HasData     bool
	Multiplayer bool
}

// storeLibraryAttempts bounds retries of a library write that lost a deadlock
// or serialization race with a concurrent sync.
const storeLibraryAttempts = 5

// GameSyncStates returns the stored sync state of each known user. Unknown
// SteamID64s are absent from the map.
func (s *Store) GameSyncStates(ctx context.Context, steamID64s []string) (map[string]GameSyncState, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT steam_id_64, id, game_sync_state, game_sync_state_at
		FROM users WHERE steam_id_64 = ANY($1::text[])`, steamID64s)
	if err != nil {
		return nil, fmt.Errorf("game sync states: %w", err)
	}
	defer rows.Close()
	states := make(map[string]GameSyncState)
	for rows.Next() {
		var state GameSyncState
		if err := rows.Scan(&state.SteamID64, &state.UserID, &state.State, &state.At); err != nil {
			return nil, fmt.Errorf("game sync states: scan: %w", err)
		}
		states[state.SteamID64] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("game sync states: rows: %w", err)
	}
	return states, nil
}

// StoreUserLibrary upserts the user's owned games and marks the user synced, in
// one transaction. Position is the game's index in games; a repeated app id
// keeps its first position. Rows are written in app id order so concurrent
// syncs with overlapping libraries lock in the same order, and a transaction
// that still loses a deadlock or serialization race is retried.
func (s *Store) StoreUserLibrary(ctx context.Context, userID int64, games []steamapi.Game) error {
	type entry struct {
		game     steamapi.Game
		position int
	}
	seen := make(map[int]bool, len(games))
	entries := make([]entry, 0, len(games))
	for i, game := range games {
		if seen[game.AppID] {
			continue
		}
		seen[game.AppID] = true
		entries = append(entries, entry{game: game, position: i})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].game.AppID < entries[j].game.AppID })

	var err error
	for attempt := 0; attempt < storeLibraryAttempts; attempt++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			for _, e := range entries {
				var gameID int64
				if err := tx.QueryRow(ctx, `
					INSERT INTO games (app_id, name, img_icon_url) VALUES ($1, $2, $3)
					ON CONFLICT (app_id) DO UPDATE SET
						name = EXCLUDED.name,
						img_icon_url = EXCLUDED.img_icon_url
					RETURNING id`, e.game.AppID, e.game.Name, e.game.ImgIconURL).Scan(&gameID); err != nil {
					return fmt.Errorf("upsert game %d: %w", e.game.AppID, err)
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO user_games (user_id, game_id, position) VALUES ($1, $2, $3)
					ON CONFLICT (user_id, game_id) DO UPDATE SET position = EXCLUDED.position`,
					userID, gameID, e.position); err != nil {
					return fmt.Errorf("upsert user game %d: %w", e.game.AppID, err)
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE users SET game_sync_state = $2, game_sync_state_at = now() WHERE id = $1`,
				userID, GameSyncSynced); err != nil {
				return fmt.Errorf("set sync state: %w", err)
			}
			return nil
		})
		if err == nil || !retryableTxError(err) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("store user library: %w", err)
	}
	return nil
}

// RecordEmptyLibrary handles a sync whose owned list came back empty. A user
// with no stored games becomes private_or_no_games; a user with stored games
// keeps every row and stays synced. It returns the resulting state.
func (s *Store) RecordEmptyLibrary(ctx context.Context, userID int64) (string, error) {
	var state string
	err := s.pool.QueryRow(ctx, `
		UPDATE users SET
			game_sync_state = CASE WHEN EXISTS (SELECT 1 FROM user_games WHERE user_id = $1) THEN $2 ELSE $3 END,
			game_sync_state_at = now()
		WHERE id = $1
		RETURNING game_sync_state`, userID, GameSyncSynced, GameSyncPrivateOrNoGames).Scan(&state)
	if err != nil {
		return "", fmt.Errorf("record empty library: %w", err)
	}
	return state, nil
}

// GamesMissingData returns, in input order, the app ids whose games row exists
// but whose details have not been fetched.
func (s *Store) GamesMissingData(ctx context.Context, appIDs []int) ([]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT app_id FROM games WHERE app_id = ANY($1::integer[]) AND details_fetched_at IS NULL`, appIDs)
	if err != nil {
		return nil, fmt.Errorf("games missing data: %w", err)
	}
	defer rows.Close()
	missing := make(map[int]bool)
	for rows.Next() {
		var appID int
		if err := rows.Scan(&appID); err != nil {
			return nil, fmt.Errorf("games missing data: scan: %w", err)
		}
		missing[appID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("games missing data: rows: %w", err)
	}
	result := make([]int, 0, len(missing))
	for _, appID := range appIDs {
		if missing[appID] {
			result = append(result, appID)
			delete(missing, appID)
		}
	}
	return result, nil
}

// StoreGameData records fetched details for existing games.
func (s *Store) StoreGameData(ctx context.Context, data []GameData) error {
	if len(data) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, d := range data {
		batch.Queue(`
			UPDATE games SET details_fetched_at = now(), has_data = $2, multiplayer = $3
			WHERE app_id = $1`, d.AppID, d.HasData, d.Multiplayer)
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("store game data: %w", err)
	}
	return nil
}

// UserGames returns the user's stored library in position order, then app id.
func (s *Store) UserGames(ctx context.Context, steamID64 string) ([]StoredGame, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.app_id, g.name, g.img_icon_url, g.details_fetched_at IS NOT NULL, g.has_data, g.multiplayer
		FROM users u
		JOIN user_games ug ON ug.user_id = u.id
		JOIN games g ON g.id = ug.game_id
		WHERE u.steam_id_64 = $1
		ORDER BY ug.position, g.app_id`, steamID64)
	if err != nil {
		return nil, fmt.Errorf("user games: %w", err)
	}
	return scanStoredGames(rows, "user games")
}

// SharedStoredGames returns the caller's stored games that every other user
// also owns, in the caller's position order. With no other users it returns the
// caller's library.
func (s *Store) SharedStoredGames(ctx context.Context, callerSteamID64 string, otherSteamID64s []string) ([]StoredGame, error) {
	others := uniqueStrings(otherSteamID64s)
	rows, err := s.pool.Query(ctx, `
		SELECT g.app_id, g.name, g.img_icon_url, g.details_fetched_at IS NOT NULL, g.has_data, g.multiplayer
		FROM users u
		JOIN user_games ug ON ug.user_id = u.id
		JOIN games g ON g.id = ug.game_id
		WHERE u.steam_id_64 = $1
			AND (
				SELECT count(DISTINCT o.id) FROM users o
				JOIN user_games og ON og.user_id = o.id
				WHERE o.steam_id_64 = ANY($2::text[]) AND og.game_id = g.id
			) = cardinality($2::text[])
		ORDER BY ug.position, g.app_id`, callerSteamID64, others)
	if err != nil {
		return nil, fmt.Errorf("shared stored games: %w", err)
	}
	return scanStoredGames(rows, "shared stored games")
}

func scanStoredGames(rows pgx.Rows, op string) ([]StoredGame, error) {
	defer rows.Close()
	games := make([]StoredGame, 0)
	for rows.Next() {
		var game StoredGame
		if err := rows.Scan(&game.AppID, &game.Name, &game.ImgIconURL, &game.DataFetched, &game.HasData, &game.Multiplayer); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", op, err)
		}
		games = append(games, game)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: rows: %w", op, err)
	}
	return games, nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			unique = append(unique, v)
		}
	}
	return unique
}

// retryableTxError reports a deadlock (40P01) or serialization failure (40001),
// after which the whole transaction can safely be retried.
func retryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}
