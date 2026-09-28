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
	ResolveUserID(context.Context, string) (int64, bool, error)
	CreateParty(context.Context, int64, string) (string, error)
	LeaveParty(context.Context, string, int64) error
	StepDown(ctx context.Context, partyID string, actingLeaderUserID int64, targetUserID int64) error
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
	defer res.Body.Close()
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

// leaveIdentity accepts identity only from the verified session middleware.
func (app *application) leaveIdentity(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	steamID, ok := ctx.Context.Value(middleware.SteamID{}).(string)
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

func (app *application) handleLeaveConfirmation(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, _, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	if err := template.LeaveParty(partyID).Render(ctx, w); err != nil {
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
		// An empty successful response dismisses the confirmation via outerHTML.
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
		middleware.Chain(w, r, view.Home, auth.LoadSteamId)
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

	mux.HandleFunc("GET /parties/{partyID}/leave", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleLeaveConfirmation, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/leave", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleLeaveParty, auth.LoadSteamId)
	})
	mux.HandleFunc("POST /parties/{partyID}/step-down", func(w http.ResponseWriter, r *http.Request) {
		middleware.Chain(w, r, app.handleStepDown, auth.LoadSteamId)
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
