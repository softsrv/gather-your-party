// Package db provides persistence for users, sessions, and parties.
package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Store uses a pool owned and closed by the application.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertUser persists a profile for a caller-verified SteamID64. The caller must
// verify ownership before calling; Player.SteamID is never used as identity.
func (s *Store) UpsertUser(ctx context.Context, verifiedSteamID64 string, player steamapi.Player) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (steam_id_64) DO UPDATE SET
			persona_name = EXCLUDED.persona_name,
			avatar_small = EXCLUDED.avatar_small,
			avatar_medium = EXCLUDED.avatar_medium,
			avatar_full = EXCLUDED.avatar_full,
			updated_at = now()
		RETURNING id`, verifiedSteamID64, player.PersonaName, player.AvatarSmall, player.AvatarMedium, player.AvatarFull).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert user: %w", err)
	}
	return id, nil
}

// ResolveUserID turns a verified SteamID64 into its users.id. Because
// steam_id_64 is UNIQUE, a known SteamID64 yields exactly one id; an unknown
// one returns pgx.ErrNoRows-derived not-found (mirroring ResolveSession).
func (s *Store) ResolveUserID(ctx context.Context, steamID64 string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE steam_id_64 = $1`, steamID64).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve user id: %w", err)
	}
	return id, true, nil
}

// CreateSession creates a seven-day session with a 256-bit random opaque token.
// A token is returned only after its database row has been stored successfully.
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO sessions (token, user_id, expires_at, created_at)
		VALUES ($1, $2, now() + interval '7 days', now())`, token, userID)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// ResolveSession refreshes an unexpired session and returns its verified identity.
func (s *Store) ResolveSession(ctx context.Context, token string) (string, bool, error) {
	var steamID64 string
	err := s.pool.QueryRow(ctx, `
		UPDATE sessions s SET expires_at = now() + interval '7 days'
		FROM users u
		WHERE s.token = $1 AND s.expires_at > now() AND s.user_id = u.id
		RETURNING u.steam_id_64`, token).Scan(&steamID64)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve session: %w", err)
	}
	return steamID64, true, nil
}

// DeleteSession invalidates a session; an unknown token is already invalidated.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token = $1`, token); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// CreateParty atomically makes the creator the leader and sole member of a new party.
func (s *Store) CreateParty(ctx context.Context, leaderUserID int64, name string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin create party: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var partyID string
	if err := tx.QueryRow(ctx, `INSERT INTO parties (leader_id, name) VALUES ($1, $2) RETURNING id`, leaderUserID, name).Scan(&partyID); err != nil {
		return "", fmt.Errorf("create party: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, partyID, leaderUserID); err != nil {
		return "", fmt.Errorf("create leader membership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit create party: %w", err)
	}
	return partyID, nil
}

// StepDown hands leadership to a current member without removing the former leader.
func (s *Store) StepDown(ctx context.Context, partyID string, actingUserID int64, targetUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin step down: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialize leadership changes with LeaveParty before checking membership.
	var leaderID int64
	if err := tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&leaderID); err != nil {
		return fmt.Errorf("get step down leader: %w", err)
	}
	if leaderID != actingUserID {
		return errors.New("step down: only the current leader can transfer leadership")
	}
	var memberCount int64
	var targetIsMember bool
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE user_id = $2) > 0 FROM memberships WHERE party_id = $1`, partyID, targetUserID).Scan(&memberCount, &targetIsMember); err != nil {
		return fmt.Errorf("get step down members: %w", err)
	}
	if memberCount <= 1 {
		return errors.New("step down: another member is required")
	}
	if !targetIsMember {
		return errors.New("step down: target must be a current party member")
	}
	if _, err := tx.Exec(ctx, `UPDATE parties SET leader_id = $2 WHERE id = $1`, partyID, targetUserID); err != nil {
		return fmt.Errorf("step down: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit step down: %w", err)
	}
	return nil
}

// LeaveParty removes only the acting user's membership and, if they led the
// party, transfers leadership to the earliest-joined remaining member.
// Empty-party cleanup is handled separately.
func (s *Store) LeaveParty(ctx context.Context, partyID string, actingUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin leave party: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var leaderID int64
	err = tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&leaderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get leave party leader: %w", err)
	}
	removed, err := tx.Exec(ctx, `DELETE FROM memberships WHERE party_id = $1 AND user_id = $2`, partyID, actingUserID)
	if err != nil {
		return fmt.Errorf("leave party: %w", err)
	}
	if leaderID == actingUserID && removed.RowsAffected() > 0 {
		var successorID int64
		err := tx.QueryRow(ctx, `SELECT user_id FROM memberships WHERE party_id = $1 ORDER BY joined_at, user_id LIMIT 1`, partyID).Scan(&successorID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get leave party successor: %w", err)
		}
		if err == nil {
			if _, err := tx.Exec(ctx, `UPDATE parties SET leader_id = $2 WHERE id = $1`, partyID, successorID); err != nil {
				return fmt.Errorf("set leave party successor: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit leave party: %w", err)
	}
	return nil
}

func newSessionToken() (string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(entropy[:]), nil
}
