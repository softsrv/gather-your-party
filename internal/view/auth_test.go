package view

import (
	"context"
	"errors"
	"gather-your-party/internal/auth"
	"gather-your-party/internal/authopenid"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/store"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

// ---- fakes -------------------------------------------------------------

type fakeAuthStore struct {
	upsertCalls  []upsertCall
	createCalls  []createCall
	returnedUser store.User
	upsertErr    error
	createErr    error
}

type upsertCall struct{ steamID, persona, avatar string }
type createCall struct {
	token  string
	userID int64
}

func (f *fakeAuthStore) UpsertUser(ctx context.Context, steamID, persona, avatar string) (store.User, error) {
	f.upsertCalls = append(f.upsertCalls, upsertCall{steamID, persona, avatar})
	if f.upsertErr != nil {
		return store.User{}, f.upsertErr
	}
	if f.returnedUser.SteamID64 == "" {
		f.returnedUser = store.User{ID: 42, SteamID64: steamID, Persona: persona, AvatarURL: avatar}
	}
	return f.returnedUser, nil
}

func (f *fakeAuthStore) CreateSession(ctx context.Context, token string, userID int64) (store.Session, error) {
	f.createCalls = append(f.createCalls, createCall{token, userID})
	if f.createErr != nil {
		return store.Session{}, f.createErr
	}
	return store.Session{Token: token, UserID: userID}, nil
}

type fakePlayers struct{ players []steamapi.Player }

func (f *fakePlayers) Players(ctx context.Context, ids []string) ([]steamapi.Player, error) {
	return f.players, nil
}

type fakeLogoutStore struct {
	deleted []string
	err     error
}

func (f *fakeLogoutStore) DeleteSession(ctx context.Context, token string) error {
	f.deleted = append(f.deleted, token)
	return f.err
}

// ---- CLM-1 -----------------------------------------------------------

// CLM-1: GET /login → 3xx redirect to Steam's OpenID endpoint with
// realm/return_to derived from APP_BASE_URL.
func TestLoginRedirect_303ToSteamWithAppBaseURL(t *testing.T) {
	req := httptest.NewRequest("GET", "/login", nil)
	// Deliberately set r.Host to a value that MUST NOT appear in the redirect
	// — this discriminates against a wrongly-implemented handler that
	// derived origin from r.Host. CLM-2 discriminator.
	req.Host = "attacker.example.com"
	w := httptest.NewRecorder()

	ctx := &middleware.CustomContext{
		Context:    context.Background(),
		AppBaseURL: "https://myapp.example.org",
	}
	LoginRedirect(ctx, w, req)

	if w.Code < 300 || w.Code >= 400 {
		t.Fatalf("status = %d, want 3xx", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("no Location header on redirect")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location parse err = %v", err)
	}
	if u.Host != "steamcommunity.com" || u.Path != "/openid/login" {
		t.Fatalf("Location host/path = %s%s, want steamcommunity.com/openid/login", u.Host, u.Path)
	}
	q := u.Query()
	realm := q.Get("openid.realm")
	returnTo := q.Get("openid.return_to")
	if realm != "https://myapp.example.org" {
		t.Errorf("openid.realm = %q, want %q", realm, "https://myapp.example.org")
	}
	if returnTo != "https://myapp.example.org"+CallbackPath {
		t.Errorf("openid.return_to = %q, want %q", returnTo, "https://myapp.example.org"+CallbackPath)
	}
	// CLM-2 discriminator: the attacker Host MUST NOT leak into the redirect.
	if strings.Contains(loc, "attacker.example.com") {
		t.Fatalf("Location contains request Host — realm/return_to leaked from r.Host (CLM-2 breach): %s", loc)
	}
}

// ---- CLM-5, CLM-6, CLM-7, CLM-8 -------------------------------------

// CLM-5 + CLM-7 + CLM-8: verified callback upserts, creates session, sets cookie.
func TestSteamCallback_Success_UpsertsCreatesAndSetsSignedCookie(t *testing.T) {
	prev := authopenid.VerifyFn
	authopenid.VerifyFn = func(fullURL string) (string, error) {
		return "https://steamcommunity.com/openid/id/76561198000000123", nil
	}
	defer func() { authopenid.VerifyFn = prev }()

	fs := &fakeAuthStore{}
	fp := &fakePlayers{players: []steamapi.Player{{SteamID: "76561198000000123", PersonaName: "TestPersona", AvatarFull: "https://cdn/avatar.jpg"}}}
	restore := setCallbackOverrides(fs, fp)
	defer restore()

	req := httptest.NewRequest("GET", CallbackPath+"?openid.mode=id_res&openid.claimed_id=whatever", nil)
	w := httptest.NewRecorder()
	ctx := &middleware.CustomContext{
		Context:       context.Background(),
		AppBaseURL:    "https://myapp.example.org",
		SessionSecret: "s3cret",
	}
	SteamCallback(ctx, w, req)

	if len(fs.upsertCalls) != 1 {
		t.Fatalf("UpsertUser calls = %d, want 1", len(fs.upsertCalls))
	}
	u := fs.upsertCalls[0]
	if u.steamID != "76561198000000123" || u.persona != "TestPersona" || u.avatar != "https://cdn/avatar.jpg" {
		t.Errorf("UpsertUser args = %+v, want steamID/persona/avatar from Steam API", u)
	}
	if len(fs.createCalls) != 1 {
		t.Fatalf("CreateSession calls = %d, want 1", len(fs.createCalls))
	}
	if fs.createCalls[0].userID != 42 {
		t.Errorf("CreateSession userID = %d, want 42", fs.createCalls[0].userID)
	}
	// Cookie: assert it exists and its value HMAC-verifies to the token
	// that CreateSession was called with (CLM-8, CLM-10).
	sc := w.Result().Cookies()
	if len(sc) == 0 {
		t.Fatal("no Set-Cookie on success")
	}
	var sessionCookie *http.Cookie
	for _, c := range sc {
		if c.Name == auth.SessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("no cookie named %q; got %+v", auth.SessionCookieName, sc)
	}
	tok, ok := auth.VerifyToken(sessionCookie.Value, "s3cret")
	if !ok {
		t.Fatal("cookie value does not HMAC-verify")
	}
	if tok != fs.createCalls[0].token {
		t.Errorf("cookie token = %q, CreateSession token = %q", tok, fs.createCalls[0].token)
	}
	// CLM-11 cookie attrs at Set-Cookie level too.
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie attrs weak: HttpOnly=%v Secure=%v SameSite=%v", sessionCookie.HttpOnly, sessionCookie.Secure, sessionCookie.SameSite)
	}
	if sessionCookie.MaxAge != 30*24*60*60 {
		t.Errorf("cookie MaxAge = %d, want %d", sessionCookie.MaxAge, 30*24*60*60)
	}
}

// CLM-6: failed verify creates NO session and sets NO session cookie.
// Discriminator: this test goes red if the success path is reachable on
// a failed verify.
func TestSteamCallback_VerifyFailure_NoSessionNoCookie(t *testing.T) {
	prev := authopenid.VerifyFn
	authopenid.VerifyFn = func(fullURL string) (string, error) {
		return "", errors.New("openid check_authentication failed")
	}
	defer func() { authopenid.VerifyFn = prev }()

	fs := &fakeAuthStore{}
	fp := &fakePlayers{}
	restore := setCallbackOverrides(fs, fp)
	defer restore()

	req := httptest.NewRequest("GET", CallbackPath+"?openid.mode=id_res", nil)
	w := httptest.NewRecorder()
	ctx := &middleware.CustomContext{
		Context:       context.Background(),
		AppBaseURL:    "https://myapp.example.org",
		SessionSecret: "s3cret",
	}
	SteamCallback(ctx, w, req)

	if len(fs.upsertCalls) != 0 {
		t.Errorf("UpsertUser called %d times on failed verify; want 0 (CLM-6)", len(fs.upsertCalls))
	}
	if len(fs.createCalls) != 0 {
		t.Errorf("CreateSession called %d times on failed verify; want 0 (CLM-6)", len(fs.createCalls))
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			t.Errorf("session cookie set on failed verify (CLM-6 breach)")
		}
	}
	if w.Code == http.StatusOK || (w.Code >= 300 && w.Code < 400) {
		// A signed-in response would be 200 OK or 3xx redirect. Verify
		// failure must NOT return one of those.
		// Our handler responds 401 in this path.
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status on failed verify = %d, want 4xx (not sign-in)", w.Code)
		}
	}
}

// CLM-5: SteamIDFromClaimedID extracts the trailing steamid64 from a
// well-formed claimed_id URL.
func TestSteamIDFromClaimedID(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		wantOK bool
	}{
		{"https://steamcommunity.com/openid/id/76561198000000001", "76561198000000001", true},
		{"https://steamcommunity.com/openid/id/76561198000000001/", "76561198000000001", true},
		{"https://steamcommunity.com/openid/id/", "", false},
		{"https://steamcommunity.com/openid/id/not-a-number", "", false},
		{"garbage", "", false},
	}
	for _, tc := range cases {
		got, ok := SteamIDFromClaimedID(tc.in)
		if ok != tc.wantOK || got != tc.wantID {
			t.Errorf("SteamIDFromClaimedID(%q) = %q,%v; want %q,%v", tc.in, got, ok, tc.wantID, tc.wantOK)
		}
	}
}

// ---- CLM-16 logout --------------------------------------------------

// CLM-16: logout calls DeleteSession(token) and emits a clearing cookie.
func TestLogout_DeletesSessionAndClearsCookie(t *testing.T) {
	secret := "s3cret"
	token := "opaque-tok"
	signed := auth.SignToken(token, secret)

	fs := &fakeLogoutStore{}
	restore := setLogoutOverride(fs)
	defer restore()

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: signed})
	w := httptest.NewRecorder()

	ctx := &middleware.CustomContext{
		Context:       context.Background(),
		SessionSecret: secret,
	}
	Logout(ctx, w, req)

	if len(fs.deleted) != 1 || fs.deleted[0] != token {
		t.Fatalf("DeleteSession calls = %+v, want [%q]", fs.deleted, token)
	}
	// Clearing cookie: MaxAge <= 0.
	var clearing *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			clearing = c
		}
	}
	if clearing == nil {
		t.Fatal("no session cookie emitted by logout")
	}
	if clearing.MaxAge >= 0 {
		t.Errorf("logout cookie MaxAge = %d, want < 0 (clearing)", clearing.MaxAge)
	}
}

// CLM-16 negative: logout with no cookie must not crash, no DeleteSession call.
func TestLogout_NoCookie_NoDeleteCall(t *testing.T) {
	fs := &fakeLogoutStore{}
	restore := setLogoutOverride(fs)
	defer restore()

	req := httptest.NewRequest("POST", "/logout", nil)
	w := httptest.NewRecorder()
	ctx := &middleware.CustomContext{
		Context:       context.Background(),
		SessionSecret: "s3cret",
	}
	Logout(ctx, w, req)

	if len(fs.deleted) != 0 {
		t.Fatalf("DeleteSession called with no cookie present: %+v", fs.deleted)
	}
}
