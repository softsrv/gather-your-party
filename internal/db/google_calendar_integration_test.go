//go:build integration

package db

import (
	"testing"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

// CLM-1: An integration test (testdb, real postgres:16) applies migrations,
// upserts a user, creates a session, and saves a connection. It deletes the session
// via DeleteSession, creates a new one, and asserts the connection lookup for that
// user still returns the connection with its email.
func TestGoogleCalendarConnectionSurvivesSessionDeletion(t *testing.T) {
	store, _, ctx := partyTestStore(t)

	// Upsert user and create initial session
	userID, err := store.UpsertUser(ctx, "76561198000000901", steamapi.Player{
		PersonaName: "Gordon",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	sessionToken, err := store.CreateSession(ctx, userID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Save Google Calendar connection
	expiry := time.Now().Add(1 * time.Hour).Truncate(time.Microsecond)
	conn := GoogleCalendarConnection{
		UserID:       userID,
		Email:        "gordon@example.com",
		RefreshToken: "refresh-token-1",
		AccessToken:  "access-token-1",
		TokenExpiry:  expiry,
	}
	if err := store.SaveGoogleCalendarConnection(ctx, userID, conn); err != nil {
		t.Fatalf("save connection: %v", err)
	}

	// Delete session and verify it is gone
	if err := store.DeleteSession(ctx, sessionToken); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, ok, err := store.ResolveSession(ctx, sessionToken); err != nil || ok {
		t.Fatalf("expected session deleted, got ok=%v, err=%v", ok, err)
	}

	// Create a new session for the same user
	newSessionToken, err := store.CreateSession(ctx, userID)
	if err != nil {
		t.Fatalf("create new session: %v", err)
	}
	if newSessionToken == "" {
		t.Fatal("expected non-empty new session token")
	}

	// Connection lookup for user still returns connection with saved email
	loaded, found, err := store.GoogleCalendarConnection(ctx, userID)
	if err != nil {
		t.Fatalf("lookup connection: %v", err)
	}
	if !found {
		t.Fatal("expected connection found=true after session deletion, got false")
	}
	if loaded.Email != "gordon@example.com" {
		t.Fatalf("expected email 'gordon@example.com', got %q", loaded.Email)
	}
	if loaded.RefreshToken != "refresh-token-1" {
		t.Fatalf("expected refresh token 'refresh-token-1', got %q", loaded.RefreshToken)
	}
	if loaded.AccessToken != "access-token-1" {
		t.Fatalf("expected access token 'access-token-1', got %q", loaded.AccessToken)
	}
}

// CLM-3: Connection reads, writes and deletes are scoped to one user ID.
// Saving, reading or deleting for user A never returns, replaces or removes user B's connection.
func TestGoogleCalendarConnectionsScopedToUser(t *testing.T) {
	store, _, ctx := partyTestStore(t)

	userA, err := store.UpsertUser(ctx, "76561198000000902", steamapi.Player{
		PersonaName: "User A",
	})
	if err != nil {
		t.Fatalf("upsert user A: %v", err)
	}

	userB, err := store.UpsertUser(ctx, "76561198000000903", steamapi.Player{
		PersonaName: "User B",
	})
	if err != nil {
		t.Fatalf("upsert user B: %v", err)
	}

	expiryA := time.Now().Add(1 * time.Hour).Truncate(time.Microsecond)
	connA := GoogleCalendarConnection{
		UserID:       userA,
		Email:        "usera@example.com",
		RefreshToken: "refresh-a",
		AccessToken:  "access-a",
		TokenExpiry:  expiryA,
	}
	if err := store.SaveGoogleCalendarConnection(ctx, userA, connA); err != nil {
		t.Fatalf("save conn A: %v", err)
	}

	expiryB := time.Now().Add(2 * time.Hour).Truncate(time.Microsecond)
	connB := GoogleCalendarConnection{
		UserID:       userB,
		Email:        "userb@example.com",
		RefreshToken: "refresh-b",
		AccessToken:  "access-b",
		TokenExpiry:  expiryB,
	}
	if err := store.SaveGoogleCalendarConnection(ctx, userB, connB); err != nil {
		t.Fatalf("save conn B: %v", err)
	}

	// 1. Lookup for A returns only A's email; lookup for B returns B's email
	loadedA, foundA, err := store.GoogleCalendarConnection(ctx, userA)
	if err != nil || !foundA {
		t.Fatalf("lookup A: found=%v, err=%v", foundA, err)
	}
	if loadedA.Email != "usera@example.com" {
		t.Fatalf("expected A's email 'usera@example.com', got %q", loadedA.Email)
	}

	loadedB, foundB, err := store.GoogleCalendarConnection(ctx, userB)
	if err != nil || !foundB {
		t.Fatalf("lookup B: found=%v, err=%v", foundB, err)
	}
	if loadedB.Email != "userb@example.com" {
		t.Fatalf("expected B's email 'userb@example.com', got %q", loadedB.Email)
	}

	// 2. Save for A (new email/tokens) leaves B's row unchanged
	connAUpdated := GoogleCalendarConnection{
		UserID:       userA,
		Email:        "usera-updated@example.com",
		RefreshToken: "refresh-a-2",
		AccessToken:  "access-a-2",
		TokenExpiry:  expiryA,
	}
	if err := store.SaveGoogleCalendarConnection(ctx, userA, connAUpdated); err != nil {
		t.Fatalf("save conn A updated: %v", err)
	}

	loadedBAfterUpdate, foundB, err := store.GoogleCalendarConnection(ctx, userB)
	if err != nil || !foundB {
		t.Fatalf("lookup B after A update: found=%v, err=%v", foundB, err)
	}
	if loadedBAfterUpdate.Email != "userb@example.com" || loadedBAfterUpdate.RefreshToken != "refresh-b" {
		t.Fatalf("user B was mutated by user A update: %+v", loadedBAfterUpdate)
	}

	// 3. Delete for A leaves B's row intact, returns A's deleted row, and A's lookup is found=false
	deletedA, foundDelA, err := store.DeleteGoogleCalendarConnection(ctx, userA)
	if err != nil || !foundDelA {
		t.Fatalf("delete A: found=%v, err=%v", foundDelA, err)
	}
	if deletedA.Email != "usera-updated@example.com" {
		t.Fatalf("expected deleted A email 'usera-updated@example.com', got %q", deletedA.Email)
	}

	loadedAAfterDel, foundAAfterDel, err := store.GoogleCalendarConnection(ctx, userA)
	if err != nil {
		t.Fatalf("lookup A after del: %v", err)
	}
	if foundAAfterDel {
		t.Fatalf("expected A found=false after delete, got %+v", loadedAAfterDel)
	}

	loadedBAfterDel, foundBAfterDel, err := store.GoogleCalendarConnection(ctx, userB)
	if err != nil || !foundBAfterDel {
		t.Fatalf("lookup B after A delete: found=%v, err=%v", foundBAfterDel, err)
	}
	if loadedBAfterDel.Email != "userb@example.com" || loadedBAfterDel.RefreshToken != "refresh-b" {
		t.Fatalf("user B was affected by user A delete: %+v", loadedBAfterDel)
	}
}
