//go:build integration

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/softsrv/steamapi/steamapi"
)

// Use a separate schema so this test and internal/db's suite can run concurrently.
// TEST_DATABASE_URL must still refer to a disposable test database.
func TestLogoutInvalidatesSession(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL must name an empty, disposable Postgres database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	defer admin.Close()
	schema := fmt.Sprintf("logout_test_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot configure scoped test pool")
	}
	defer pool.Close()
	migration, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	store := db.New(pool)
	userID, err := store.UpsertUser(ctx, testSteamID, steamapi.Player{PersonaName: "Alyx"})
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateSession(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	app := application{store: store, config: appConfig{SessionSecret: "integration-test-secret"}}
	auth := &middleware.Authenticator{Resolver: app.store, Secret: []byte(app.config.SessionSecret)}
	mac := hmac.New(sha256.New, []byte(app.config.SessionSecret))
	mac.Write([]byte(token))
	cookie := &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	request := func(method, path string) *http.Request {
		r := httptest.NewRequest(method, path, nil).WithContext(ctx)
		r.AddCookie(cookie)
		return r
	}
	before := &middleware.CustomContext{Context: ctx, StartTime: time.Now()}
	if err := auth.LoadSteamId(before, httptest.NewRecorder(), request(http.MethodGet, "/")); err != nil || before.Value(middleware.SteamID{}) != testSteamID {
		t.Fatal("session must authenticate before logout")
	}
	w := httptest.NewRecorder()
	app.handleLogout(w, request(http.MethodPost, "/auth/steam/logout"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatal("logout must redirect home")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "session" || cookies[0].Value != "" || cookies[0].Path != "/" || cookies[0].MaxAge != -1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("logout did not securely clear the browser cookie")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token = $1`, token).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session row count after logout = %d, error %v", count, err)
	}
	if identity, ok, err := store.ResolveSession(ctx, token); err != nil || ok || identity != "" {
		t.Fatal("logged-out token still resolves")
	}
	// Re-present the original signed cookie, not the browser's cleared cookie.
	after := &middleware.CustomContext{Context: ctx, StartTime: time.Now()}
	replay := httptest.NewRecorder()
	if err := auth.LoadSteamId(after, replay, request(http.MethodGet, "/")); err != nil || after.Value(middleware.SteamID{}) != nil || len(replay.Result().Cookies()) != 0 {
		t.Fatal("replaying a logged-out cookie must remain unauthenticated")
	}
}
