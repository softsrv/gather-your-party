package view

import (
	"context"
	"gather-your-party/internal/auth"
	"gather-your-party/internal/authopenid"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/store"
	"net/http"
	"strings"

	"github.com/softsrv/steamapi/steamapi"
)

// authUserStore is the minimal store surface the auth callback needs
// (matches *store.Store). Broken out so the callback is unit-testable
// with a fake. CLM-7, CLM-8.
type authUserStore interface {
	UpsertUser(ctx context.Context, steamID64, persona, avatarURL string) (store.User, error)
	CreateSession(ctx context.Context, token string, userID int64) (store.Session, error)
}

// authLogoutStore is the store surface the logout handler needs.
type authLogoutStore interface {
	DeleteSession(ctx context.Context, token string) error
}

// playersClient is the minimal steamapi surface the callback needs to
// hydrate persona + avatar. Satisfied by *steamapi.Client.
type playersClient interface {
	Players(ctx context.Context, ids []string) ([]steamapi.Player, error)
}

// injectable seams for unit tests. Production sets no override.
var (
	callbackStoreOverride  authUserStore
	callbackClientOverride playersClient
	logoutStoreOverride    authLogoutStore
	// callbackPath is the return_to path Steam redirects back to. Kept as
	// a package var so tests can align expectations without duplicating.
	CallbackPath = "/auth/steam/callback"
)

// LoginRedirect is the GET /login handler. It responds with a 303 See
// Other to Steam's OpenID 2.0 provider. openid.realm and
// openid.return_to are derived SOLELY from ctx.AppBaseURL (loaded at
// boot from APP_BASE_URL); the request's Host header / r.URL.Host are
// never consulted. CLM-1, CLM-2.
func LoginRedirect(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	realm := trimTrailingSlash(ctx.AppBaseURL)
	returnTo := realm + CallbackPath

	redirectURL, err := authopenid.RedirectURL(returnTo, realm)
	if err != nil {
		http.Error(w, "failed to build openid redirect", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

// SteamCallback is the GET /auth/steam/callback handler. It:
//   - verifies the OpenID assertion via authopenid.Verify (which performs
//     check_authentication + realm/return_to matching) — CLM-5.
//   - on failed verify, creates NO session and sets NO cookie — CLM-6.
//   - on success, extracts SteamID64 from claimed_id, hydrates persona +
//     avatar via the Steam Web API, upserts the user (CLM-7), creates a
//     session row (CLM-8), and sets a signed session cookie (CLM-10, CLM-11).
//
// Never logs SESSION_SECRET or the raw session token. CLM-12.
func SteamCallback(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	fullURL := authopenid.CallbackFullURL(ctx.AppBaseURL, r)

	claimedID, err := authopenid.Verify(fullURL)
	if err != nil {
		// CLM-6: MUST-forbid signing in on a failed verify. Return BEFORE
		// any CreateSession or Set-Cookie.
		http.Error(w, "openid verification failed", http.StatusUnauthorized)
		return
	}

	steamID64, ok := SteamIDFromClaimedID(claimedID)
	if !ok {
		http.Error(w, "unrecognised claimed_id", http.StatusBadRequest)
		return
	}

	// Hydrate persona + avatar. Fall back gracefully if the players list
	// is empty (we still create the session with empty persona/avatar).
	var persona, avatarURL string
	client := resolvePlayersClient(ctx)
	if client != nil {
		players, err := client.Players(ctx.Context, []string{steamID64})
		if err == nil && len(players) > 0 {
			persona = players[0].PersonaName
			avatarURL = players[0].AvatarFull
		}
	}

	sr := resolveAuthUserStore(ctx)
	user, err := sr.UpsertUser(ctx.Context, steamID64, persona, avatarURL)
	if err != nil {
		http.Error(w, "user upsert failed", http.StatusInternalServerError)
		return
	}

	token, err := auth.NewSessionToken()
	if err != nil {
		http.Error(w, "token generation failed", http.StatusInternalServerError)
		return
	}
	if _, err := sr.CreateSession(ctx.Context, token, user.ID); err != nil {
		http.Error(w, "session persist failed", http.StatusInternalServerError)
		return
	}

	signed := auth.SignToken(token, ctx.SessionSecret)
	http.SetCookie(w, auth.NewSessionCookie(signed))

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout is the POST /logout handler. It resolves the token from the
// signed cookie, calls DeleteSession, and emits a clearing Set-Cookie.
// CLM-16. Never logs the token.
func Logout(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		if token, ok := auth.VerifyToken(cookie.Value, ctx.SessionSecret); ok {
			ls := resolveLogoutStore(ctx)
			if ls != nil {
				_ = ls.DeleteSession(ctx.Context, token)
			}
		}
	}
	http.SetCookie(w, auth.ClearSessionCookie())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// SteamIDFromClaimedID parses the trailing path segment of a Steam
// claimed_id URL of the form "https://steamcommunity.com/openid/id/<steamid64>".
// Exported (and pure) so it's directly unit-testable.
func SteamIDFromClaimedID(claimedID string) (string, bool) {
	// Trim any trailing slash then take the last "/"-separated segment.
	s := strings.TrimRight(claimedID, "/")
	i := strings.LastIndex(s, "/")
	if i < 0 || i == len(s)-1 {
		return "", false
	}
	id := s[i+1:]
	if id == "" {
		return "", false
	}
	// Sanity: SteamID64 is all digits.
	for _, c := range id {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return id, true
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func resolveAuthUserStore(ctx *middleware.CustomContext) authUserStore {
	if callbackStoreOverride != nil {
		return callbackStoreOverride
	}
	return ctx.Store
}

func resolveLogoutStore(ctx *middleware.CustomContext) authLogoutStore {
	if logoutStoreOverride != nil {
		return logoutStoreOverride
	}
	return ctx.Store
}

func resolvePlayersClient(ctx *middleware.CustomContext) playersClient {
	if callbackClientOverride != nil {
		return callbackClientOverride
	}
	apiKey := ctx.SteamAPIKey
	if apiKey == "" {
		return nil
	}
	return steamapi.NewClient(apiKey)
}

// setCallbackOverrides is a test-only seam. Prod callers do not touch it.
// Kept as an exported-style function inside the package so *_test.go files
// in this package can set overrides without exposing them externally.
func setCallbackOverrides(s authUserStore, c playersClient) (restore func()) {
	prevS, prevC := callbackStoreOverride, callbackClientOverride
	callbackStoreOverride = s
	callbackClientOverride = c
	return func() {
		callbackStoreOverride = prevS
		callbackClientOverride = prevC
	}
}

func setLogoutOverride(s authLogoutStore) (restore func()) {
	prev := logoutStoreOverride
	logoutStoreOverride = s
	return func() { logoutStoreOverride = prev }
}

// keep import lines stable
