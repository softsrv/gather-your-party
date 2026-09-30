package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/template"
	"gather-your-party/internal/view"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/softsrv/steamapi/steamapi"
)

var steamOpenIDEndpoint = "https://steamcommunity.com/openid/login"

// appConfig holds settings for session security and the public OpenID origin.
type appConfig struct {
	SessionSecret string
	AppBaseURL    string
}

type sessionStore interface {
	UpsertUser(context.Context, string, steamapi.Player) (int64, error)
	CreateSession(context.Context, int64) (string, error)
	ResolveSession(context.Context, string) (string, bool, error)
	DeleteSession(context.Context, string) error
	UserProfile(context.Context, string) (steamapi.Player, bool, error)
	ResolveUserID(context.Context, string) (int64, bool, error)
	CreateParty(context.Context, int64, string) (string, error)
	UserParties(context.Context, int64) ([]db.UserParty, error)
	UserPendingInvites(context.Context, int64) ([]db.UserPendingInvite, error)
	PartyMembers(context.Context, string) ([]db.PartyMember, error)
	IsMember(context.Context, string, int64) (bool, error)
	LeaveParty(context.Context, string, int64) error
	StepDown(ctx context.Context, partyID string, actingLeaderUserID int64, targetUserID int64) error
	SendInvite(context.Context, string, int64, int64) error
	AcceptInvite(context.Context, string, int64) error
	RejectInvite(context.Context, string, int64) error
	InviteCandidates(context.Context, string, int64, []string) ([]db.InviteCandidate, error)
}

type application struct {
	store  sessionStore
	config appConfig
}

func main() {
	_ = godotenv.Load()
	config := appConfig{
		SessionSecret: os.Getenv("SESSION_SECRET"),
		AppBaseURL:    os.Getenv("APP_BASE_URL"),
	}
	// Without these, Steam rejects the OpenID return_to ("invalid return protocol")
	// or the callback silently refuses every sign-in.
	if config.SessionSecret == "" {
		fmt.Fprintln(os.Stderr, "SESSION_SECRET is required")
		os.Exit(1)
	}
	if base, err := url.Parse(config.AppBaseURL); err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		fmt.Fprintln(os.Stderr, "APP_BASE_URL must be an absolute http(s) origin, e.g. http://localhost:8080")
		os.Exit(1)
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

func (app *application) handleSteamLogin(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(steamOpenIDEndpoint)
	if err != nil {
		http.Error(w, "unable to start Steam sign-in", http.StatusInternalServerError)
		return
	}
	u.RawQuery = url.Values{
		"openid.ns":         {"http://specs.openid.net/auth/2.0"},
		"openid.mode":       {"checkid_setup"},
		"openid.identity":   {"http://specs.openid.net/auth/2.0/identifier_select"},
		"openid.claimed_id": {"http://specs.openid.net/auth/2.0/identifier_select"},
		"openid.return_to":  {app.config.AppBaseURL + "/auth/steam/callback"},
		"openid.realm":      {app.config.AppBaseURL},
	}.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (app *application) handleSteamCallback(w http.ResponseWriter, r *http.Request) {
	reject := func() { http.Redirect(w, r, "/", http.StatusSeeOther) }
	if app.config.SessionSecret == "" {
		reject()
		return
	}
	params := url.Values{}
	for key, values := range r.URL.Query() {
		if strings.HasPrefix(key, "openid.") {
			// Reject ambiguous assertions rather than validate one value and use another.
			if len(values) != 1 {
				reject()
				return
			}
			params[key] = append([]string(nil), values...)
		}
	}
	if params.Get("openid.mode") != "id_res" {
		reject()
		return
	}
	params.Set("openid.mode", "check_authentication")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, steamOpenIDEndpoint, strings.NewReader(params.Encode()))
	if err != nil {
		reject()
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		reject()
		return
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil || res.StatusCode != http.StatusOK || !assertionIsValid(string(body)) {
		reject()
		return
	}
	// Bind the verified assertion to this relying party and to Steam's signed identity.
	if params.Get("openid.ns") != "http://specs.openid.net/auth/2.0" ||
		params.Get("openid.op_endpoint") != steamOpenIDEndpoint ||
		params.Get("openid.return_to") != app.config.AppBaseURL+"/auth/steam/callback" ||
		params.Get("openid.identity") != params.Get("openid.claimed_id") {
		reject()
		return
	}
	signed := "," + params.Get("openid.signed") + ","
	for _, field := range []string{"op_endpoint", "return_to", "claimed_id", "identity", "response_nonce", "assoc_handle"} {
		if !strings.Contains(signed, ","+field+",") {
			reject()
			return
		}
	}
	verifiedSteamID64, err := steamID64FromClaimedID(params.Get("openid.claimed_id"))
	if err != nil {
		reject()
		return
	}
	client := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
	player, err := client.Player(ctx, verifiedSteamID64)
	if err != nil {
		reject()
		return
	}
	userID, err := app.store.UpsertUser(ctx, verifiedSteamID64, player)
	if err != nil {
		reject()
		return
	}
	token, err := app.store.CreateSession(ctx, userID)
	if err != nil {
		reject()
		return
	}
	mac := hmac.New(sha256.New, []byte(app.config.SessionSecret))
	mac.Write([]byte(token))
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token + "." + hex.EncodeToString(mac.Sum(nil)),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *application) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("session"); err == nil {
		if separator := strings.LastIndex(cookie.Value, "."); separator > 0 {
			// Clear the browser cookie even if server-side deletion fails.
			_ = app.store.DeleteSession(r.Context(), cookie.Value[:separator])
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleHome shows Steam sign-in, or the dashboard greeting from the profile stored
// at sign-in (no Steam call per page load).
func (app *application) handleHome(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	steamID, _ := ctx.Value(middleware.SteamID{}).(string)
	if steamID == "" {
		if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	player, found, err := app.store.UserProfile(r.Context(), steamID)
	if err != nil || !found {
		w.WriteHeader(http.StatusInternalServerError)
		if err := template.ErrorPage("Gather Your Party", "unable to load your profile").Render(ctx, w); err != nil {
			fmt.Printf("render error: %s\n", err)
		}
		return
	}
	if err := template.Home(player, "Gather Your Party", template.Main).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

// currentProfile loads the signed-in user's stored profile for the page chrome.
// The avatar is decorative, so a failed lookup falls back to a generic one.
func (app *application) currentProfile(ctx *middleware.CustomContext) steamapi.Player {
	steamID, _ := ctx.Value(middleware.SteamID{}).(string)
	if steamID == "" {
		return steamapi.Player{}
	}
	player, _, err := app.store.UserProfile(ctx.Context, steamID)
	if err != nil {
		fmt.Printf("profile lookup error: %s\n", err)
	}
	return player
}

// sessionUserID accepts identity only from the verified session middleware.
func (app *application) sessionUserID(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) (int64, bool) {
	steamID, ok := ctx.Value(middleware.SteamID{}).(string)
	if !ok || steamID == "" {
		http.Error(w, "sign in to manage parties", http.StatusUnauthorized)
		return 0, false
	}
	userID, found, err := app.store.ResolveUserID(r.Context(), steamID)
	if err != nil {
		http.Error(w, "unable to resolve user", http.StatusInternalServerError)
		return 0, false
	}
	if !found {
		http.Error(w, "sign in to manage parties", http.StatusUnauthorized)
		return 0, false
	}
	return userID, true
}

func (app *application) handleParties(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := app.sessionUserID(ctx, w, r)
	if !ok {
		return
	}
	parties, err := app.store.UserParties(r.Context(), userID)
	if err != nil {
		http.Error(w, "unable to load parties", http.StatusInternalServerError)
		return
	}
	invites, err := app.store.UserPendingInvites(r.Context(), userID)
	if err != nil {
		http.Error(w, "unable to load invites", http.StatusInternalServerError)
		return
	}
	current := make([]component.UserParty, 0, len(parties))
	for _, party := range parties {
		current = append(current, component.UserParty{PartyID: party.PartyID, Name: party.Name})
	}
	pending := make([]component.UserPendingInvite, 0, len(invites))
	for _, invite := range invites {
		pending = append(pending, component.UserPendingInvite{PartyID: invite.PartyID, Name: invite.Name})
	}
	if err := template.PartiesPage(app.currentProfile(ctx), current, pending).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func (app *application) handleCreateParty(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	userID, ok := app.sessionUserID(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	partyID, err := app.store.CreateParty(r.Context(), userID, r.PostForm.Get("name"))
	if err != nil {
		http.Error(w, "unable to create party", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		// Navigate to the new party instead of swapping an empty response into the form.
		w.Header().Set("HX-Redirect", "/parties/"+partyID)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/parties/"+partyID, http.StatusSeeOther)
}

// leaveIdentity accepts identity only from the verified session middleware.
func (app *application) leaveIdentity(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	steamID, ok := ctx.Value(middleware.SteamID{}).(string)
	if !ok || steamID == "" {
		http.Error(w, "sign in to leave a party", http.StatusUnauthorized)
		return "", 0, false
	}
	partyID := r.PathValue("partyID")
	var id pgtype.UUID
	if err := id.Scan(partyID); err != nil || !id.Valid {
		http.Error(w, "invalid party ID", http.StatusBadRequest)
		return "", 0, false
	}
	userID, found, err := app.store.ResolveUserID(r.Context(), steamID)
	if err != nil {
		http.Error(w, "unable to resolve user", http.StatusInternalServerError)
		return "", 0, false
	}
	if !found {
		http.Error(w, "sign in to leave a party", http.StatusUnauthorized)
		return "", 0, false
	}
	return partyID, userID, true
}

func (app *application) handlePartyDetail(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, userID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	member, err := app.store.IsMember(r.Context(), partyID, userID)
	if err != nil {
		http.Error(w, "unable to check membership", http.StatusInternalServerError)
		return
	}
	if !member {
		http.Error(w, "party members only", http.StatusForbidden)
		return
	}
	members, err := app.store.PartyMembers(r.Context(), partyID)
	if err != nil {
		http.Error(w, "unable to load party members", http.StatusInternalServerError)
		return
	}
	roster := make([]component.PartyMember, 0, len(members))
	isLeader := false
	for _, member := range members {
		roster = append(roster, component.PartyMember{Name: member.Name, AvatarURL: member.AvatarURL, IsLeader: member.IsLeader})
		if member.UserID == userID && member.IsLeader {
			isLeader = true
		}
	}
	// The name is only a heading, so a lookup failure falls back to a generic title.
	var partyName string
	if parties, err := app.store.UserParties(r.Context(), userID); err == nil {
		for _, party := range parties {
			if party.PartyID == partyID {
				partyName = party.Name
			}
		}
	}
	var inviteCandidates []component.InviteCandidate
	var stepDownCandidates []component.StepDownCandidate
	if isLeader {
		service := steamapi.NewClient(os.Getenv("STEAM_API_KEY"))
		deadline := time.Now().Add(5000 * time.Millisecond)
		newCtx, cancelCtx := context.WithDeadline(ctx.Context, deadline)
		defer cancelCtx()
		steamID := ctx.Value(middleware.SteamID{}).(string)
		inviteCandidates, err = view.BuildInviteCandidates(newCtx, service, app.store, partyID, userID, steamID)
		if err != nil {
			http.Error(w, "unable to load invite candidates", http.StatusInternalServerError)
			return
		}
		for _, member := range members {
			if member.UserID != userID {
				stepDownCandidates = append(stepDownCandidates, component.StepDownCandidate{UserID: member.UserID, Name: member.Name, AvatarURL: member.AvatarURL})
			}
		}
	}
	if err := template.PartyDetail(app.currentProfile(ctx), partyID, partyName, roster, isLeader, inviteCandidates, stepDownCandidates).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func (app *application) handleLeaveConfirmation(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, _, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := template.LeaveParty(app.currentProfile(ctx), partyID).Render(ctx, w); err != nil {
		fmt.Printf("render error: %s\n", err)
	}
}

func (app *application) handleLeaveParty(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, userID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil || r.PostForm.Get("confirm") != "yes" {
		http.Error(w, "confirm before leaving the party", http.StatusBadRequest)
		return
	}
	if err := app.store.LeaveParty(r.Context(), partyID, userID); err != nil {
		http.Error(w, "unable to leave party", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// An empty successful response; the page then navigates back to /parties.
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *application) handleStepDown(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, actingUserID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	targetID, err := strconv.ParseInt(r.PostForm.Get("target"), 10, 64)
	if err != nil {
		http.Error(w, "invalid target", http.StatusBadRequest)
		return
	}
	if err := app.store.StepDown(r.Context(), partyID, actingUserID, targetID); err != nil {
		http.Error(w, "unable to step down", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *application) handleSendInvite(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, actingUserID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	targetID, err := strconv.ParseInt(r.PostForm.Get("target"), 10, 64)
	if err != nil {
		http.Error(w, "invalid target", http.StatusBadRequest)
		return
	}
	if err := app.store.SendInvite(r.Context(), partyID, actingUserID, targetID); err != nil {
		http.Error(w, "unable to send invite", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *application) handleAcceptInvite(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, actingUserID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := app.store.AcceptInvite(r.Context(), partyID, actingUserID); err != nil {
		http.Error(w, "unable to accept invite", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *application) handleRejectInvite(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, actingUserID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := app.store.RejectInvite(r.Context(), partyID, actingUserID); err != nil {
		http.Error(w, "unable to reject invite", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func assertionIsValid(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSuffix(line, "\r") == "is_valid:true" {
			return true
		}
	}
	return false
}

func steamID64FromClaimedID(claimedID string) (string, error) {
	const prefix = "https://steamcommunity.com/openid/id/"
	if !strings.HasPrefix(claimedID, prefix) {
		return "", fmt.Errorf("invalid Steam claimed ID")
	}
	id := strings.TrimPrefix(claimedID, prefix)
	if len(id) != 17 || strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return "", fmt.Errorf("invalid SteamID64")
	}
	return id, nil
}

func (app *application) routes() http.Handler {
	auth := &middleware.Authenticator{Resolver: app.store, Secret: []byte(app.config.SessionSecret)}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /favicon.ico", view.ServeFavicon)
	mux.HandleFunc("GET /static/", view.ServeStaticFiles)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleHome, auth.LoadSteamId)
	})
	mux.HandleFunc("GET /auth/steam", app.handleSteamLogin)
	mux.HandleFunc("GET /auth/steam/callback", app.handleSteamCallback)
	mux.HandleFunc("POST /auth/steam/logout", app.handleLogout)
	mux.HandleFunc("GET /frag/games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.GamesList, auth.LoadSteamId)
	})
	mux.HandleFunc("GET /frag/friends", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.FriendsList, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /frag/shared-games", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, view.SharedGamesList, auth.LoadSteamId)
	})

	mux.HandleFunc("GET /parties", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleParties, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleCreateParty, auth.LoadSteamId)
	})
	mux.HandleFunc("GET /parties/{partyID}", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handlePartyDetail, auth.LoadSteamId)
	})
	mux.HandleFunc("GET /parties/{partyID}/leave", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleLeaveConfirmation, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/leave", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleLeaveParty, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/step-down", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleStepDown, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/invites/send", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleSendInvite, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/invites/accept", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleAcceptInvite, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/invites/reject", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleRejectInvite, auth.LoadSteamId)
	})
	return mux
}

func (app *application) serve() {
	fmt.Printf("server is running on port %s\n", os.Getenv("LISTEN_ADDR"))
	err := http.ListenAndServe(":"+os.Getenv("LISTEN_ADDR"), app.routes())
	if err != nil {
		fmt.Println(err)
	}

}
