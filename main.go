package main

import (
	"fmt"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/view"
	"net/http"
	"os"

	"github.com/joho/godotenv"
)

func main() {

	_ = godotenv.Load()
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
