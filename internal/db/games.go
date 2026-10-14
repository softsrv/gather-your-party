package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"gather-your-party/internal/syncreason"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Game-sync states stored in users.game_sync_state.
const (
	GameSyncNeverSynced      = "never_synced"
	GameSyncSynced           = "synced"
	GameSyncPrivateOrNoGames = "private_or_no_games"
)

// GameSyncState is a user's stored library-sync state. At is nil until the
// first sync records a state. LibrarySyncedAt is the last successful page sync
// (nil when there has been none); LibrarySyncError is the reason of the last
// failed sync since then (empty when none) and LibrarySyncErrorAt its time.
type GameSyncState struct {
	SteamID64          string
	UserID             int64
	State              string
	At                 *time.Time
	LibrarySyncedAt    *time.Time
	LibrarySyncError   string
	LibrarySyncErrorAt *time.Time
}

// librarySyncInterval is how long a successful library sync stays fresh.
const librarySyncInterval = 24 * time.Hour

// userSyncLockNamespace is the first key of the two-key advisory lock that
// serializes library syncs of one user; the second key is the user id.
const userSyncLockNamespace int32 = 0x4c696273 // "Libs"

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
		SELECT steam_id_64, id, game_sync_state, game_sync_state_at,
			library_synced_at, COALESCE(library_sync_error, ''), library_sync_error_at
		FROM users WHERE steam_id_64 = ANY($1::text[])`, steamID64s)
	if err != nil {
		return nil, fmt.Errorf("game sync states: %w", err)
	}
	defer rows.Close()
	states := make(map[string]GameSyncState)
	for rows.Next() {
		var state GameSyncState
		if err := rows.Scan(&state.SteamID64, &state.UserID, &state.State, &state.At,
			&state.LibrarySyncedAt, &state.LibrarySyncError, &state.LibrarySyncErrorAt); err != nil {
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
// keeps its first position. Games are written set-based (two statements over
// unnested arrays) in app id order so concurrent syncs with overlapping
// libraries lock in the same order, and a transaction that still loses a
// deadlock or serialization race is retried. Stored games missing from games
// are kept.
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
	appIDs := make([]int32, len(entries))
	names := make([]string, len(entries))
	icons := make([]string, len(entries))
	positions := make([]int32, len(entries))
	for i, e := range entries {
		appIDs[i] = int32(e.game.AppID)
		names[i] = e.game.Name
		icons[i] = e.game.ImgIconURL
		positions[i] = int32(e.position)
	}

	var err error
	for attempt := 0; attempt < storeLibraryAttempts; attempt++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				INSERT INTO games (app_id, name, img_icon_url)
				SELECT app_id, name, img_icon_url
				FROM unnest($1::integer[], $2::text[], $3::text[]) AS g(app_id, name, img_icon_url)
				ORDER BY app_id
				ON CONFLICT (app_id) DO UPDATE SET
					name = EXCLUDED.name,
					img_icon_url = EXCLUDED.img_icon_url`, appIDs, names, icons); err != nil {
				return fmt.Errorf("upsert games: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_games (user_id, game_id, position)
				SELECT $1, g.id, l.position
				FROM unnest($2::integer[], $3::integer[]) AS l(app_id, position)
				JOIN games g ON g.app_id = l.app_id
				ORDER BY g.id
				ON CONFLICT (user_id, game_id) DO UPDATE SET position = EXCLUDED.position`,
				userID, appIDs, positions); err != nil {
				return fmt.Errorf("upsert user games: %w", err)
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

// LibrarySyncDue reports whether the user has no successful library sync or
// the last one is older than 24 hours. An unknown user is due.
func (s *Store) LibrarySyncDue(ctx context.Context, userID int64) (bool, error) {
	var due bool
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT library_synced_at IS NULL OR library_synced_at < now() - $2::interval FROM users WHERE id = $1),
			true)`, userID, librarySyncInterval).Scan(&due)
	if err != nil {
		return false, fmt.Errorf("library sync due: %w", err)
	}
	return due, nil
}

// RecordSyncSuccess stamps a successful library sync and clears any stored
// failure.
func (s *Store) RecordSyncSuccess(ctx context.Context, userID int64) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE users SET library_synced_at = now(), library_sync_error = NULL, library_sync_error_at = NULL
		WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("record sync success: %w", err)
	}
	return nil
}

// RecordSyncFailure stores the reason of a failed library sync. The last
// success stamp is left unchanged. The reason is scrubbed of URL query strings
// and key parameters before it is stored.
func (s *Store) RecordSyncFailure(ctx context.Context, userID int64, reason string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE users SET library_sync_error = $2, library_sync_error_at = now()
		WHERE id = $1`, userID, syncreason.Scrub(reason)); err != nil {
		return fmt.Errorf("record sync failure: %w", err)
	}
	return nil
}

// WithUserSyncLock runs fn while holding the user's library-sync lock: a
// session-level advisory lock on a dedicated pool connection. It blocks while
// another session holds the lock, until ctx is done. The lock lives on the
// connection, so a connection (or instance) that dies frees it. If the unlock
// fails the connection is closed rather than returned to the pool.
func (s *Store) WithUserSyncLock(ctx context.Context, userID int64, fn func(ctx context.Context) error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("user sync lock: acquire connection: %w", err)
	}
	defer conn.Release()
	key := int32(userID) ^ int32(userID>>32)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, userSyncLockNamespace, key); err != nil {
		// A cancelled lock wait may leave the connection in an unknown state.
		closeConn(conn)
		return fmt.Errorf("user sync lock: lock: %w", err)
	}
	fnErr := fn(ctx)
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRow(unlockCtx, `SELECT pg_advisory_unlock($1, $2)`, userSyncLockNamespace, key).Scan(&unlocked); err != nil || !unlocked {
		closeConn(conn)
	}
	return fnErr
}

// closeConn closes the connection so the pool discards it on Release, freeing
// any session-level lock it held.
func closeConn(conn *pgxpool.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Conn().Close(closeCtx)
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
