//go:build integration

package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gather-your-party/internal/middleware"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// This test requires an empty, disposable database. No application data is used.
func TestPersistence(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name an empty, disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal("cannot connect to test database")
	}
	migration, err := os.ReadFile("../../migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, "DROP TABLE sessions, users"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()

	store := New(pool)
	const verifiedID = "76561198000000001"
	profile := steamapi.Player{
		SteamID: "76561198000000002", PersonaName: "Alyx",
		AvatarSmall: "https://example.com/small", AvatarMedium: "https://example.com/medium", AvatarFull: "https://example.com/full",
	}
	id, err := store.UpsertUser(ctx, verifiedID, profile)
	if err != nil {
		t.Fatal(err)
	}
	var gotID, persona, small, medium, full string
	var created, updated time.Time
	readUser := func() {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full, created_at, updated_at FROM users WHERE id = $1`, id).
			Scan(&gotID, &persona, &small, &medium, &full, &created, &updated)
		if err != nil {
			t.Fatal(err)
		}
		if gotID != verifiedID || persona != profile.PersonaName || small != profile.AvatarSmall || medium != profile.AvatarMedium || full != profile.AvatarFull {
			t.Fatalf("wrong persisted profile: %q %q %q %q %q", gotID, persona, small, medium, full)
		}
	}
	readUser()
	if id <= 0 || created.IsZero() || updated.IsZero() {
		t.Fatal("missing generated id or timestamps")
	}
	originalCreated := created
	if _, err := pool.Exec(ctx, `UPDATE users SET updated_at = '2000-01-01' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	profile.PersonaName = "Barney"
	profile.AvatarSmall += "-new"
	profile.AvatarMedium += "-new"
	profile.AvatarFull += "-new"
	updatedID, err := store.UpsertUser(ctx, verifiedID, profile)
	if err != nil || updatedID != id {
		t.Fatalf("upsert returned id %d, error %v", updatedID, err)
	}
	readUser()
	if !created.Equal(originalCreated) || updated.Before(originalCreated) {
		t.Fatal("upsert did not preserve creation time and refresh update time")
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one user, got %d, error %v", count, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full) VALUES ($1, '', '', '', '')`, verifiedID)
	assertSQLState(t, err, "23505")

	token, err := store.CreateSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatal("session token is not a 256-bit hex string")
	}
	secondToken, err := store.CreateSession(ctx, id)
	if err != nil || token == secondToken {
		t.Fatalf("second session did not get a distinct token: %v", err)
	}
	var owner int64
	var expires, sessionCreated time.Time
	if err := pool.QueryRow(ctx, `SELECT users.id, sessions.expires_at, sessions.created_at FROM sessions JOIN users ON sessions.user_id = users.id WHERE token = $1`, token).Scan(&owner, &expires, &sessionCreated); err != nil {
		t.Fatal(err)
	}
	if owner != id || sessionCreated.IsZero() || expires.Sub(sessionCreated) != 7*24*time.Hour {
		t.Fatal("session has wrong owner, creation time, or expiry")
	}
	_, err = pool.Exec(ctx, `INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, now())`, token, id)
	assertSQLState(t, err, "23505")
	invalidToken, err := store.CreateSession(ctx, -1)
	assertSQLState(t, err, "23503")
	if invalidToken != "" {
		t.Fatal("failed insert returned a usable token")
	}
	// Measure against database time so host clock skew cannot hide a bad lifetime.
	assertSevenDays := func() time.Time {
		t.Helper()
		var expiry, now time.Time
		if err := pool.QueryRow(ctx, `SELECT expires_at, now() FROM sessions WHERE token = $1`, token).Scan(&expiry, &now); err != nil {
			t.Fatal(err)
		}
		if remaining := expiry.Sub(now); remaining < 7*24*time.Hour-time.Minute || remaining > 7*24*time.Hour+time.Minute {
			t.Fatalf("session lifetime = %v, want approximately seven days", remaining)
		}
		return expiry
	}
	assertSevenDays()
	if identity, ok, err := store.ResolveSession(ctx, token); err != nil || !ok || identity != verifiedID {
		t.Fatalf("resolve = %q, %t, %v", identity, ok, err)
	}
	assertSevenDays()

	// Exercise the real middleware with the real store, including context identity.
	secret := []byte("integration-test-secret")
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(token))
	cookie := &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	auth := &middleware.Authenticator{Resolver: store, Secret: secret}
	resolveRequest := func(wantIdentity bool) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		requestContext := &middleware.CustomContext{Context: ctx, StartTime: time.Now()}
		if err := auth.LoadSteamId(requestContext, w, r); err != nil {
			t.Fatal(err)
		}
		if wantIdentity {
			if requestContext.Value(middleware.SteamID{}) != verifiedID {
				t.Fatal("verified database identity did not reach request context")
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Value != cookie.Value || cookies[0].MaxAge != 7*24*3600 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
				t.Fatal("authenticated request did not refresh the secure browser cookie")
			}
		} else if requestContext.Value(middleware.SteamID{}) != nil || len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid session reached context or refreshed its cookie")
		}
	}
	var oldExpiry time.Time
	if err := pool.QueryRow(ctx, `UPDATE sessions SET expires_at = now() + interval '1 hour' WHERE token = $1 RETURNING expires_at`, token).Scan(&oldExpiry); err != nil {
		t.Fatal(err)
	}
	resolveRequest(true)
	if refreshed := assertSevenDays(); !refreshed.After(oldExpiry) {
		t.Fatal("near-expiry session did not slide forward")
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token = $1`, token); err != nil {
		t.Fatal(err)
	}
	if identity, ok, err := store.ResolveSession(ctx, token); err != nil || ok || identity != "" {
		t.Fatalf("expired session resolved: %q, %t, %v", identity, ok, err)
	}
	resolveRequest(false)
	var stillExpired bool
	if err := pool.QueryRow(ctx, `SELECT expires_at <= now() FROM sessions WHERE token = $1`, token).Scan(&stillExpired); err != nil || !stillExpired {
		t.Fatal("expired session was revived")
	}
	if err := store.DeleteSession(ctx, secondToken); err != nil {
		t.Fatal(err)
	}
	if identity, ok, err := store.ResolveSession(ctx, secondToken); err != nil || ok || identity != "" {
		t.Fatalf("deleted session resolved: %q, %t, %v", identity, ok, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token = $1`, secondToken).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted session count = %d, error %v", count, err)
	}
	if err := store.DeleteSession(ctx, secondToken); err != nil {
		t.Fatalf("repeated delete must be idempotent: %v", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if failedID, err := store.UpsertUser(cancelled, verifiedID, profile); err == nil || failedID != 0 {
		t.Fatal("cancelled upsert did not return an error and zero id")
	}
	if identity, ok, err := store.ResolveSession(cancelled, token); err == nil || ok || identity != "" {
		t.Fatal("cancelled resolution did not return an error without identity")
	}
	if err := store.DeleteSession(cancelled, token); err == nil {
		t.Fatal("cancelled deletion did not return an error")
	}
}

func assertSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %v", code, err)
	}
}
