package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PartyEvent is a scheduled party event. StartAt and EndAt are absolute
// instants; GameName always comes from the games row the event references.
type PartyEvent struct {
	ID        string
	GameAppID int
	GameName  string
	StartAt   time.Time
	EndAt     time.Time
}

// ErrUnknownGame is returned by CreatePartyEvent when no games row has the app ID.
var ErrUnknownGame = errors.New("unknown game")

// CreatePartyEvent saves one event for the party. The game is referenced by its
// games row, resolved from the Steam app ID; the instants are stored as
// timestamptz, so their zone is irrelevant.
func (s *Store) CreatePartyEvent(ctx context.Context, partyID string, appID int, startAt, endAt time.Time) error {
	q, done := s.querier(ctx)
	defer done()
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO party_events (party_id, game_id, start_at, end_at)
		SELECT $1, g.id, $3, $4 FROM games g WHERE g.app_id = $2
		RETURNING id::text`, partyID, appID, startAt.UTC(), endAt.UTC()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("create party event: %w", ErrUnknownGame)
	}
	if err != nil {
		return fmt.Errorf("create party event: %w", err)
	}
	return nil
}

// UpcomingPartyEvents returns the party's events whose end is later than now,
// earliest start first, each with the stored game's name.
func (s *Store) UpcomingPartyEvents(ctx context.Context, partyID string, now time.Time) ([]PartyEvent, error) {
	q, done := s.querier(ctx)
	defer done()
	rows, err := q.Query(ctx, `
		SELECT e.id::text, g.app_id, g.name, e.start_at, e.end_at
		FROM party_events e
		JOIN games g ON g.id = e.game_id
		WHERE e.party_id = $1 AND e.end_at > $2
		ORDER BY e.start_at, e.id`, partyID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("upcoming party events: %w", err)
	}
	defer rows.Close()
	events := make([]PartyEvent, 0)
	for rows.Next() {
		var event PartyEvent
		if err := rows.Scan(&event.ID, &event.GameAppID, &event.GameName, &event.StartAt, &event.EndAt); err != nil {
			return nil, fmt.Errorf("upcoming party events: scan: %w", err)
		}
		event.StartAt, event.EndAt = event.StartAt.UTC(), event.EndAt.UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upcoming party events: %w", err)
	}
	return events, nil
}
