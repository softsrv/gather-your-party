package view

import (
	"context"
	"errors"
	"fmt"
	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/syncreason"
	"gather-your-party/internal/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

type inviteFriendsService interface {
	Friends(context.Context, string) ([]steamapi.Friend, error)
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
		candidates = append(candidates, component.InviteCandidate{UserID: candidate.UserID, Name: candidate.Name, AvatarURL: candidate.AvatarURL})
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

func GamesList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Value(middleware.SteamID{})
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
	games, err := SteamService.Games(newCtx, playerId, true, true)
	if err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	// Most-played first; the library can run to hundreds of games.
	sort.SliceStable(games, func(i, j int) bool { return games[i].PlaytimeForever > games[j].PlaytimeForever })
	if err := template.GameList(games).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func FriendsList(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	SteamService := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	steamIDValue := ctx.Value(middleware.SteamID{})
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

	friendIDs := make([]string, 0, len(friends))
	for _, friend := range friends {
		friendIDs = append(friendIDs, friend.SteamID)
	}
	roster, err := SteamService.Players(newCtx, friendIDs)
	if err != nil {
		if err := template.ErrorMessage(err.Error()).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}

	sortFriends(roster)
	if err := template.FriendsList(roster).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

// sharedGamesSyncBudget bounds the synchronous sync and game-data fetches of
// one Get shared games request. Batches stored before it expires persist; games
// still unresolved at expiry are left out of that result.
const sharedGamesSyncBudget = 20 * time.Second

// SharedGamesListStore is the store surface Get shared games uses.
type SharedGamesListStore interface {
	SharedGamesStore
	GameSyncStore
	ResolveUserID(context.Context, string) (int64, bool, error)
	UpsertFriend(context.Context, string, steamapi.Player) (int64, error)
	LibrarySyncStore
}

// LibrarySyncStore is the store surface that decides whether a user's library
// sync is due, serialises syncs of one user and records their outcome.
type LibrarySyncStore interface {
	LibrarySyncDue(context.Context, int64) (bool, error)
	RecordSyncSuccess(context.Context, int64) error
	RecordSyncFailure(context.Context, int64, string) error
	WithUserSyncLock(context.Context, int64, func(context.Context) error) error
}

// SharedGamesList serves Get shared games from stored libraries and stored game
// data. Each request syncs the caller's library and that of every selected
// current Steam friend when their library sync is due (creating a friend's
// users row when missing); selected ids that are not current friends are
// ignored.
type SharedGamesList struct {
	Store SharedGamesListStore
	// Steam returns the Steam client for one request.
	Steam func() SteamService
	// SyncBudget overrides sharedGamesSyncBudget when positive.
	SyncBudget time.Duration
}

// NewSharedGamesList returns a Get shared games handler with the production
// sync budget.
func NewSharedGamesList(store SharedGamesListStore, steam func() SteamService) *SharedGamesList {
	return &SharedGamesList{Store: store, Steam: steam, SyncBudget: sharedGamesSyncBudget}
}

// Serve renders the shared multiplayer games of the caller and the selected
// friends.
func (h *SharedGamesList) Serve(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	steamIDValue := ctx.Value(middleware.SteamID{})
	if steamIDValue == nil {
		if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	playerID := steamIDValue.(string)
	steam := h.Steam()
	budget := h.SyncBudget
	if budget <= 0 {
		budget = sharedGamesSyncBudget
	}
	syncCtx, cancelSync := context.WithTimeout(ctx.Context, budget)
	defer cancelSync()

	friends, err := steam.Friends(syncCtx, playerID)
	if err != nil {
		renderErrorMessage(ctx, w, err.Error())
		return
	}
	if err := r.ParseForm(); err != nil {
		renderErrorMessage(ctx, w, err.Error())
		return
	}

	rosterIDs := make([]string, 0, len(friends))
	isFriend := make(map[string]bool, len(friends))
	for _, friend := range friends {
		rosterIDs = append(rosterIDs, friend.SteamID)
		isFriend[friend.SteamID] = true
	}
	// Only current Steam friends are synced, created or intersected.
	selected := make([]string, 0, len(r.Form["friendID"]))
	seen := map[string]bool{playerID: true}
	for _, id := range r.Form["friendID"] {
		if isFriend[id] && !seen[id] {
			seen[id] = true
			selected = append(selected, id)
		}
	}

	roster, err := steam.Players(syncCtx, rosterIDs)
	if err != nil {
		renderErrorMessage(ctx, w, err.Error())
		return
	}
	profiles := make(map[string]steamapi.Player, len(roster))
	names := make(map[string]string, len(roster))
	for _, player := range roster {
		profiles[player.SteamID] = player
		names[player.SteamID] = player.PersonaName
	}

	h.syncMembers(syncCtx, steam, playerID, selected, profiles)

	// Store reads use the request context so an expired budget still renders
	// what was resolved; only Steam fetches are bounded by the budget.
	games, messages, err := storedSharedGames(ctx.Context, syncCtx, steam, h.Store, playerID, selected, names)
	if err != nil {
		renderErrorMessage(ctx, w, err.Error())
		return
	}
	if len(messages) > 0 {
		if err := template.ErrorMessages(messages).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	if err := template.SharedGamesList(games).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

// syncMembers syncs, concurrently under ctx, the caller's library and that of
// every selected friend whose library sync is due (never synced successfully,
// or the last success is over 24 hours old). Each sync runs under the user's
// sync lock and re-checks due-ness once the lock is held, so a sync already
// running elsewhere is waited on rather than repeated. Outcomes are recorded;
// failures are also logged: the result is built from whatever was stored.
func (h *SharedGamesList) syncMembers(ctx context.Context, steam SteamService, caller string, friends []string, profiles map[string]steamapi.Player) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, memberSyncConcurrency)
	start := func(userID int64, steamID string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A panic here would end the process (the request's recover does
			// not cover this goroutine); log it like any other sync failure.
			var err error
			defer func() {
				if err != nil {
					fmt.Printf("shared games: %s\n", err)
				}
			}()
			defer recoverToError(&err, fmt.Sprintf("sync games %s", steamID))
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				err = fmt.Errorf("sync games %s: %w", steamID, ctx.Err())
				return
			}
			err = h.syncMemberIfDue(ctx, steam, userID, steamID)
		}()
	}

	if callerID, ok, err := h.Store.ResolveUserID(ctx, caller); err != nil {
		fmt.Printf("shared games: resolve caller: %s\n", err)
	} else if ok {
		start(callerID, caller)
	}

	if len(friends) > 0 {
		states, err := h.Store.GameSyncStates(ctx, friends)
		if err != nil {
			fmt.Printf("shared games: sync states: %s\n", err)
		} else {
			for _, id := range friends {
				state, ok := states[id]
				if !ok {
					profile := profiles[id]
					profile.SteamID = id
					userID, err := h.Store.UpsertFriend(ctx, id, profile)
					if err != nil {
						fmt.Printf("shared games: create friend %s: %s\n", id, err)
						continue
					}
					state = db.GameSyncState{SteamID64: id, UserID: userID, State: db.GameSyncNeverSynced}
				}
				start(state.UserID, id)
			}
		}
	}
	wg.Wait()
}

// memberSyncConcurrency caps one request's concurrent member syncs. Each holds
// one pool connection for its sync lock and runs its queries on it (they use
// the lock's context), so the cap keeps a connection free on the default-sized
// pool.
const memberSyncConcurrency = 3

// syncRecordTimeout bounds recording a sync outcome, which runs detached from
// the request's budget so an expired budget is still recorded.
const syncRecordTimeout = 5 * time.Second

// syncMemberIfDue syncs one member's library when due, under the member's sync
// lock. A member whose lock cannot be taken before ctx ends is left without an
// attempt (and so without a recorded outcome).
//
// Assumed decision: a shared-games member sync stamps success once the library
// is stored, without the friends refresh the page-triggered sync also runs.
func (h *SharedGamesList) syncMemberIfDue(ctx context.Context, steam SteamService, userID int64, steamID string) error {
	due, err := h.Store.LibrarySyncDue(ctx, userID)
	if err != nil {
		return fmt.Errorf("sync games %s: %w", steamID, err)
	}
	if !due {
		return nil
	}
	// Every store call below uses the lock's ctx (or one derived from it), so
	// it runs on the lock connection rather than taking a second one.
	return h.Store.WithUserSyncLock(ctx, userID, func(ctx context.Context) error {
		// Another sync may have finished while this one waited on the lock.
		due, err := h.Store.LibrarySyncDue(ctx, userID)
		if err != nil {
			return fmt.Errorf("sync games %s: %w", steamID, err)
		}
		if !due {
			return nil
		}
		syncErr := SyncUserGames(ctx, steam, h.Store, userID, steamID)
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncRecordTimeout)
		defer cancel()
		if syncErr == nil || errors.Is(syncErr, ErrGameDetails) {
			if err := h.Store.RecordSyncSuccess(recordCtx, userID); err != nil {
				return errors.Join(syncErr, err)
			}
			return syncErr
		}
		return errors.Join(syncErr, h.Store.RecordSyncFailure(recordCtx, userID, syncreason.Reason(syncErr)))
	})
}

func renderErrorMessage(ctx *middleware.CustomContext, w http.ResponseWriter, msg string) {
	if err := template.ErrorMessage(msg).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

// sortFriends lists friends who are in a game first, then online, then offline,
// alphabetically within each group.
func sortFriends(friends []steamapi.Player) {
	rank := func(p steamapi.Player) int {
		switch {
		case p.GameExtraInfo != "":
			return 0
		case p.PersonaState != 0:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(friends, func(i, j int) bool {
		if ri, rj := rank(friends[i]), rank(friends[j]); ri != rj {
			return ri < rj
		}
		return strings.ToLower(friends[i].PersonaName) < strings.ToLower(friends[j].PersonaName)
	})
}
