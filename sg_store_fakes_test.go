package main

import (
	"context"

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

// The Google Calendar connection methods of the session-store double: no
// user has a connection, and saving or deleting is a no-op.

func (s *fakeSessionStore) SaveGoogleCalendarConnection(context.Context, int64, db.GoogleCalendarConnection) error {
	return nil
}

func (s *fakeSessionStore) GoogleCalendarConnection(context.Context, int64) (db.GoogleCalendarConnection, bool, error) {
	return db.GoogleCalendarConnection{}, false, nil
}

func (s *fakeSessionStore) DeleteGoogleCalendarConnection(context.Context, int64) (db.GoogleCalendarConnection, bool, error) {
	return db.GoogleCalendarConnection{}, false, nil
}
