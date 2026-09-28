package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/db"
)

// CLM-4/5: drive the actual router and middleware; denial must not even load rows.
func TestPartiesRoutes(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		authenticated, invalidSession, missingUser bool
		resolveErr, partiesErr, invitesErr         error
		status, partiesCalls, invitesCalls         int
	}{
		{name: "authenticated", authenticated: true, status: 200, partiesCalls: 1, invitesCalls: 1},
		{name: "no cookie", status: 401},
		{name: "unknown session", authenticated: true, invalidSession: true, status: 401},
		{name: "unresolvable user", authenticated: true, missingUser: true, status: 401},
		{name: "identity error", authenticated: true, resolveErr: errors.New("private database details"), status: 500},
		{name: "parties error", authenticated: true, partiesErr: errors.New("private database details"), status: 500, partiesCalls: 1},
		{name: "invites error", authenticated: true, invitesErr: errors.New("private database details"), status: 500, partiesCalls: 1, invitesCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &leaveStore{
				found: !tc.missingUser, invalidSession: tc.invalidSession,
				resolveErr: tc.resolveErr, partiesErr: tc.partiesErr, invitesErr: tc.invitesErr,
				parties: []db.UserParty{{PartyID: testPartyID, Name: "Private current party"}},
				invites: []db.UserPendingInvite{{PartyID: "61daf402-9c06-4c3b-923f-5f7dfd4369c8", Name: "Private invitation"}},
			}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties?user_id=999&steamID=forged", "", tc.authenticated))
			body := w.Body.String()
			if w.Code != tc.status || store.partiesCalls != tc.partiesCalls || store.invitesCalls != tc.invitesCalls {
				t.Fatalf("status=%d parties=%d invites=%d body=%s", w.Code, store.partiesCalls, store.invitesCalls, body)
			}
			for _, marker := range []string{"<title>Parties</title>", "Private current party", "Private invitation", `href="/parties/` + testPartyID + `"`, `action="/parties/61daf402-9c06-4c3b-923f-5f7dfd4369c8/invites/accept"`} {
				if strings.Contains(body, marker) != (tc.status == 200) {
					t.Errorf("unexpected page visibility for %q: %s", marker, body)
				}
			}
			if strings.Contains(body, "private database details") {
				t.Fatal("database details leaked")
			}
			if tc.partiesCalls > 0 && (store.partiesUser != 42 || store.identity != testSteamID) {
				t.Fatal("parties queried for wrong identity")
			}
			if tc.invitesCalls > 0 && store.invitesUser != 42 {
				t.Fatal("invites queried for wrong identity")
			}
		})
	}
}

// CLM-1/10: a successful fake remains available in denial cases so removing the
// identity gate produces a creation, not a coincidental database failure.
func TestCreatePartyRoutes(t *testing.T) {
	for _, tc := range []struct {
		name                                           string
		authenticated, invalidSession, missingUser, hx bool
		body                                           string
		resolveErr, createErr                          error
		status, calls                                  int
	}{
		{name: "ordinary form", authenticated: true, body: "name=Friday+games&user_id=999&steamID=forged", status: 303, calls: 1},
		{name: "HTMX form", authenticated: true, hx: true, body: "name=Friday+games&user_id=999", status: 200, calls: 1},
		{name: "no cookie", body: "name=Friday+games&user_id=42&steamID=" + testSteamID, status: 401},
		{name: "unknown session", authenticated: true, invalidSession: true, body: "name=Friday+games", status: 401},
		{name: "unresolvable user", authenticated: true, missingUser: true, body: "name=Friday+games", status: 401},
		{name: "identity error", authenticated: true, resolveErr: errors.New("private database details"), status: 500},
		{name: "invalid form", authenticated: true, body: "name=%zz", status: 400},
		{name: "create error", authenticated: true, body: "name=Friday+games", createErr: errors.New("private database details"), status: 500, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &leaveStore{found: !tc.missingUser, invalidSession: tc.invalidSession, resolveErr: tc.resolveErr, createErr: tc.createErr}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			r := leaveRequest(http.MethodPost, "/parties?name=query-spoof", tc.body, tc.authenticated)
			if tc.hx {
				r.Header.Set("HX-Request", "true")
			}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, r)
			if w.Code != tc.status || store.createCalls != tc.calls || strings.Contains(w.Body.String(), "private database details") {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, store.createCalls, w.Body.String())
			}
			if tc.calls > 0 && (store.createUser != 42 || store.createName != "Friday games" || store.identity != testSteamID) {
				t.Fatalf("wrong creation arguments: user=%d name=%q identity=%q", store.createUser, store.createName, store.identity)
			}
			if tc.status == 200 && (w.Header().Get("HX-Redirect") != "/parties/"+testPartyID || w.Body.Len() != 0) {
				t.Fatal("HTMX creation must navigate to the new party")
			}
			if tc.status == 303 && w.Header().Get("Location") != "/parties/"+testPartyID {
				t.Fatal("ordinary creation must redirect to the new party")
			}
		})
	}
}
