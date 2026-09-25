package middleware

import (
	"context"
	"fmt"
	"net/http"
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

func LoadSteamId(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	fmt.Println("inside LoadSteamId middleware")
	cookie, err := r.Cookie("steam_id")

	if err != nil {
		fmt.Println("got an error loading cookie")
		if err == http.ErrNoCookie {
			fmt.Println("cookie was not found")
			return nil
		}
	}
	fmt.Printf("got the cookie: %s\n", cookie.Value)

	ctx.Context = context.WithValue(ctx.Context, SteamID{}, cookie.Value)
	return nil
}
