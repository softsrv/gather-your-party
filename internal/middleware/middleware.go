package middleware

import (
	"context"
	"errors"
	"fmt"
	"gather-your-party/internal/auth"
	"gather-your-party/internal/store"
	"net/http"
	"time"
)

// SessionResolver is the minimal store surface ResolveSession needs. It
// is satisfied by *store.Store; a fake in tests records lookup calls to
// prove CLM-13 (no lookup on a bad MAC) and CLM-14 (lookup happens on a
// valid MAC).
type SessionResolver interface {
	GetSteamIDByToken(ctx context.Context, token string) (string, error)
}

// CustomContext carries request-scoped state to handlers. AppBaseURL,
// SessionSecret, and SteamAPIKey are threaded from main.go's cfg via
// Chain so handlers (login redirect, callback, resolve-session
// middleware, logout) never need to read the environment or the request
// host to construct auth URLs, verify cookies, or hit the Steam Web API.
// CLM-2, CLM-4.
type CustomContext struct {
	context.Context
	StartTime     time.Time
	Store         *store.Store
	AppBaseURL    string
	SessionSecret string
	SteamAPIKey   string
	// SessionResolver is what ResolveSession consults. It defaults to
	// Store in Chain; tests may replace it with a fake before invoking
	// ResolveSession directly.
	SessionResolver SessionResolver
}

type CustomHandler func(ctx *CustomContext, w http.ResponseWriter, r *http.Request)
type CustomMiddleware func(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error

// Chain builds a CustomContext (with the injected store + config) and runs
// the middleware chain before calling the handler.
func Chain(st *store.Store, appBaseURL, sessionSecret, steamAPIKey string, w http.ResponseWriter, r *http.Request, handler CustomHandler, middleware ...CustomMiddleware) {
	customContext := &CustomContext{
		Context:         context.Background(),
		StartTime:       time.Now(),
		Store:           st,
		AppBaseURL:      appBaseURL,
		SessionSecret:   sessionSecret,
		SteamAPIKey:     steamAPIKey,
		SessionResolver: st,
	}
	for _, mw := range middleware {
		if err := mw(customContext, w, r); err != nil {
			fmt.Printf("middleware error: %s\n", err)
			return
		}
	}
	handler(customContext, w, r)
	_ = Log(customContext, w, r)
}

func Log(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	elapsedTime := time.Since(ctx.StartTime)
	formattedTime := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] [%s] [%s] [%s]\n", formattedTime, r.Method, r.URL.Path, elapsedTime)
	return nil
}

func ParseForm(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	r.ParseForm()
	return nil
}

func ParseMultipartForm(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	r.ParseMultipartForm(10 << 20)
	return nil
}

// ResolveSession is the successor to the retired LoadSteamId middleware.
// It reads the signed session cookie, verifies the HMAC, and (only on
// valid HMAC) looks the opaque token up via store.GetSteamIDByToken. On
// success the resolved SteamID64 is placed on the request context under
// the pre-existing "steamID" key so the four data views (Home, GamesList,
// FriendsList, SharedGamesList) read it exactly as they do today. CLM-14.
//
// A missing cookie, an HMAC-invalid cookie, or an expired session (store
// returns ErrNotFound) all leave the request in the signed-out state with
// no "steamID" in context, mirroring today's behaviour for a request with
// no cookie. CLM-13, CLM-15.
//
// Never logs the token value or the session secret. CLM-12.
func ResolveSession(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		// No cookie ⇒ signed-out. Not an error.
		return nil
	}

	// CLM-13: verify HMAC BEFORE any DB lookup. If invalid, fall through
	// to signed-out without calling store.GetSteamIDByToken.
	token, ok := auth.VerifyToken(cookie.Value, ctx.SessionSecret)
	if !ok {
		return nil
	}

	steamID, err := ctx.SessionResolver.GetSteamIDByToken(ctx.Context, token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Absent or expired session ⇒ signed-out. CLM-15.
			return nil
		}
		// Real DB error: surface signed-out without leaking token to log.
		return nil
	}

	ctx.Context = context.WithValue(ctx.Context, "steamID", steamID)
	return nil
}
