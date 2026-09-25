package view

import (
	"context"
	"errors"
	"fmt"
	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/template"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

// PartyStore is the persistence surface needed by party action handlers.
// Session-only handlers keep their existing, narrower interface.
type PartyStore interface {
	ResolveUserID(context.Context, string) (int64, bool, error)
	CreateParty(context.Context, string, int64) (string, error)
	LeaveParty(context.Context, string, int64) error
	StepDown(context.Context, string, int64, int64) error
}

// PartyActions supplies store-backed handlers compatible with middleware.Chain.
type PartyActions struct {
	Store PartyStore
}

func (a PartyActions) actingUser(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) (int64, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return 0, false
	}
	steamID64, ok := ctx.Context.Value(middleware.SteamID{}).(string)
	if !ok || steamID64 == "" {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return 0, false
	}
	userID, found, err := a.Store.ResolveUserID(r.Context(), steamID64)
	if err != nil {
		http.Error(w, "unable to resolve user", http.StatusInternalServerError)
		return 0, false
	}
	if !found {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return 0, false
	}
	return userID, true
}

func partyActionComplete(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/parties")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/parties", http.StatusSeeOther)
}

func (a PartyActions) CreateParty(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := a.actingUser(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid party form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		http.Error(w, "party name required", http.StatusBadRequest)
		return
	}
	if _, err := a.Store.CreateParty(r.Context(), name, userID); err != nil {
		http.Error(w, "unable to create party", http.StatusInternalServerError)
		return
	}
	partyActionComplete(w, r)
}

func (a PartyActions) LeaveParty(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := a.actingUser(ctx, w, r)
	if !ok {
		return
	}
	partyID := r.PathValue("partyID")
	if partyID == "" {
		http.Error(w, "party required", http.StatusBadRequest)
		return
	}
	if err := a.Store.LeaveParty(r.Context(), partyID, userID); err != nil {
		http.Error(w, "unable to leave party", http.StatusInternalServerError)
		return
	}
	partyActionComplete(w, r)
}

func (a PartyActions) StepDown(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := a.actingUser(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid step down form", http.StatusBadRequest)
		return
	}
	partyID := r.PathValue("partyID")
	targetID, err := strconv.ParseInt(r.PostForm.Get("targetUserID"), 10, 64)
	if partyID == "" || err != nil || targetID <= 0 {
		http.Error(w, "party and target member required", http.StatusBadRequest)
		return
	}
	if err := a.Store.StepDown(r.Context(), partyID, userID, targetID); err != nil {
		if errors.Is(err, db.ErrStepDownNotAllowed) {
			http.Error(w, "step down not allowed", http.StatusForbidden)
			return
		}
		http.Error(w, "unable to step down", http.StatusInternalServerError)
		return
	}
	partyActionComplete(w, r)
}

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
	steamIDValue := ctx.Context.Value(middleware.SteamID{})
	if steamIDValue == nil {
		if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}

	playerIdList := []string{steamIDValue.(string)}
	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	players, err := SteamService.Players(newCtx, playerIdList)
	if err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	if len(players) == 0 {
		if err := template.ErrorMessage("no player found").Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	if err := template.Home(players[0], "Gather Your Party", template.Main).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func GamesList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Context.Value(middleware.SteamID{})
	if steamIDValue == nil {
		if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	playerId := steamIDValue.(string)
	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	games, err := SteamService.Games(newCtx, playerId)
	if err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	if err := template.GameList(games).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func FriendsList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Context.Value(middleware.SteamID{})
	if steamIDValue == nil {
		if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}

	playerId := steamIDValue.(string)

	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()
	friends, err := SteamService.Friends(newCtx, playerId)
	if err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}

	if err := template.FriendsList(friends).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func SharedGamesList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Context.Value(middleware.SteamID{})
	if steamIDValue == nil {
		if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	playerId := steamIDValue.(string)

	deadline := time.Now().Add(5000 * time.Millisecond)
	newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
	defer cancelCtx()

	friends, err := SteamService.Friends(newCtx, playerId)
	if err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}

	if err := r.ParseForm(); err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	friendIDs := r.Form["friendID"]

	games, err := SteamService.SharedGames(newCtx, playerId, friendIDs...)
	if err != nil {
		if messages, ok := sharedGamesNoGamesMessages(err, friends); ok {
			if err := template.ErrorMessages(messages).Render(ctx, w); err != nil {
				fmt.Printf("render error: %s\n", err)
			}
			return
		}
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}

	if err := template.SharedGamesList(games).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
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
