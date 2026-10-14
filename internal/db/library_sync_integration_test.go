//go:build integration

package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// sameSchemaPool opens another pool on the schema the given pool uses, with at
// most maxConns connections.
func sameSchemaPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool, maxConns int32) *pgxpool.Pool {
	t.Helper()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = maxConns
	other, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	t.Cleanup(other.Close)
	return other
}

func distinctGames(first, n int) []steamapi.Game {
	games := make([]steamapi.Game, n)
	for i := range games {
		app := first + i
		games[i] = steamapi.Game{AppID: app, Name: fmt.Sprintf("Game %d", app), ImgIconURL: fmt.Sprintf("icon%d", app)}
	}
	return games
}

func userGameCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_games WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// CLM-11: four concurrent 5,000-game saves through a 4-connection pool finish
// within the 20s shared-games sync budget.
func TestStoreUserLibraryFourConcurrentLargeLibraries(t *testing.T) {
	_, base, ctx := partyTestStore(t)
	pool := sameSchemaPool(t, ctx, base, 4)
	store := New(pool)
	userIDs := make([]int64, 4)
	for i := range userIDs {
		userIDs[i] = gameTestUser(t, store, ctx, fmt.Sprintf("7656119800002100%d", i))
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(userIDs))
	for i, userID := range userIDs {
		wg.Add(1)
		go func(i int, userID int64) {
			defer wg.Done()
			errs[i] = store.StoreUserLibrary(saveCtx, userID, distinctGames(100000*(i+1), 5000))
		}(i, userID)
	}
	wg.Wait()
	for i, userID := range userIDs {
		if errs[i] != nil {
			t.Fatalf("save %d: %v", i, errs[i])
		}
		if n := userGameCount(t, ctx, pool, userID); n != 5000 {
			t.Fatalf("user %d: user_games=%d want 5000", i, n)
		}
	}
}

// CLM-12: a save that times out persists no rows and no state change.
func TestStoreUserLibraryTimeoutPersistsNothing(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	userID := gameTestUser(t, store, ctx, "76561198000022001")
	if err := store.StoreUserLibrary(ctx, userID, steamGames(1, 2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSyncSuccess(ctx, userID); err != nil {
		t.Fatal(err)
	}
	read := func() (string, time.Time, time.Time, int, int) {
		t.Helper()
		var state string
		var stateAt, syncedAt time.Time
		var games int
		if err := pool.QueryRow(ctx, `SELECT game_sync_state, game_sync_state_at, library_synced_at, (SELECT count(*) FROM games) FROM users WHERE id = $1`, userID).
			Scan(&state, &stateAt, &syncedAt, &games); err != nil {
			t.Fatal(err)
		}
		return state, stateAt, syncedAt, userGameCount(t, ctx, pool, userID), games
	}
	s0, a0, y0, n0, g0 := read()
	deadline, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := store.StoreUserLibrary(deadline, userID, distinctGames(500000, 5000)); err == nil {
		t.Fatal("expected a timeout error")
	}
	s1, a1, y1, n1, g1 := read()
	if s1 != s0 || !a1.Equal(a0) || !y1.Equal(y0) || n1 != n0 || g1 != g0 {
		t.Fatalf("state changed: before %s %v %v %d %d after %s %v %v %d %d", s0, a0, y0, n0, g0, s1, a1, y1, n1, g1)
	}
}

// CLM-8: a lock left by a closed connection is free for the next sync.
func TestUserSyncLockFreedByDeadConnection(t *testing.T) {
	store, base, ctx := partyTestStore(t)
	userID := gameTestUser(t, store, ctx, "76561198000023001")
	dead := sameSchemaPool(t, ctx, base, 1)
	conn, err := dead.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := int32(userID) ^ int32(userID>>32)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, userSyncLockNamespace, key); err != nil {
		t.Fatal(err)
	}
	// While held, another session cannot take it.
	held, cancelHeld := context.WithTimeout(ctx, 300*time.Millisecond)
	if err := store.WithUserSyncLock(held, userID, func(context.Context) error { return nil }); err == nil {
		t.Fatal("lock taken while another session holds it")
	}
	cancelHeld()
	conn.Release()
	dead.Close() // closes the connection without unlocking

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ran := false
	if err := store.WithUserSyncLock(lockCtx, userID, func(ctx context.Context) error {
		ran = true
		return store.RecordSyncSuccess(ctx, userID)
	}); err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if due, err := store.LibrarySyncDue(ctx, userID); err != nil || due {
		t.Fatalf("due=%v err=%v after recorded success", due, err)
	}
	// Released after fn: the lock can be taken again at once.
	again, cancelAgain := context.WithTimeout(ctx, time.Second)
	defer cancelAgain()
	if err := store.WithUserSyncLock(again, userID, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("relock: %v", err)
	}
}

// CLM-4 / CLM-10: due-ness follows the success stamp; success clears a failure
// and a failure keeps the stamp.
func TestLibrarySyncDueAndRecordSyncSuccessFailure(t *testing.T) {
	store, pool, ctx := partyTestStore(t)
	const steamID = "76561198000024001"
	userID := gameTestUser(t, store, ctx, steamID)
	due := func() bool {
		t.Helper()
		d, err := store.LibrarySyncDue(ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if !due() {
		t.Fatal("never-synced user must be due")
	}
	for _, c := range []struct {
		age  string
		want bool
	}{{"1 hour", false}, {"25 hours", true}} {
		if _, err := pool.Exec(ctx, `UPDATE users SET library_synced_at = now() - $2::interval WHERE id = $1`, userID, c.age); err != nil {
			t.Fatal(err)
		}
		if got := due(); got != c.want {
			t.Fatalf("stamp %s ago: due=%v want %v", c.age, got, c.want)
		}
	}
	stateOf := func() GameSyncState {
		t.Helper()
		states, err := store.GameSyncStates(ctx, []string{steamID})
		if err != nil {
			t.Fatal(err)
		}
		return states[steamID]
	}
	stamp := stateOf().LibrarySyncedAt
	if err := store.RecordSyncFailure(ctx, userID, "steam: 503"); err != nil {
		t.Fatal(err)
	}
	failed := stateOf()
	if failed.LibrarySyncError != "steam: 503" || failed.LibrarySyncErrorAt == nil || failed.LibrarySyncedAt == nil || !failed.LibrarySyncedAt.Equal(*stamp) {
		t.Fatalf("after failure: %+v", failed)
	}
	if err := store.RecordSyncSuccess(ctx, userID); err != nil {
		t.Fatal(err)
	}
	ok := stateOf()
	if ok.LibrarySyncError != "" || ok.LibrarySyncErrorAt != nil || ok.LibrarySyncedAt == nil || !ok.LibrarySyncedAt.After(*stamp) {
		t.Fatalf("after success: %+v", ok)
	}
	if due() {
		t.Fatal("just-synced user must not be due")
	}
}
