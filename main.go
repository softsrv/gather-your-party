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

// config holds boot-time configuration assembled from the environment.
type config struct {
	databaseURL   string
	appBaseURL    string
	sessionSecret string
	steamAPIKey   string
}

// loadConfig reads all required environment variables (after godotenv.Load
// has already been called) and returns a populated config.
func loadConfig() config {
	return config{
		databaseURL:   os.Getenv("DATABASE_URL"),
		appBaseURL:    os.Getenv("APP_BASE_URL"),
		sessionSecret: os.Getenv("SESSION_SECRET"),
		steamAPIKey:   os.Getenv("STEAM_API_KEY"), // CLM-1: read at boot alongside the others
	}
}

func main() {
	_ = godotenv.Load()

	cfg := loadConfig()

	// CLM-1: DATABASE_URL — required; boot fails if missing/empty.
	if cfg.databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required but not set")
	}

	// CLM-3: build the pool exactly once in main(), then inject
	// it into the store. No handler or view may call pgxpool.New.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		log.Fatalf("failed to create database pool: %v", err)
	}
	defer pool.Close()

	// Wire the store via dependency injection (CLM-4).
	st := store.NewStore(pool)

	log.Printf("boot: APP_BASE_URL=%q session_secret_len=%d steam_api_key_set=%v",
		cfg.appBaseURL, len(cfg.sessionSecret), cfg.steamAPIKey != "")

	mux := http.NewServeMux()

	mux.HandleFunc("GET /favicon.ico", view.ServeFavicon)
	mux.HandleFunc("GET /static/", view.ServeStaticFiles)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, w, r, view.Home, middleware.LoadSteamId)
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, w, r, view.Login)
	})
	mux.HandleFunc("GET /frag/games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, w, r, view.GamesList, middleware.LoadSteamId)
	})
	mux.HandleFunc("GET /frag/friends", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, w, r, view.FriendsList, middleware.LoadSteamId)
	})
	mux.HandleFunc("POST /frag/shared-games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, w, r, view.SharedGamesList, middleware.LoadSteamId)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, w, r, view.PostLoginRedirect)
	})

	fmt.Printf("server is running on port %s\n", os.Getenv("LISTEN_ADDR"))
	if err := http.ListenAndServe(":"+os.Getenv("LISTEN_ADDR"), mux); err != nil {
		fmt.Println(err)
	}
}
