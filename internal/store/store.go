package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a session token is absent or expired.
var ErrNotFound = errors.New("not found")

// User mirrors the users table.
type User struct {
	ID        int64
	SteamID64 string
	Persona   string
	AvatarURL string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Session mirrors the sessions table.
type Session struct {
	Token     string
	UserID    int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Store provides access to the Postgres persistence layer.
// The pool is injected by the caller (main.go) — no pool is ever
// constructed inside this package.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store with the given connection pool.
// The pool must already be open; Store does not own its lifecycle.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// UpsertUser inserts a user row keyed on steamid64, or updates persona
// and avatar_url on conflict. Returns the resulting user row.
//
// CLM-9: satisfies the upsert-keyed-on-steamid64 requirement.
func (s *Store) UpsertUser(ctx context.Context, steamID64, persona, avatarURL string) (User, error) {
	const q = `
		INSERT INTO users (steamid64, persona, avatar_url)
		VALUES ($1, $2, $3)
		ON CONFLICT (steamid64) DO UPDATE
			SET persona     = EXCLUDED.persona,
			    avatar_url  = EXCLUDED.avatar_url,
			    updated_at  = now()
		RETURNING id, steamid64, persona, avatar_url, created_at, updated_at`

	rows, err := s.pool.Query(ctx, q, steamID64, persona, avatarURL)
	if err != nil {
		return User{}, err
	}
	return pgx.CollectOneRow(rows, func(row pgx.CollectableRow) (User, error) {
		var u User
		err := row.Scan(&u.ID, &u.SteamID64, &u.Persona, &u.AvatarURL, &u.CreatedAt, &u.UpdatedAt)
		return u, err
	})
}

// CreateSession inserts a new session row with an opaque token and an
// expiry set 30 days from now. Returns the inserted session.
//
// CLM-10: satisfies the create-session-with-30-day-expiry requirement.
func (s *Store) CreateSession(ctx context.Context, token string, userID int64) (Session, error) {
	expiry := time.Now().Add(30 * 24 * time.Hour)
	const q = `
		INSERT INTO sessions (token, user_id, expires_at)
		VALUES ($1, $2, $3)
		RETURNING token, user_id, expires_at, created_at`

	rows, err := s.pool.Query(ctx, q, token, userID, expiry)
	if err != nil {
		return Session{}, err
	}
	return pgx.CollectOneRow(rows, func(row pgx.CollectableRow) (Session, error) {
		var sess Session
		err := row.Scan(&sess.Token, &sess.UserID, &sess.ExpiresAt, &sess.CreatedAt)
		return sess, err
	})
}

// GetSteamIDByToken looks up an unexpired session by its token and
// returns the associated user's SteamID64.
//
// If the token is absent OR expired, ErrNotFound is returned so the
// caller treats the request as signed-out (CLM-11, CLM-12).
// The SQL WHERE clause `expires_at > now()` is the enforcement point
// for CLM-12's polarity-forbid: an expired token yields no row.
func (s *Store) GetSteamIDByToken(ctx context.Context, token string) (string, error) {
	const q = `
		SELECT u.steamid64
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token = $1
		  AND s.expires_at > now()`

	var steamID64 string
	err := s.pool.QueryRow(ctx, q, token).Scan(&steamID64)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return steamID64, nil
}

// DeleteSession removes the session row with the given token.
// After deletion, GetSteamIDByToken for the same token returns ErrNotFound.
//
// CLM-13: satisfies the delete-session requirement.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	const q = `DELETE FROM sessions WHERE token = $1`
	_, err := s.pool.Exec(ctx, q, token)
	return err
}
