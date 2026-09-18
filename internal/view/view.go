package view

import (
	"context"
	"fmt"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/template"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

func ServeFavicon(w http.ResponseWriter, r *http.Request) {
	filePath := "favicon.ico"
	fullPath := filepath.Join(".", "static", filePath)
	http.ServeFile(w, r, fullPath)
}

func ServeStaticFiles(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Path[len("/static/"):]
	fullPath := filepath.Join(".", "static", filePath)
	http.ServeFile(w, r, fullPath)
}

func Home(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	fmt.Println("Created the service!")
	steamIDValue := ctx.Context.Value("steamID")
	if steamIDValue == nil {
		template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w)
		return
	}

	playerIdList := []string{steamIDValue.(string)}
	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	players, err := SteamService.Players(newCtx, playerIdList)
	if err != nil {
		http.NotFound(w, r)
	}
	template.Home(players[0], "Gather Your Party", template.Main).Render(ctx, w)
}

func GamesList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Context.Value("steamID")
	if steamIDValue == nil {
		template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w)
		return
	}
	playerId := steamIDValue.(string)
	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	games, err := SteamService.Games(newCtx, playerId)
	if err != nil {
		http.NotFound(w, r)
	}
	template.GameList(games).Render(ctx, w)
}

func FriendsList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Context.Value("steamID")
	if steamIDValue == nil {
		template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w)
		return
	}

	playerId := steamIDValue.(string)

	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	friends, err := SteamService.Friends(newCtx, playerId)
	if err != nil {
		http.NotFound(w, r)
	}

	template.FriendsList(friends).Render(ctx, w)
}

func SharedGamesList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Context.Value("steamID")
	if steamIDValue == nil {
		template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w)
		return
	}

	playerId := steamIDValue.(string)
	friendIDs := r.URL.Query()["friendID"]
	if len(friendIDs) == 0 {
		template.SharedGamesPrompt().Render(ctx, w)
		return
	}
	ids := sharedGameIDs(playerId, friendIDs)

	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	games, err := SteamService.SharedGames(newCtx, ids)
	if err != nil {
		http.NotFound(w, r)
	}

	template.GameList(games).Render(ctx, w)
}

func sharedGameIDs(playerId string, friendIDs []string) []string {
	return append([]string{playerId}, friendIDs...)
}

func Login(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login" {
		http.NotFound(w, r)
		return
	}
	template.Login().Render(ctx, w)
}

func PostLoginRedirect(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:    "steam_id",
		Value:   r.PostFormValue("steamID"),
		Expires: time.Now().Add(120 * time.Second),
	})
	w.Header().Set("HX-redirect", "/")

	http.RedirectHandler("/", http.StatusSeeOther)
}
