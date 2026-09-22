package view

import (
	"context"
	"errors"
	"fmt"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/template"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

const noGamesMessageFormat = "no games found for user %s. Their list may be private"

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
		template.ErrorMessage(err.Error()).Render(ctx, w)
		return
	}
	if len(players) == 0 {
		template.ErrorMessage("no player found").Render(ctx, w)
		return
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
		template.ErrorMessage(err.Error()).Render(ctx, w)
		return
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
		template.ErrorMessage(err.Error()).Render(ctx, w)
		return
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

	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()

	friends, err := SteamService.Friends(newCtx, playerId)
	if err != nil {
		template.ErrorMessage(err.Error()).Render(ctx, w)
		return
	}

	if err := r.ParseForm(); err != nil {
		template.ErrorMessage(err.Error()).Render(ctx, w)
		return
	}
	friendIDs := r.Form["friendID"]

	games, err := SteamService.SharedGames(newCtx, playerId, friendIDs...)
	if err != nil {
		if messages, ok := sharedGamesNoGamesMessages(err, friends); ok {
			template.ErrorMessages(messages).Render(ctx, w)
			return
		}
		template.ErrorMessage(err.Error()).Render(ctx, w)
		return
	}

	template.SharedGamesList(games).Render(ctx, w)
}

func sharedGamesNoGamesMessages(err error, roster []steamapi.Player) ([]string, bool) {
	var nge *steamapi.NoGamesError
	if !errors.As(err, &nge) {
		return nil, false
	}
	return noGamesMessages(nge.SteamIDs, roster), true
}

func noGamesMessages(ids []string, roster []steamapi.Player) []string {
	personasBySteamID := make(map[string]string, len(roster))
	for _, player := range roster {
		personasBySteamID[player.SteamID] = player.PersonaName
	}

	messages := make([]string, 0, len(ids))
	for _, steamID := range ids {
		personaName := personasBySteamID[steamID]
		if personaName == "" {
			personaName = steamID
		}
		messages = append(messages, fmt.Sprintf(noGamesMessageFormat, personaName))
	}
	return messages
}

func Login(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login" {
		http.NotFound(w, r)
		return
	}
	template.Login().Render(ctx, w)
}

// PostLoginRedirect has been REMOVED. It previously set an unsigned
// plaintext "steam_id" cookie from r.PostFormValue, which allowed a user
// to log in as any SteamID. The signed-session flow (LoginRedirect →
// SteamCallback in auth.go) replaces it. CLM-17 MUST-forbid.
