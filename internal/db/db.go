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

func newSessionToken() (string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(entropy[:]), nil
}
