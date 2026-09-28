//go:build integration

package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

// Observe invocation without replacing any database operation. In particular,
// an erroneous unauthorized CreateParty call must fail the test even if its
// attempted insert is subsequently rejected by a database constraint.
type partiesIntegrationStore struct {
	*db.Store
	createCalls int
}

func (s *partiesIntegrationStore) CreateParty(ctx context.Context, userID int64, name string) (string, error) {
	s.createCalls++
	return s.Store.CreateParty(ctx, userID, name)
}

// CLM-1/4/5/10 at the integration rung: real sessions, user lookup, enumeration
// and creation all run against Postgres through the production router.
func TestPartiesRoutesRealStore(t *testing.T) {
	realStore, pool, ctx := inviteIntegrationStore(t)
	store := &partiesIntegrationStore{Store: realStore}
	user, err := store.UpsertUser(ctx, testSteamID, steamapi.Player{PersonaName: "Page user"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.UpsertUser(ctx, "76561198000000799", steamapi.Player{PersonaName: "Other user"})
	if err != nil {
		t.Fatal(err)
	}
	ownParty, err := realStore.CreateParty(ctx, user, "Private current party")
	if err != nil {
		t.Fatal(err)
	}
	invitedParty, err := realStore.CreateParty(ctx, other, "Private pending invite")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SendInvite(ctx, invitedParty, other, user); err != nil {
		t.Fatal(err)
	}
	config := appConfig{SessionSecret: "parties-integration-secret"}
	cookieFor := func(token string) *http.Cookie {
		mac := hmac.New(sha256.New, []byte(config.SessionSecret))
		mac.Write([]byte(token))
		return &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
	}
	token, err := store.CreateSession(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := store.CreateSession(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	app := application{store: store, config: config}
	router := app.routes()
	request := func(method string, cookie *http.Cookie, hx bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/parties?user_id=999&steamID=forged", strings.NewReader("name=Created+through+route&user_id=999&steamID=forged")).WithContext(ctx)
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if hx {
			r.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"anonymous", nil}, {"revoked", cookieFor(revoked)}, {"unknown", cookieFor("unknown-session-token")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				w := request(method, tc.cookie, false)
				if w.Code != http.StatusUnauthorized || store.createCalls != 0 || strings.Contains(w.Body.String(), "<title>Parties</title>") || strings.Contains(w.Body.String(), "Private") {
					t.Fatalf("%s status=%d creations=%d body=%s", method, w.Code, store.createCalls, w.Body.String())
				}
			}
		})
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM parties`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("unauthorized creation changed parties: count=%d err=%v", count, err)
	}
	page := request(http.MethodGet, cookieFor(token), false)
	if page.Code != http.StatusOK {
		t.Fatalf("authenticated page status=%d body=%s", page.Code, page.Body.String())
	}
	for _, want := range []string{"<title>Parties</title>", "Private current party", "Private pending invite", `href="/parties/` + ownParty + `"`, `action="/parties/` + invitedParty + `/invites/accept"`, `action="/parties/` + invitedParty + `/invites/reject"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(page.Body.String(), `href="/parties/`+invitedParty+`"`) {
		t.Fatal("non-membership listed as current party")
	}
	for _, hx := range []bool{false, true} {
		w := request(http.MethodPost, cookieFor(token), hx)
		location := w.Header().Get("Location")
		wantStatus := http.StatusSeeOther
		if hx {
			location = w.Header().Get("HX-Redirect")
			wantStatus = http.StatusOK
		}
		if w.Code != wantStatus || !strings.HasPrefix(location, "/parties/") {
			t.Fatalf("create status=%d location=%q", w.Code, location)
		}
		partyID := strings.TrimPrefix(location, "/parties/")
		var leader, member int64
		var name string
		if err := pool.QueryRow(ctx, `SELECT p.name, p.leader_id, m.user_id FROM parties p JOIN memberships m ON m.party_id = p.id WHERE p.id = $1`, partyID).Scan(&name, &leader, &member); err != nil {
			t.Fatal(err)
		}
		if name != "Created through route" || leader != user || member != user {
			t.Fatalf("wrong creation name=%q leader=%d member=%d", name, leader, member)
		}
	}
	if store.createCalls != 2 {
		t.Fatalf("creation calls=%d want=2", store.createCalls)
	}
}
