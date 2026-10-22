package main

import (
	"context"
	"time"

	"gather-your-party/internal/db"
)

// The view.LibrarySyncStore methods of the session-store double: no library
// sync is ever due, and the sync lock runs fn directly.

func (s *fakeSessionStore) LibrarySyncDue(context.Context, int64) (bool, error) {
	return false, nil
}

func (s *fakeSessionStore) RecordSyncSuccess(context.Context, int64) error { return nil }

func (s *fakeSessionStore) RecordSyncFailure(context.Context, int64, string) error { return nil }

func (s *fakeSessionStore) WithUserSyncLock(ctx context.Context, _ int64, fn func(context.Context) error) error {
	return fn(ctx)
}

// The party-events methods of the session-store double: no events, and a
// create that records nothing.

func (s *fakeSessionStore) CreatePartyEvent(context.Context, string, int, time.Time, time.Time) error {
	s.calls = append(s.calls, "create-event")
	return nil
}

func (s *fakeSessionStore) UpcomingPartyEvents(context.Context, string, time.Time) ([]db.PartyEvent, error) {
	return nil, nil
}
