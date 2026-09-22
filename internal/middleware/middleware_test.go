package middleware

import (
	"context"
	"gather-your-party/internal/auth"
	"gather-your-party/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeResolver records every GetSteamIDByToken call so tests can prove
// zero-lookup (CLM-13) and lookup-happens (CLM-14) invariants.
type fakeResolver struct {
	calls     []string
	returnID  string
	returnErr error
}

func (f *fakeResolver) GetSteamIDByToken(ctx context.Context, token string) (string, error) {
	f.calls = append(f.calls, token)
	return f.returnID, f.returnErr
}

// CLM-13: a cookie whose HMAC does not validate MUST NOT trigger any
// session-table lookup; the request falls through to signed-out.
func TestResolveSession_TamperedMAC_NoLookup(t *testing.T) {
	secret := "s3cret"
	// Craft a value that looks like our format but has a wrong MAC.
	tampered := "sometoken|0000000000000000000000000000000000000000000000000000000000000000"
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tampered})
	w := httptest.NewRecorder()

	fake := &fakeResolver{}
	ctx := &CustomContext{
		Context:         context.Background(),
		Store:           nil,
		SessionSecret:   secret,
		SessionResolver: fake,
	}

	if err := ResolveSession(ctx, w, req); err != nil {
		t.Fatalf("ResolveSession err = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("GetSteamIDByToken called %d times on tampered cookie; want 0 (CLM-13)", len(fake.calls))
	}
	if ctx.Context.Value("steamID") != nil {
		t.Fatalf("steamID set in context on tampered cookie; want signed-out")
	}
}

// CLM-15: no cookie ⇒ signed-out, no lookup, no steamID.
func TestResolveSession_NoCookie_SignedOut(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	fake := &fakeResolver{}
	ctx := &CustomContext{
		Context:         context.Background(),
		SessionSecret:   "s",
		SessionResolver: fake,
	}
	if err := ResolveSession(ctx, w, req); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unexpected lookup on missing cookie: %v", fake.calls)
	}
	if ctx.Context.Value("steamID") != nil {
		t.Fatal("steamID set on missing cookie")
	}
}

// CLM-14: valid cookie + resolver returning a SteamID64 ⇒ context "steamID"
// is set to that SteamID.
func TestResolveSession_ValidCookie_SetsSteamID(t *testing.T) {
	secret := "s3cret"
	token := "opaque-token-xyz"
	signed := auth.SignToken(token, secret)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: signed})
	w := httptest.NewRecorder()

	fake := &fakeResolver{returnID: "76561198000000001"}
	ctx := &CustomContext{
		Context:         context.Background(),
		SessionSecret:   secret,
		SessionResolver: fake,
	}
	if err := ResolveSession(ctx, w, req); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 lookup, got %d", len(fake.calls))
	}
	if fake.calls[0] != token {
		t.Fatalf("lookup token = %q, want %q", fake.calls[0], token)
	}
	got := ctx.Context.Value("steamID")
	if got != "76561198000000001" {
		t.Fatalf("steamID in context = %v, want 76561198000000001", got)
	}
}

// CLM-15: valid cookie but store returns ErrNotFound (absent/expired) ⇒
// signed-out, no steamID in context.
func TestResolveSession_ExpiredOrAbsent_SignedOut(t *testing.T) {
	secret := "s3cret"
	token := "opaque-token"
	signed := auth.SignToken(token, secret)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: signed})
	w := httptest.NewRecorder()

	fake := &fakeResolver{returnErr: store.ErrNotFound}
	ctx := &CustomContext{
		Context:         context.Background(),
		SessionSecret:   secret,
		SessionResolver: fake,
	}
	if err := ResolveSession(ctx, w, req); err != nil {
		t.Fatalf("err = %v", err)
	}
	if ctx.Context.Value("steamID") != nil {
		t.Fatalf("steamID set on ErrNotFound; want signed-out (CLM-15)")
	}
}
