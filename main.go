package main

import (
	"context"
	"errors"
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
// CLM-4: APP_BASE_URL and SESSION_SECRET continue to be read here via
// os.Getenv after godotenv.Load(), so the auth code relies on both being
// loaded configuration values (not derived at request time).
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
		steamAPIKey:   os.Getenv("STEAM_API_KEY"),
	}
}

// validateConfig enforces boot-time fail-fast on required values.
// Extracted so it is unit-testable without invoking main(). CLM-3.
//
// APP_BASE_URL is required: the auth flow derives realm/return_to solely
// from it (CLM-2 MUST-forbid on request-host derivation), so if it is
// empty the process must not start.
func validateConfig(cfg config) error {
	if cfg.databaseURL == "" {
		return errors.New("DATABASE_URL environment variable is required but not set")
	}
	if cfg.appBaseURL == "" {
		return errors.New("APP_BASE_URL environment variable is required but not set")
	}
	if cfg.sessionSecret == "" {
		return errors.New("SESSION_SECRET environment variable is required but not set")
	}
	return nil
}

func main() {
	_ = godotenv.Load()

	cfg := loadConfig()
	if err := validateConfig(cfg); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		log.Fatalf("failed to create database pool: %v", err)
	}
	defer pool.Close()

	st := store.NewStore(pool)

	// CLM-12: never log SESSION_SECRET or the session token. Log only that
	// the secret is present (length), never the value.
	log.Printf("boot: APP_BASE_URL=%q session_secret_len=%d steam_api_key_set=%v",
		cfg.appBaseURL, len(cfg.sessionSecret), cfg.steamAPIKey != "")

	mux := http.NewServeMux()

	mux.HandleFunc("GET /favicon.ico", view.ServeFavicon)
	mux.HandleFunc("GET /static/", view.ServeStaticFiles)

	// Data views: use the new ResolveSession middleware (CLM-14 successor
	// to the retired LoadSteamId). They read ctx.Value("steamID") exactly
	// as they do today.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.Home, middleware.ResolveSession)
	})
	mux.HandleFunc("GET /frag/games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.GamesList, middleware.ResolveSession)
	})
	mux.HandleFunc("GET /frag/friends", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.FriendsList, middleware.ResolveSession)
	})
	mux.HandleFunc("POST /frag/shared-games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.SharedGamesList, middleware.ResolveSession)
	})

	// Auth routes (CLM-1, CLM-5..CLM-11).
	// GET /login → 303 redirect to Steam's OpenID provider (CLM-1).
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.LoginRedirect)
	})
	// GET /auth/steam/callback → verify, upsert, create session, sign cookie.
	mux.HandleFunc("GET "+view.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.SteamCallback)
	})
	// POST /logout → delete session + clear cookie (CLM-16).
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(st, cfg.appBaseURL, cfg.sessionSecret, cfg.steamAPIKey, w, r, view.Logout)
	})

	// NOTE: the insecure POST /login → view.PostLoginRedirect route has
	// been REMOVED (CLM-17 MUST-forbid). No handler in the app now accepts
	// a user-supplied SteamID as identity, and no code sets a plaintext
	// "steam_id" cookie.

	fmt.Printf("server is running on port %s\n", os.Getenv("LISTEN_ADDR"))
	if err := http.ListenAndServe(":"+os.Getenv("LISTEN_ADDR"), mux); err != nil {
		fmt.Println(err)
	}
}
