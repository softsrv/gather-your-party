// Package db provides the persistence write paths for users and sessions.
package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

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

func newSessionToken() (string, error) {
	var entropy [32]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(entropy[:]), nil
}
