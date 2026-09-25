package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type CustomContext struct {
	context.Context
	StartTime time.Time
}

type SteamID struct{}

type CustomHandler func(ctx *CustomContext, w http.ResponseWriter, r *http.Request)
type CustomMiddleware func(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error

func Chain(w http.ResponseWriter, r *http.Request, handler CustomHandler, middleware ...CustomMiddleware) {
	fmt.Println("Starting teh middleware chain")
	customContext := &CustomContext{
		Context:   context.Background(),
		StartTime: time.Now(),
	}
	fmt.Println("done creating custom context")
	for _, mw := range middleware {
		err := mw(customContext, w, r)
		if err != nil {
			fmt.Printf("got an error: %s", err)
			return
		}
	}
	fmt.Println("done with middleware chain")
	handler(customContext, w, r)
	fmt.Println("done with hander")
	if err := Log(customContext, w, r); err != nil {
		fmt.Printf("logging error: %s\n", err)
	}
	fmt.Println("done with logger")
}

func Log(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	elapsedTime := time.Since(ctx.StartTime)
	formattedTime := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] [%s] [%s] [%s]\n", formattedTime, r.Method, r.URL.Path, elapsedTime)
	return nil
}

func ParseForm(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return err
	}
	fmt.Printf("%+v\n", r.Form)
	return nil
}

func ParseMultipartForm(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return err
	}
	return nil
}

type SessionResolver interface {
	ResolveSession(context.Context, string) (string, bool, error)
}

type Authenticator struct {
	Resolver SessionResolver
	Secret   []byte
}

func (a *Authenticator) LoadSteamId(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie("session")
	if err != nil || len(a.Secret) == 0 {
		return nil
	}
	token, macHex, ok := strings.Cut(cookie.Value, ".")
	if !ok || token == "" {
		return nil
	}
	gotMAC, err := hex.DecodeString(macHex)
	if err != nil {
		return nil
	}
	mac := hmac.New(sha256.New, a.Secret)
	mac.Write([]byte(token))
	if !hmac.Equal(mac.Sum(nil), gotMAC) {
		return nil
	}
	steamID64, ok, err := a.Resolver.ResolveSession(r.Context(), token)
	if err != nil || !ok {
		return nil
	}
	ctx.Context = context.WithValue(ctx.Context, SteamID{}, steamID64)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    cookie.Value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	})
	return nil
}
