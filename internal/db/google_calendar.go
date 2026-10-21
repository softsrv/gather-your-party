package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GoogleCalendarConnection holds the persisted OAuth tokens and identity
// for a user's connected Google Calendar.
type GoogleCalendarConnection struct {
	UserID       int64
	Email        string
	RefreshToken string
	AccessToken  string
	TokenExpiry  time.Time
}

// SaveGoogleCalendarConnection upserts a connection keyed to userID.
// It scopes to WHERE / ON CONFLICT (user_id) so one user's connection
// never overwrites another user's row.
func (s *Store) SaveGoogleCalendarConnection(ctx context.Context, userID int64, c GoogleCalendarConnection) error {
	q, done := s.querier(ctx)
	defer done()

	_, err := q.Exec(ctx, `
		INSERT INTO google_calendar_connections (user_id, email, refresh_token, access_token, token_expiry, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (user_id) DO UPDATE SET
			email = EXCLUDED.email,
			refresh_token = EXCLUDED.refresh_token,
			access_token = EXCLUDED.access_token,
			token_expiry = EXCLUDED.token_expiry,
			updated_at = now()`,
		userID, c.Email, c.RefreshToken, c.AccessToken, c.TokenExpiry,
	)
	if err != nil {
		return fmt.Errorf("save google calendar connection: %w", err)
	}
	return nil
}

// GoogleCalendarConnection loads the stored Google Calendar connection for userID.
// Returns found=false when no connection exists.
func (s *Store) GoogleCalendarConnection(ctx context.Context, userID int64) (GoogleCalendarConnection, bool, error) {
	q, done := s.querier(ctx)
	defer done()

	var conn GoogleCalendarConnection
	var tokenExpiry *time.Time
	err := q.QueryRow(ctx, `
		SELECT user_id, email, refresh_token, access_token, token_expiry
		FROM google_calendar_connections
		WHERE user_id = $1`, userID).Scan(
		&conn.UserID, &conn.Email, &conn.RefreshToken, &conn.AccessToken, &tokenExpiry,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return GoogleCalendarConnection{}, false, nil
	}
	if err != nil {
		return GoogleCalendarConnection{}, false, fmt.Errorf("google calendar connection: %w", err)
	}
	if tokenExpiry != nil {
		conn.TokenExpiry = *tokenExpiry
	}
	return conn, true, nil
}

// DeleteGoogleCalendarConnection removes the stored Google Calendar connection for userID.
// It returns the deleted connection row, or found=false if no row existed.
func (s *Store) DeleteGoogleCalendarConnection(ctx context.Context, userID int64) (GoogleCalendarConnection, bool, error) {
	q, done := s.querier(ctx)
	defer done()

	var conn GoogleCalendarConnection
	var tokenExpiry *time.Time
	err := q.QueryRow(ctx, `
		DELETE FROM google_calendar_connections
		WHERE user_id = $1
		RETURNING user_id, email, refresh_token, access_token, token_expiry`, userID).Scan(
		&conn.UserID, &conn.Email, &conn.RefreshToken, &conn.AccessToken, &tokenExpiry,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return GoogleCalendarConnection{}, false, nil
	}
	if err != nil {
		return GoogleCalendarConnection{}, false, fmt.Errorf("delete google calendar connection: %w", err)
	}
	if tokenExpiry != nil {
		conn.TokenExpiry = *tokenExpiry
	}
	return conn, true, nil
}
