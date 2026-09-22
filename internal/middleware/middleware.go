package middleware

import (
	"context"
	"fmt"
	"gather-your-party/internal/store"
	"net/http"
	"time"
)

type CustomContext struct {
	context.Context
	StartTime time.Time
	Store     *store.Store // CLM-4: store is reachable from the handler/view path
}

type CustomHandler func(ctx *CustomContext, w http.ResponseWriter, r *http.Request)
type CustomMiddleware func(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error

// Chain builds a CustomContext (with the injected store) and runs
// the middleware chain before calling the handler. The store parameter
// wires the persistence layer into every handler that receives a *CustomContext.
func Chain(st *store.Store, w http.ResponseWriter, r *http.Request, handler CustomHandler, middleware ...CustomMiddleware) {
	fmt.Println("Starting teh middleware chain")
	customContext := &CustomContext{
		Context:   context.Background(),
		StartTime: time.Now(),
		Store:     st, // CLM-4: injected at request-path construction time
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
	Log(customContext, w, r)
	fmt.Println("done with logger")
}

func Log(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	elapsedTime := time.Since(ctx.StartTime)
	formattedTime := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] [%s] [%s] [%s]\n", formattedTime, r.Method, r.URL.Path, elapsedTime)
	return nil
}

func ParseForm(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	r.ParseForm()
	fmt.Printf("%+v\n", r.Form)
	return nil
}

func ParseMultipartForm(ctx *CustomContext, w http.ResponseWriter, r *http.Request) error {
	r.ParseMultipartForm(10 << 20)
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

	ctx.Context = context.WithValue(ctx.Context, "steamID", cookie.Value)
	return nil
}
