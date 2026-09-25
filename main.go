package main

import (
	"context"
	"fmt"
	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/view"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// appConfig holds settings for the future session-security and OpenID handlers.
type appConfig struct {
	SessionSecret string
	AppBaseURL    string
}

type application struct {
	store  *db.Store
	config appConfig
}

func main() {
	_ = godotenv.Load()
	config := appConfig{
		SessionSecret: os.Getenv("SESSION_SECRET"),
		AppBaseURL:    os.Getenv("APP_BASE_URL"),
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		// Do not log the DSN or parsing error, which can contain credentials.
		fmt.Fprintln(os.Stderr, "unable to initialize database pool")
		return
	}
	defer pool.Close()

	app := application{store: db.New(pool), config: config}
	app.serve()
}

func (app *application) serve() {
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
	err := http.ListenAndServe(":"+os.Getenv("LISTEN_ADDR"), mux)
	if err != nil {
		fmt.Println(err)
	}

}
