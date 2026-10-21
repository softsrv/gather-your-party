package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"
	"gather-your-party/internal/view"

	"github.com/softsrv/steamapi/steamapi"
)

const (
	calTestSecret   = "calendar-unit-secret"
	calTestClientID = "client-id-123.apps.googleusercontent.com"
	calTestClientSc = "client-secret-xyz"
	calTestBaseURL  = "https://gyp.example.com"
	userASteamID    = "76561197960287930"
	userBSteamID    = "76561197960287931"
	userAToken      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	userBToken      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	userAID         = int64(101)
	userBID         = int64(202)
)

// calendarStore is a session store double with two signed-in users and an
// in-memory Google Calendar connection table that records every call.
type calendarStore struct {
	fakeSessionStore
	t          *testing.T
	mu         sync.Mutex
	conns      map[int64]db.GoogleCalendarConnection
	connCalls  []string
	getErr     error
	deleteErr  error
	forbidSave bool // fail the test on any save or delete
}

func newCalendarStore(t *testing.T) *calendarStore {
	return &calendarStore{t: t, conns: map[int64]db.GoogleCalendarConnection{}}
}

func (s *calendarStore) ResolveSession(_ context.Context, token string) (string, bool, error) {
	switch token {
	case userAToken:
		return userASteamID, true, nil
	case userBToken:
		return userBSteamID, true, nil
	}
	return "", false, nil
}

func (s *calendarStore) ResolveUserID(_ context.Context, steamID string) (int64, bool, error) {
	switch steamID {
	case userASteamID:
		return userAID, true, nil
	case userBSteamID:
		return userBID, true, nil
	}
	return 0, false, nil
}

func (s *calendarStore) UserProfile(_ context.Context, steamID string) (steamapi.Player, bool, error) {
	return steamapi.Player{SteamID: steamID, PersonaName: "Player " + steamID[len(steamID)-2:]}, true, nil
}

func (s *calendarStore) SaveGoogleCalendarConnection(_ context.Context, userID int64, c db.GoogleCalendarConnection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forbidSave {
		s.t.Errorf("unexpected SaveGoogleCalendarConnection(%d)", userID)
	}
	s.connCalls = append(s.connCalls, "save")
	c.UserID = userID
	s.conns[userID] = c
	return nil
}

func (s *calendarStore) GoogleCalendarConnection(_ context.Context, userID int64) (db.GoogleCalendarConnection, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connCalls = append(s.connCalls, "get")
	if s.getErr != nil {
		return db.GoogleCalendarConnection{}, false, s.getErr
	}
	c, ok := s.conns[userID]
	return c, ok, nil
}

func (s *calendarStore) DeleteGoogleCalendarConnection(_ context.Context, userID int64) (db.GoogleCalendarConnection, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forbidSave {
		s.t.Errorf("unexpected DeleteGoogleCalendarConnection(%d)", userID)
	}
	s.connCalls = append(s.connCalls, "delete")
	if s.deleteErr != nil {
		return db.GoogleCalendarConnection{}, false, s.deleteErr
	}
	c, ok := s.conns[userID]
	delete(s.conns, userID)
	return c, ok, nil
}

func (s *calendarStore) conn(userID int64) (db.GoogleCalendarConnection, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[userID]
	return c, ok
}

func (s *calendarStore) writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.connCalls {
		if c == "save" || c == "delete" {
			out = append(out, c)
		}
	}
	return out
}

func calendarApp(store sessionStore, googleConfigured bool) *application {
	config := appConfig{SessionSecret: calTestSecret, AppBaseURL: calTestBaseURL}
	if googleConfigured {
		config.GoogleClientID = calTestClientID
		config.GoogleClientSecret = calTestClientSc
	}
	return &application{
		store:  store,
		config: config,
		steam:  func() view.SteamService { return &fakeSteamRouteService{} },
	}
}

// fakeGoogle stands in for Google's token and revocation endpoints and points
// the package endpoint variables at itself for the test.
type fakeGoogle struct {
	mu          sync.Mutex
	tokenForms  []url.Values
	revokeForms []url.Values
	tokenStatus int
	tokenBody   map[string]any
	revokeHook  func()
	revokeCode  int
	token       *httptest.Server
	revoke      *httptest.Server
}

func idTokenWithEmail(email string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"iss": "https://accounts.google.com", "email": email, "email_verified": true})
	return header + "." + enc.EncodeToString(payload) + ".c2ln"
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	g := &fakeGoogle{tokenStatus: http.StatusOK, revokeCode: http.StatusOK}
	g.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		g.mu.Lock()
		g.tokenForms = append(g.tokenForms, r.PostForm)
		status, body := g.tokenStatus, g.tokenBody
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(body)
		} else {
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}
	}))
	g.revoke = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		g.mu.Lock()
		g.revokeForms = append(g.revokeForms, r.PostForm)
		hook, code := g.revokeHook, g.revokeCode
		g.mu.Unlock()
		if hook != nil {
			hook()
		}
		w.WriteHeader(code)
	}))
	g.tokenBody = map[string]any{
		"access_token":  "ya29.access-token",
		"refresh_token": "1//refresh-token",
		"expires_in":    3599,
		"id_token":      idTokenWithEmail("player@example.com"),
		"token_type":    "Bearer",
		"scope":         "openid https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/calendar.readonly",
	}
	oldToken, oldRevoke := googleTokenEndpoint, googleRevokeEndpoint
	googleTokenEndpoint, googleRevokeEndpoint = g.token.URL, g.revoke.URL
	t.Cleanup(func() {
		googleTokenEndpoint, googleRevokeEndpoint = oldToken, oldRevoke
		g.token.Close()
		g.revoke.Close()
	})
	return g
}

func (g *fakeGoogle) tokenCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tokenForms)
}

func (g *fakeGoogle) revokeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.revokeForms)
}

// serve sends one request through the real router with the given cookies.
func serve(app *application, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)
	return w
}

func sessionOf(token string) *http.Cookie { return profileTestCookie(calTestSecret, token) }

// startConnect runs connect for the user and returns the state and state cookie.
func startConnect(t *testing.T, app *application, token string) (string, *http.Cookie) {
	t.Helper()
	w := serve(app, http.MethodGet, component.GoogleCalendarConnectPath, sessionOf(token))
	if w.Code != http.StatusFound {
		t.Fatalf("connect status = %d, body %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == googleStateCookie {
			return loc.Query().Get("state"), c
		}
	}
	t.Fatal("connect set no state cookie")
	return "", nil
}

func callbackURL(params url.Values) string {
	return component.GoogleCalendarCallbackPath + "?" + params.Encode()
}

func assertConnectedPage(t *testing.T, body, email string) {
	t.Helper()
	if !strings.Contains(body, "<span>connected</span>") || !strings.Contains(body, email) {
		t.Errorf("profile should show connected with %q: %s", email, body)
	}
	if strings.Contains(body, "not connected") {
		t.Errorf("connected profile must not say not connected: %s", body)
	}
}

// CLM-1: connect redirects to Google with exactly the expected parameters and a
// fresh signed state in a Lax, HttpOnly, Secure cookie scoped to the callback.
func TestGoogleCalendarConnectRedirectsToGoogle(t *testing.T) {
	store := newCalendarStore(t)
	app := calendarApp(store, true)
	var states []string
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://attacker.invalid"+component.GoogleCalendarConnectPath, nil)
		req.Host = "attacker.invalid"
		req.Header.Set("X-Forwarded-Host", "attacker.invalid")
		req.AddCookie(sessionOf(userAToken))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		if w.Code != http.StatusFound && w.Code != http.StatusSeeOther {
			t.Fatalf("status = %d", w.Code)
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if loc.Scheme != "https" || loc.Host != "accounts.google.com" || loc.Path != "/o/oauth2/v2/auth" {
			t.Fatalf("redirect endpoint = %s", loc)
		}
		q := loc.Query()
		keys := make([]string, 0, len(q))
		for k, v := range q {
			if len(v) != 1 {
				t.Errorf("parameter %s repeated: %v", k, v)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if want := []string{"access_type", "client_id", "prompt", "redirect_uri", "response_type", "scope", "state"}; !reflect.DeepEqual(keys, want) {
			t.Fatalf("parameters = %v, want %v", keys, want)
		}
		if q.Get("client_id") != calTestClientID || q.Get("response_type") != "code" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
			t.Errorf("query = %v", q)
		}
		if got := q.Get("redirect_uri"); got != calTestBaseURL+component.GoogleCalendarCallbackPath {
			t.Errorf("redirect_uri = %q", got)
		}
		scopes := strings.Fields(q.Get("scope"))
		sort.Strings(scopes)
		if want := []string{"email", "https://www.googleapis.com/auth/calendar.readonly", "openid"}; !reflect.DeepEqual(scopes, want) {
			t.Errorf("scopes = %v, want %v", scopes, want)
		}
		state := q.Get("state")
		if len(state) < 32 {
			t.Fatalf("state %q is too short to be unguessable", state)
		}
		states = append(states, state)
		var cookie *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == googleStateCookie {
				cookie = c
			}
		}
		if cookie == nil {
			t.Fatal("no state cookie")
		}
		if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("state cookie flags: HttpOnly=%v Secure=%v SameSite=%v", cookie.HttpOnly, cookie.Secure, cookie.SameSite)
		}
		if cookie.Path == "" || !strings.HasPrefix(component.GoogleCalendarCallbackPath, cookie.Path) {
			t.Errorf("cookie path %q does not cover %s", cookie.Path, component.GoogleCalendarCallbackPath)
		}
		if !strings.HasPrefix(cookie.Value, state+".") || cookie.Value == state+"." {
			t.Errorf("state cookie must carry the state and its MAC: %q", cookie.Value)
		}
		if cookie.Value != state+"."+app.googleStateMAC(userAID, state) {
			t.Errorf("state cookie MAC is not bound to the session user")
		}
	}
	if states[0] == states[1] {
		t.Fatal("state must differ across connects")
	}
	if len(store.writes()) != 0 {
		t.Fatalf("connect wrote to the store: %v", store.writes())
	}
}

// CLM-2 and CLM-11: a valid callback exchanges the code, stores the
// connection for the initiating user and the profile then shows it, with the
// tokens never reaching the browser.
func TestGoogleCalendarCallbackStoresConnection(t *testing.T) {
	store := newCalendarStore(t)
	g := newFakeGoogle(t)
	g.tokenBody["access_token"] = "SENTINEL-ACCESS-7f3a"
	g.tokenBody["refresh_token"] = "SENTINEL-REFRESH-9c1b"
	app := calendarApp(store, true)

	state, stateCookie := startConnect(t, app, userAToken)
	w := serve(app, http.MethodGet, callbackURL(url.Values{"state": {state}, "code": {"auth-code-1"}, "scope": {googleCalendarScope}}), sessionOf(userAToken), stateCookie)
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/profile" {
		t.Fatalf("Location = %q, want /profile", loc)
	}
	assertNoSentinel(t, "callback", w, "SENTINEL-ACCESS-7f3a", "SENTINEL-REFRESH-9c1b")

	if g.tokenCount() != 1 {
		t.Fatalf("token endpoint calls = %d", g.tokenCount())
	}
	form := g.tokenForms[0]
	if form.Get("code") != "auth-code-1" || form.Get("client_id") != calTestClientID || form.Get("client_secret") != calTestClientSc ||
		form.Get("redirect_uri") != calTestBaseURL+component.GoogleCalendarCallbackPath || form.Get("grant_type") != "authorization_code" {
		t.Errorf("token form = %v", form)
	}
	if got := store.writes(); !reflect.DeepEqual(got, []string{"save"}) {
		t.Fatalf("store writes = %v", got)
	}
	conn, ok := store.conn(userAID)
	if !ok || conn.Email != "player@example.com" || conn.RefreshToken != "SENTINEL-REFRESH-9c1b" || conn.AccessToken != "SENTINEL-ACCESS-7f3a" {
		t.Fatalf("stored connection = %+v, %v", conn, ok)
	}
	if _, ok := store.conn(userBID); ok {
		t.Fatal("connection stored for the wrong user")
	}

	page := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
	if page.Code != http.StatusOK {
		t.Fatalf("profile status = %d", page.Code)
	}
	assertConnectedPage(t, page.Body.String(), "player@example.com")
	if strings.Contains(page.Body.String(), "Couldn't connect") {
		t.Error("successful connect must show no failure message")
	}
	assertNoSentinel(t, "profile", page, "SENTINEL-ACCESS-7f3a", "SENTINEL-REFRESH-9c1b")

	d := serve(app, http.MethodPost, component.GoogleCalendarDisconnectPath, sessionOf(userAToken))
	if d.Header().Get("Location") != "/profile" {
		t.Fatalf("disconnect Location = %q", d.Header().Get("Location"))
	}
	assertNoSentinel(t, "disconnect", d, "SENTINEL-ACCESS-7f3a", "SENTINEL-REFRESH-9c1b")
}

func assertNoSentinel(t *testing.T, what string, w *httptest.ResponseRecorder, sentinels ...string) {
	t.Helper()
	var all strings.Builder
	all.WriteString(w.Body.String())
	for k, vs := range w.Header() {
		all.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
	}
	for _, s := range sentinels {
		if strings.Contains(all.String(), s) {
			t.Errorf("%s response leaks token %q", what, s)
		}
	}
}

// CLM-3: disconnect deletes first, then revokes the deleted token, then the
// profile shows not connected.
func TestGoogleCalendarDisconnectDeletesThenRevokes(t *testing.T) {
	store := newCalendarStore(t)
	store.conns[userAID] = db.GoogleCalendarConnection{UserID: userAID, Email: "player@example.com", RefreshToken: "refresh-stored", AccessToken: "access-stored"}
	g := newFakeGoogle(t)
	stillThere := true
	g.revokeHook = func() { _, stillThere = store.conn(userAID) }
	app := calendarApp(store, true)

	w := serve(app, http.MethodPost, component.GoogleCalendarDisconnectPath, sessionOf(userAToken))
	if w.Header().Get("Location") != "/profile" {
		t.Fatalf("status %d Location %q", w.Code, w.Header().Get("Location"))
	}
	if g.revokeCount() != 1 || g.revokeForms[0].Get("token") != "refresh-stored" {
		t.Fatalf("revocation requests = %v", g.revokeForms)
	}
	if stillThere {
		t.Fatal("revocation ran before the connection was deleted")
	}
	if got := store.writes(); !reflect.DeepEqual(got, []string{"delete"}) {
		t.Fatalf("store writes = %v", got)
	}
	page := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
	if !strings.Contains(page.Body.String(), "not connected") {
		t.Fatalf("profile should show not connected: %s", page.Body.String())
	}
}

// CLM-4: a failing or unreachable revocation endpoint does not undo or block
// the delete.
func TestGoogleCalendarDisconnectIgnoresRevocationFailure(t *testing.T) {
	for _, mode := range []string{"500", "closed"} {
		t.Run(mode, func(t *testing.T) {
			store := newCalendarStore(t)
			store.conns[userAID] = db.GoogleCalendarConnection{UserID: userAID, Email: "player@example.com", RefreshToken: "refresh-stored"}
			g := newFakeGoogle(t)
			if mode == "500" {
				g.revokeCode = http.StatusInternalServerError
			} else {
				g.revoke.Close()
			}
			app := calendarApp(store, true)
			w := serve(app, http.MethodPost, component.GoogleCalendarDisconnectPath, sessionOf(userAToken))
			if w.Header().Get("Location") != "/profile" {
				t.Fatalf("status %d Location %q", w.Code, w.Header().Get("Location"))
			}
			if mode == "500" && g.revokeCount() != 1 {
				t.Fatalf("revocation requests = %d", g.revokeCount())
			}
			if _, ok := store.conn(userAID); ok {
				t.Fatal("connection still stored after disconnect")
			}
			page := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
			if !strings.Contains(page.Body.String(), "not connected") {
				t.Fatalf("profile should show not connected: %s", page.Body.String())
			}
		})
	}
}

// CLM-5: a delete store error is a 500 ErrorPage, no revocation, no change.
func TestGoogleCalendarDisconnectDeleteErrorSkipsRevocation(t *testing.T) {
	store := newCalendarStore(t)
	original := db.GoogleCalendarConnection{UserID: userAID, Email: "player@example.com", RefreshToken: "refresh-stored"}
	store.conns[userAID] = original
	store.deleteErr = errors.New("db down")
	g := newFakeGoogle(t)
	app := calendarApp(store, true)

	w := serve(app, http.MethodPost, component.GoogleCalendarDisconnectPath, sessionOf(userAToken))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<title>Gather Your Party</title>") || !strings.Contains(w.Body.String(), "unable to disconnect Google Calendar") {
		t.Errorf("body is not the ErrorPage: %s", w.Body.String())
	}
	if g.revokeCount() != 0 {
		t.Fatalf("revocation requests = %d, want 0", g.revokeCount())
	}
	if got, ok := store.conn(userAID); !ok || got != original {
		t.Fatalf("connection changed: %+v %v", got, ok)
	}
}

// CLM-6: without Google credentials a connected user still sees and can
// remove the connection; revocation needs no credentials.
func TestGoogleCalendarConnectedWithoutCredentials(t *testing.T) {
	for _, revokeFails := range []bool{false, true} {
		name := "revoke ok"
		if revokeFails {
			name = "revoke fails"
		}
		t.Run(name, func(t *testing.T) {
			store := newCalendarStore(t)
			store.conns[userAID] = db.GoogleCalendarConnection{UserID: userAID, Email: "player@example.com", RefreshToken: "refresh-stored"}
			g := newFakeGoogle(t)
			if revokeFails {
				g.revokeCode = http.StatusBadGateway
			}
			app := calendarApp(store, false)

			page := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
			if page.Code != http.StatusOK {
				t.Fatalf("status = %d", page.Code)
			}
			body := page.Body.String()
			assertConnectedPage(t, body, "player@example.com")
			if !strings.Contains(body, `action="`+component.GoogleCalendarDisconnectPath+`"`) {
				t.Errorf("missing disconnect control: %s", body)
			}
			if strings.Contains(body, "unavailable") {
				t.Errorf("connected user must not see unavailable: %s", body)
			}

			w := serve(app, http.MethodPost, component.GoogleCalendarDisconnectPath, sessionOf(userAToken))
			if w.Header().Get("Location") != "/profile" {
				t.Fatalf("status %d Location %q", w.Code, w.Header().Get("Location"))
			}
			if _, ok := store.conn(userAID); ok {
				t.Fatal("connection not deleted")
			}
			if g.revokeCount() != 1 || g.revokeForms[0].Get("token") != "refresh-stored" {
				t.Fatalf("revocation requests = %v", g.revokeForms)
			}
			after := serve(app, http.MethodGet, "/profile", sessionOf(userAToken)).Body.String()
			if !strings.Contains(after, "unavailable") || strings.Contains(after, "not connected") {
				t.Fatalf("profile should show unavailable: %s", after)
			}
		})
	}
}

// CLM-7: every callback failure lands on /profile with the fixed flag, stores
// nothing, and the page shows the couldn't-connect message beside the
// current state.
func TestGoogleCalendarCallbackFailures(t *testing.T) {
	const description = "DISTINCTIVE-error-description-42"
	cases := []struct {
		name        string
		tokenStatus int
		params      func(state string) url.Values
		noConnect   bool
	}{
		{name: "access_denied", params: func(s string) url.Values {
			return url.Values{"error": {"access_denied"}, "state": {s}, "error_description": {description}}
		}},
		{name: "server_error", params: func(s string) url.Values { return url.Values{"error": {"server_error"}, "state": {s}} }},
		{name: "missing code", params: func(s string) url.Values { return url.Values{"state": {s}} }},
		{name: "wrong state", params: func(string) url.Values {
			return url.Values{"state": {"0000000000000000000000000000000000000000000000000000000000000000"}, "code": {"valid-code"}}
		}},
		{name: "no prior connect", noConnect: true, params: func(string) url.Values {
			return url.Values{"state": {"abc"}, "code": {"valid-code"}}
		}},
		{name: "token endpoint 400", tokenStatus: http.StatusBadRequest, params: func(s string) url.Values {
			return url.Values{"state": {s}, "code": {"valid-code"}}
		}},
	}
	for _, tc := range cases {
		for _, seeded := range []bool{false, true} {
			name := tc.name + "/no connection"
			if seeded {
				name = tc.name + "/seeded"
			}
			t.Run(name, func(t *testing.T) {
				store := newCalendarStore(t)
				original := db.GoogleCalendarConnection{UserID: userAID, Email: "seeded@example.com", RefreshToken: "seed-refresh", AccessToken: "seed-access", TokenExpiry: time.Unix(1700000000, 0)}
				if seeded {
					store.conns[userAID] = original
				}
				g := newFakeGoogle(t)
				if tc.tokenStatus != 0 {
					g.tokenStatus = tc.tokenStatus
				}
				app := calendarApp(store, true)
				cookies := []*http.Cookie{sessionOf(userAToken)}
				state := ""
				if !tc.noConnect {
					var stateCookie *http.Cookie
					state, stateCookie = startConnect(t, app, userAToken)
					cookies = append(cookies, stateCookie)
				}
				store.forbidSave = true
				w := serve(app, http.MethodGet, callbackURL(tc.params(state)), cookies...)
				loc := w.Header().Get("Location")
				if loc != "/profile?calendar=error" {
					t.Fatalf("status %d Location %q", w.Code, loc)
				}
				if strings.Contains(loc, description) || strings.Contains(w.Body.String(), description) {
					t.Fatal("error_description copied into the response")
				}
				if got := store.writes(); len(got) != 0 {
					t.Fatalf("store writes = %v", got)
				}
				got, ok := store.conn(userAID)
				if seeded && (!ok || got != original) {
					t.Fatalf("seeded connection changed: %+v %v", got, ok)
				}
				if !seeded && ok {
					t.Fatalf("connection stored: %+v", got)
				}
				page := serve(app, http.MethodGet, loc, sessionOf(userAToken))
				body := page.Body.String()
				if page.Code != http.StatusOK || !strings.Contains(body, "Couldn&#39;t connect Google Calendar") && !strings.Contains(body, "Couldn't connect Google Calendar") {
					t.Fatalf("profile missing couldn't-connect message (%d): %s", page.Code, body)
				}
				if strings.Contains(body, description) {
					t.Fatal("error_description rendered")
				}
				if seeded {
					assertConnectedPage(t, body, "seeded@example.com")
				} else if !strings.Contains(body, "not connected") {
					t.Errorf("profile should show not connected: %s", body)
				}
			})
		}
	}
	t.Run("no flag no message", func(t *testing.T) {
		app := calendarApp(newCalendarStore(t), true)
		body := serve(app, http.MethodGet, "/profile", sessionOf(userAToken)).Body.String()
		if strings.Contains(body, "connect Google Calendar. Please try again") {
			t.Fatalf("profile without flag shows the failure message: %s", body)
		}
		other := serve(app, http.MethodGet, "/profile?calendar=other", sessionOf(userAToken)).Body.String()
		if strings.Contains(other, "connect Google Calendar. Please try again") {
			t.Fatal("only calendar=error sets the failure message")
		}
	})
}

// A consent that grants sign-in but not calendar read access (the user
// unticked it on Google's granular consent screen) is no connection: the
// just-issued token is revoked, nothing is stored and the profile shows the
// couldn't-connect message, never 'connected'.
func TestGoogleCalendarCallbackRejectsPartialGrant(t *testing.T) {
	for _, tc := range []struct {
		name      string
		refresh   bool
		wantToken string
	}{
		{name: "refresh token revoked", refresh: true, wantToken: "SENTINEL-REFRESH-partial"},
		{name: "access token revoked when no refresh", refresh: false, wantToken: "SENTINEL-ACCESS-partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newCalendarStore(t)
			g := newFakeGoogle(t)
			g.tokenBody["scope"] = "openid email"
			g.tokenBody["access_token"] = "SENTINEL-ACCESS-partial"
			if tc.refresh {
				g.tokenBody["refresh_token"] = "SENTINEL-REFRESH-partial"
			} else {
				delete(g.tokenBody, "refresh_token")
			}
			app := calendarApp(store, true)

			state, stateCookie := startConnect(t, app, userAToken)
			store.forbidSave = true
			// The callback query's scope is not what Google uses to report
			// grants; it must not rescue a partial token response.
			w := serve(app, http.MethodGet, callbackURL(url.Values{"state": {state}, "code": {"auth-code-partial"}, "scope": {googleCalendarScope}}), sessionOf(userAToken), stateCookie)
			if loc := w.Header().Get("Location"); loc != calendarErrorPath {
				t.Fatalf("status %d Location %q, want %q", w.Code, loc, calendarErrorPath)
			}
			assertNoSentinel(t, "callback", w, "SENTINEL-ACCESS-partial", "SENTINEL-REFRESH-partial")
			if got := store.writes(); len(got) != 0 {
				t.Fatalf("store writes = %v", got)
			}
			if conn, ok := store.conn(userAID); ok {
				t.Fatalf("connection stored: %+v", conn)
			}
			if g.revokeCount() != 1 || g.revokeForms[0].Get("token") != tc.wantToken {
				t.Fatalf("revocation requests = %v, want one for %q", g.revokeForms, tc.wantToken)
			}

			page := serve(app, http.MethodGet, calendarErrorPath, sessionOf(userAToken))
			body := page.Body.String()
			if page.Code != http.StatusOK {
				t.Fatalf("profile status = %d", page.Code)
			}
			if strings.Contains(body, "<span>connected</span>") {
				t.Fatalf("partial grant must not show connected: %s", body)
			}
			if !strings.Contains(body, "not connected") {
				t.Errorf("profile should show not connected: %s", body)
			}
			if !strings.Contains(body, "Couldn&#39;t connect Google Calendar") && !strings.Contains(body, "Couldn't connect Google Calendar") {
				t.Errorf("profile missing couldn't-connect message: %s", body)
			}
		})
	}
}

// CLM-8: without credentials the profile shows unavailable with no connect
// control, and connect never leaves for Google.
func TestGoogleCalendarUnavailableWithoutCredentials(t *testing.T) {
	for _, config := range []appConfig{
		{},
		{GoogleClientID: calTestClientID},
		{GoogleClientSecret: calTestClientSc},
	} {
		store := newCalendarStore(t)
		app := calendarApp(store, false)
		app.config.GoogleClientID, app.config.GoogleClientSecret = config.GoogleClientID, config.GoogleClientSecret
		page := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
		body := page.Body.String()
		if page.Code != http.StatusOK || !strings.Contains(body, "unavailable") {
			t.Fatalf("profile (%d) should show unavailable: %s", page.Code, body)
		}
		if strings.Contains(body, "not connected") || strings.Contains(body, component.GoogleCalendarConnectPath) || strings.Contains(body, "accounts.google.com") {
			t.Errorf("unavailable profile offers a connect: %s", body)
		}
		w := serve(app, http.MethodGet, component.GoogleCalendarConnectPath, sessionOf(userAToken))
		loc := w.Header().Get("Location")
		if strings.Contains(loc, "google.com") || loc != "/profile" {
			t.Errorf("connect without credentials redirected to %q", loc)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == googleStateCookie {
				t.Error("connect without credentials set a state cookie")
			}
		}
	}
}

// CLM-9: a state issued to user A and replayed in user B's session stores
// nothing and never reaches the token endpoint.
func TestGoogleCalendarStateBoundToUser(t *testing.T) {
	store := newCalendarStore(t)
	g := newFakeGoogle(t)
	app := calendarApp(store, true)
	state, stateCookie := startConnect(t, app, userAToken)
	store.forbidSave = true
	w := serve(app, http.MethodGet, callbackURL(url.Values{"state": {state}, "code": {"valid-code"}}), sessionOf(userBToken), stateCookie)
	if loc := w.Header().Get("Location"); loc != "/profile?calendar=error" {
		t.Fatalf("Location = %q", loc)
	}
	if g.tokenCount() != 0 {
		t.Fatalf("token endpoint called %d times", g.tokenCount())
	}
	if len(store.writes()) != 0 {
		t.Fatalf("store writes = %v", store.writes())
	}
	if _, ok := store.conn(userAID); ok {
		t.Fatal("connection stored for A")
	}
	if _, ok := store.conn(userBID); ok {
		t.Fatal("connection stored for B")
	}
}

// CLM-10: without a valid session the three routes neither go to Google nor
// touch connections or Google's endpoints.
func TestGoogleCalendarRoutesRequireSession(t *testing.T) {
	store := newCalendarStore(t)
	store.forbidSave = true
	store.conns[userAID] = db.GoogleCalendarConnection{UserID: userAID, Email: "player@example.com", RefreshToken: "refresh-stored"}
	g := newFakeGoogle(t)
	app := calendarApp(store, true)
	// A real state for A, so only the session is missing.
	state, stateCookie := startConnect(t, app, userAToken)
	forged := &http.Cookie{Name: "session", Value: userAToken + "." + strings.Repeat("00", 32)}
	for _, session := range []*http.Cookie{nil, forged} {
		for _, rt := range []struct{ method, target string }{
			{http.MethodGet, component.GoogleCalendarConnectPath},
			{http.MethodGet, callbackURL(url.Values{"state": {state}, "code": {"valid-code"}})},
			{http.MethodPost, component.GoogleCalendarDisconnectPath},
		} {
			cookies := []*http.Cookie{stateCookie}
			if session != nil {
				cookies = append(cookies, session)
			}
			w := serve(app, rt.method, rt.target, cookies...)
			if strings.Contains(w.Header().Get("Location"), "google") {
				t.Errorf("%s %s redirected to %q", rt.method, rt.target, w.Header().Get("Location"))
			}
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s status = %d, want 401", rt.method, rt.target, w.Code)
			}
		}
	}
	if len(store.writes()) != 0 {
		t.Fatalf("store writes = %v", store.writes())
	}
	if g.tokenCount() != 0 || g.revokeCount() != 0 {
		t.Fatalf("google calls: token %d revoke %d", g.tokenCount(), g.revokeCount())
	}
	if _, ok := store.conn(userAID); !ok {
		t.Fatal("connection removed")
	}
}

// CLM-11: a seeded connection's tokens never appear on the profile page while
// its email does.
func TestGoogleCalendarProfileNeverRendersTokens(t *testing.T) {
	store := newCalendarStore(t)
	store.conns[userAID] = db.GoogleCalendarConnection{UserID: userAID, Email: "player@example.com", RefreshToken: "SENTINEL-REFRESH-seed", AccessToken: "SENTINEL-ACCESS-seed"}
	newFakeGoogle(t)
	app := calendarApp(store, true)
	page := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
	if !strings.Contains(page.Body.String(), "player@example.com") {
		t.Fatalf("email missing: %s", page.Body.String())
	}
	assertNoSentinel(t, "profile", page, "SENTINEL-REFRESH-seed", "SENTINEL-ACCESS-seed")
	d := serve(app, http.MethodPost, component.GoogleCalendarDisconnectPath, sessionOf(userAToken))
	assertNoSentinel(t, "disconnect", d, "SENTINEL-REFRESH-seed", "SENTINEL-ACCESS-seed")
}

// CLM-12: a connection lookup error is a 500 ErrorPage, never a not-connected page.
func TestGoogleCalendarProfileLookupError(t *testing.T) {
	store := newCalendarStore(t)
	store.getErr = errors.New("db down")
	app := calendarApp(store, true)
	w := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "unable to load your profile") {
		t.Errorf("body is not the ErrorPage: %s", body)
	}
	if strings.Contains(body, "not connected") || strings.Contains(body, component.GoogleCalendarConnectPath) || strings.Contains(body, `id="calendars"`) {
		t.Errorf("error response renders calendar state: %s", body)
	}

	store.getErr = nil
	ok := serve(app, http.MethodGet, "/profile", sessionOf(userAToken))
	if ok.Code != http.StatusOK || !strings.Contains(ok.Body.String(), "not connected") {
		t.Fatalf("positive control: %d %s", ok.Code, ok.Body.String())
	}
}
