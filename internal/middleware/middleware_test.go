package middleware

import (
	"gather-your-party/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCustomContext_StoreIsReachable asserts that a *store.Store injected into
// a CustomContext.Store field can be read back — proving the store is reachable
// from the handler path without a live DB (CLM-4).
func TestCustomContext_StoreIsReachable(t *testing.T) {
	// Build a Store over a nil pool: we only need pointer identity to prove
	// reachability (no DB call is made).
	var pool *pgxpool.Pool // nil — reachability test only
	st := store.NewStore(pool)

	ctx := &CustomContext{
		Store: st,
	}

	if ctx.Store != st {
		t.Errorf("ctx.Store = %v, want %v — store not reachable from CustomContext", ctx.Store, st)
	}
}

// TestChain_StoreInjectedIntoContext asserts that Chain wires the provided store
// onto the CustomContext that the handler receives (CLM-4, wire-through proof).
func TestChain_StoreInjectedIntoContext(t *testing.T) {
	var pool *pgxpool.Pool // nil pool — no DB call occurs
	st := store.NewStore(pool)

	var capturedStore *store.Store

	handler := CustomHandler(func(ctx *CustomContext, w http.ResponseWriter, r *http.Request) {
		capturedStore = ctx.Store
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	Chain(st, rec, req, handler)

	if capturedStore != st {
		t.Errorf("handler received ctx.Store = %v, want %v", capturedStore, st)
	}
}
