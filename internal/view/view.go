package view

import (
	"context"
	"errors"
	"fmt"
	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/template"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

type inviteFriendsService interface {
	Friends(context.Context, string) ([]steamapi.Player, error)
}

type inviteCandidateStore interface {
	InviteCandidates(context.Context, string, int64, []string) ([]db.InviteCandidate, error)
}

// BuildInviteCandidates fetches the verified leader's live friends and builds
// the picker model through the store's intersection and exclusions. Callers must
// supply the SteamID and users.id resolved from the same verified session.
// This helper is ready for the parties view; it does not introduce a new page.
func BuildInviteCandidates(ctx context.Context, service inviteFriendsService, store inviteCandidateStore, partyID string, actingLeaderUserID int64, verifiedSteamID string) ([]component.InviteCandidate, error) {
	friends, err := service.Friends(ctx, verifiedSteamID)
	if err != nil {
		return nil, fmt.Errorf("invite picker: friends: %w", err)
	}
	friendIDs := make([]string, 0, len(friends))
	for _, friend := range friends {
		friendIDs = append(friendIDs, friend.SteamID)
	}
	known, err := store.InviteCandidates(ctx, partyID, actingLeaderUserID, friendIDs)
	if err != nil {
		return nil, fmt.Errorf("invite picker: candidates: %w", err)
	}
	candidates := make([]component.InviteCandidate, 0, len(known))
	for _, candidate := range known {
		candidates = append(candidates, component.InviteCandidate{UserID: candidate.UserID, Name: candidate.Name})
	}
	return candidates, nil
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
