package main

import (
	"context"
	"fmt"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/store"
	"gather-your-party/internal/view"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	// CLM-1: DATABASE_URL — required; boot fails if missing/empty.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required but not set")
	}

	// CLM-14: APP_BASE_URL — read at boot and retained.
	appBaseURL := os.Getenv("APP_BASE_URL")

	// CLM-15: SESSION_SECRET — read at boot and retained.
	sessionSecret := os.Getenv("SESSION_SECRET")

	// CLM-2 / CLM-3: build the pool exactly once in main(), then inject
	// it into the store. No handler or view may call pgxpool.New.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("failed to create database pool: %v", err)
	}
	defer pool.Close()

	// Wire the store via dependency injection (CLM-3).
	st := store.NewStore(pool)

	// cfg holds boot-time config so all env vars are genuinely used
	// and available for future tasks (auth, session cookie, etc.).
	cfg := struct {
		appBaseURL    string
		sessionSecret string
		store         *store.Store
	}{
		appBaseURL:    appBaseURL,
		sessionSecret: sessionSecret,
		store:         st,
	}
	// Confirm config is wired (log at startup so the values are consumed
	// now; auth task will thread cfg.appBaseURL and cfg.sessionSecret
	// into handlers).
	log.Printf("boot: APP_BASE_URL=%q session_secret_len=%d store=%T",
		cfg.appBaseURL, len(cfg.sessionSecret), cfg.store)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /favicon.ico", view.ServeFavicon)
	mux.HandleFunc("GET /static/", view.ServeStaticFiles)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.Home, middleware.LoadSteamId)
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.Login)
	})
	mux.HandleFunc("GET /frag/games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.GamesList, middleware.LoadSteamId)
	})
	mux.HandleFunc("GET /frag/friends", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.FriendsList, middleware.LoadSteamId)
	})
	mux.HandleFunc("POST /frag/shared-games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.SharedGamesList, middleware.LoadSteamId)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.PostLoginRedirect)
	})

	fmt.Printf("server is running on port %s\n", os.Getenv("LISTEN_ADDR"))
	if err := http.ListenAndServe(":"+os.Getenv("LISTEN_ADDR"), mux); err != nil {
		fmt.Println(err)
	}
}
