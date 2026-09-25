// Package db provides the persistence write paths for users and sessions.
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

// ErrStepDownNotAllowed indicates that leadership cannot be handed to this member.
var ErrStepDownNotAllowed = errors.New("step down not allowed")

// CreateParty makes the creator the leader and sole member atomically.
func (s *Store) CreateParty(ctx context.Context, name string, leaderUserID int64) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin create party: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // Commit closes the transaction on success.

	var partyID string
	if err := tx.QueryRow(ctx, `INSERT INTO parties (name, leader_id) VALUES ($1, $2) RETURNING id::text`, name, leaderUserID).Scan(&partyID); err != nil {
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

// LeaveParty removes only the acting user's membership. Lifecycle writers lock
// the party first, serializing leave and step down decisions for that party.
// Future membership writers must use the same party-first locking order.
func (s *Store) LeaveParty(ctx context.Context, partyID string, actingUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin leave party: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // Commit closes the transaction on success.

	var leaderID int64
	err = tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&leaderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock party for leave: %w", err)
	}
	removed, err := tx.Exec(ctx, `DELETE FROM memberships WHERE party_id = $1 AND user_id = $2`, partyID, actingUserID)
	if err != nil {
		return fmt.Errorf("leave party: %w", err)
	}
	// A non-member (including a repeated request) cannot change the party.
	if removed.RowsAffected() == 0 {
		return nil
	}
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE party_id = $1`, partyID).Scan(&remaining); err != nil {
		return fmt.Errorf("count remaining memberships: %w", err)
	}
	if remaining == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM parties WHERE id = $1`, partyID); err != nil {
			return fmt.Errorf("remove empty party: %w", err)
		}
	} else if leaderID == actingUserID {
		// Earliest seniority decides automatic succession, with a stable tie-break.
		if _, err := tx.Exec(ctx, `UPDATE parties SET leader_id = (
			SELECT user_id FROM memberships WHERE party_id = $1 ORDER BY joined_at, user_id LIMIT 1
		) WHERE id = $1`, partyID); err != nil {
			return fmt.Errorf("automatic succession: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit leave party: %w", err)
	}
	return nil
}

// StepDown transfers leadership to the caller-chosen member without removing
// the former leader's membership. It is unavailable in a party of one.
func (s *Store) StepDown(ctx context.Context, partyID string, actingLeaderUserID, targetUserID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin step down: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // Commit closes the transaction on success.

	var leaderID int64
	err = tx.QueryRow(ctx, `SELECT leader_id FROM parties WHERE id = $1 FOR UPDATE`, partyID).Scan(&leaderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStepDownNotAllowed
	}
	if err != nil {
		return fmt.Errorf("lock party for step down: %w", err)
	}
	if leaderID != actingLeaderUserID {
		return ErrStepDownNotAllowed
	}
	var count int
	var targetIsMember bool
	if err := tx.QueryRow(ctx, `SELECT count(*), EXISTS (
		SELECT 1 FROM memberships WHERE party_id = $1 AND user_id = $2
	) FROM memberships WHERE party_id = $1`, partyID, targetUserID).Scan(&count, &targetIsMember); err != nil {
		return fmt.Errorf("check step down membership: %w", err)
	}
	if count <= 1 || !targetIsMember {
		return ErrStepDownNotAllowed
	}
	if _, err := tx.Exec(ctx, `UPDATE parties SET leader_id = $2 WHERE id = $1`, partyID, targetUserID); err != nil {
		return fmt.Errorf("step down: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit step down: %w", err)
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
