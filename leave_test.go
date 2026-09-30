package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

const testPartyID = "61daf402-9c06-4c3b-923f-5f7dfd4369c7"

type leaveStore struct {
	fakeSessionStore
	invalidSession     bool
	createCalls        int
	createUser         int64
	createName         string
	createErr          error
	parties            []db.UserParty
	invites            []db.UserPendingInvite
	partiesCalls       int
	invitesCalls       int
	partiesUser        int64
	invitesUser        int64
	partiesErr         error
	invitesErr         error
	identity           string
	found              bool
	resolveErr         error
	leaveErr           error
	partyID            string
	actingID           int64
	leaveCalls         int
	stepDownErr        error
	stepDownCall       int
	stepDownParty      string
	stepDownActing     int64
	stepDownTarget     int64
	sendInviteCall     int
	sendInviteParty    string
	sendInviteActing   int64
	sendInviteTarget   int64
	sendInviteErr      error
	acceptInviteCall   int
	acceptInviteParty  string
	acceptInviteActing int64
	acceptInviteErr    error
	rejectInviteCall   int
	rejectInviteParty  string
	rejectInviteActing int64
	rejectInviteErr    error
	candidateCall      int
	candidateParty     string
	candidateActing    int64
	candidateFriends   []string
	candidates         []db.InviteCandidate
	candidateErr       error
}

func (s *leaveStore) ResolveSession(context.Context, string) (string, bool, error) {
	if s.invalidSession {
		return "", false, nil
	}
	return testSteamID, true, nil
}

func (s *leaveStore) CreateParty(_ context.Context, userID int64, name string) (string, error) {
	s.createCalls++
	s.createUser, s.createName = userID, name
	return testPartyID, s.createErr
}

func (s *leaveStore) UserParties(_ context.Context, userID int64) ([]db.UserParty, error) {
	s.partiesCalls++
	s.partiesUser = userID
	return s.parties, s.partiesErr
}

func (s *leaveStore) UserPendingInvites(_ context.Context, userID int64) ([]db.UserPendingInvite, error) {
	s.invitesCalls++
	s.invitesUser = userID
	return s.invites, s.invitesErr
}

func (s *leaveStore) UserProfile(_ context.Context, steamID string) (steamapi.Player, bool, error) {
	return steamapi.Player{SteamID: steamID, PersonaName: "Nav tester"}, true, nil
}

func (s *leaveStore) ResolveUserID(_ context.Context, steamID string) (int64, bool, error) {
	s.identity = steamID
	return 42, s.found, s.resolveErr
}

func (s *leaveStore) LeaveParty(_ context.Context, partyID string, actingID int64) error {
	s.leaveCalls++
	s.partyID, s.actingID = partyID, actingID
	return s.leaveErr
}

func (s *leaveStore) StepDown(_ context.Context, partyID string, actingID int64, targetID int64) error {
	s.stepDownCall++
	s.stepDownParty, s.stepDownActing, s.stepDownTarget = partyID, actingID, targetID
	return s.stepDownErr
}

func (s *leaveStore) SendInvite(_ context.Context, partyID string, actingID int64, targetID int64) error {
	s.sendInviteCall++
	s.sendInviteParty, s.sendInviteActing, s.sendInviteTarget = partyID, actingID, targetID
	return s.sendInviteErr
}

func (s *leaveStore) AcceptInvite(_ context.Context, partyID string, actingID int64) error {
	s.acceptInviteCall++
	s.acceptInviteParty, s.acceptInviteActing = partyID, actingID
	return s.acceptInviteErr
}

func (s *leaveStore) RejectInvite(_ context.Context, partyID string, actingID int64) error {
	s.rejectInviteCall++
	s.rejectInviteParty, s.rejectInviteActing = partyID, actingID
	return s.rejectInviteErr
}

func (s *leaveStore) InviteCandidates(_ context.Context, partyID string, actingID int64, friends []string) ([]db.InviteCandidate, error) {
	s.candidateCall++
	s.candidateParty, s.candidateActing = partyID, actingID
	s.candidateFriends = append([]string(nil), friends...)
	return s.candidates, s.candidateErr
}

func leaveRequest(method, path, body string, authenticated bool) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authenticated {
		mac := hmac.New(sha256.New, []byte("leave-test-secret"))
		mac.Write([]byte(testSessionToken))
		r.AddCookie(&http.Cookie{Name: "session", Value: testSessionToken + "." + hex.EncodeToString(mac.Sum(nil))})
	}
	return r
}

// Exercises the production router, authentication middleware and rendered templ
// surface. Browser interaction remains a separate SIT obligation.
func TestLeaveRoutes(t *testing.T) {
	store := &leaveStore{found: true}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	router := app.routes()
	path := "/parties/" + testPartyID + "/leave"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, leaveRequest(http.MethodGet, path, "", true))
	if w.Code != http.StatusOK || store.leaveCalls != 0 {
		t.Fatalf("GET status=%d, removals=%d", w.Code, store.leaveCalls)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Are you sure you want to leave this party?",
		`hx-post="` + path + `"`, `hx-target="#leave-confirmation"`,
		`name="confirm" value="yes"`, ">Confirm</button>", ">Cancel</a>",
		`<a href="/parties/` + testPartyID + `" class="btn btn-ghost">Cancel</a>`,
		`<script src="/static/script/htmx.min.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation missing %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"delete", "deleted", "removed permanently", "hx-trigger="} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("confirmation contains %q", forbidden)
		}
	}
	if strings.Count(body, "hx-post=") != 1 {
		t.Fatal("only the confirmation form may submit a leave request")
	}

	w = httptest.NewRecorder()
	r := leaveRequest(http.MethodPost, path, "confirm=yes&user_id=999&steamID=forged", true)
	r.Header.Set("HX-Request", "true")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || store.leaveCalls != 1 || store.partyID != testPartyID || store.actingID != 42 || store.identity != testSteamID {
		t.Fatalf("confirmed leave: status=%d, calls=%d, party=%s, actor=%d, identity=%s", w.Code, store.leaveCalls, store.partyID, store.actingID, store.identity)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, leaveRequest(http.MethodPost, path, "confirm=yes", true))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatal("non-HTMX confirmation must redirect home")
	}
}

func TestLeaveRejectsWithoutRemoval(t *testing.T) {
	for _, tc := range []struct {
		name          string
		method        string
		path          string
		body          string
		authenticated bool
		missingUser   bool
		resolveErr    error
		want          int
	}{
		{name: "anonymous GET", method: "GET", want: 401},
		{name: "anonymous POST", method: "POST", body: "confirm=yes", want: 401},
		{name: "forged identity", method: "POST", body: "confirm=yes&steamID=" + testSteamID, want: 401},
		{name: "unknown user", method: "POST", body: "confirm=yes", authenticated: true, missingUser: true, want: 401},
		{name: "lookup failure", method: "POST", body: "confirm=yes", authenticated: true, resolveErr: errors.New("database unavailable"), want: 500},
		{name: "missing confirmation", method: "POST", authenticated: true, want: 400},
		{name: "cancel value", method: "POST", body: "confirm=no", authenticated: true, want: 400},
		{name: "query cannot confirm", method: "POST", path: "/parties/" + testPartyID + "/leave?confirm=yes", authenticated: true, want: 400},
		{name: "invalid UUID", method: "POST", path: "/parties/not-a-uuid/leave", body: "confirm=yes", authenticated: true, want: 400},
		{name: "bad form", method: "POST", body: "confirm=%zz", authenticated: true, want: 400},
		{name: "wrong method", method: "PUT", body: "confirm=yes", authenticated: true, want: 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &leaveStore{found: !tc.missingUser, resolveErr: tc.resolveErr}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			path := tc.path
			if path == "" {
				path = "/parties/" + testPartyID + "/leave"
			}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest(tc.method, path, tc.body, tc.authenticated))
			if w.Code != tc.want || store.leaveCalls != 0 {
				t.Fatalf("status=%d want=%d, removals=%d", w.Code, tc.want, store.leaveCalls)
			}
		})
	}
}

func TestLeaveStoreError(t *testing.T) {
	store := &leaveStore{found: true, leaveErr: errors.New("private database details")}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, leaveRequest("POST", "/parties/"+testPartyID+"/leave", "confirm=yes", true))
	if w.Code != http.StatusInternalServerError || store.leaveCalls != 1 || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("failed leave: %d %s", w.Code, w.Body.String())
	}
}

// Exercises the production router, authentication middleware and step-down
// wiring analogous to TestLeaveRoutes.
func TestStepDownRoutes(t *testing.T) {
	store := &leaveStore{found: true}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	router := app.routes()
	path := "/parties/" + testPartyID + "/step-down"

	w := httptest.NewRecorder()
	r := leaveRequest(http.MethodPost, path, "target=7&user_id=999&steamID=forged", true)
	r.Header.Set("HX-Request", "true")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || store.stepDownCall != 1 ||
		store.stepDownParty != testPartyID || store.stepDownActing != 42 || store.stepDownTarget != 7 || store.identity != testSteamID {
		t.Fatalf("step down: status=%d, calls=%d, party=%s, actor=%d, target=%d, identity=%s",
			w.Code, store.stepDownCall, store.stepDownParty, store.stepDownActing, store.stepDownTarget, store.identity)
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, leaveRequest(http.MethodPost, path, "target=8", true))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatal("non-HTMX step down must redirect home")
	}
}

func TestStepDownRejectsWithoutHandoff(t *testing.T) {
	for _, tc := range []struct {
		name          string
		method        string
		path          string
		body          string
		authenticated bool
		missingUser   bool
		resolveErr    error
		want          int
	}{
		// Unlike /leave, step-down registers only POST (no GET confirmation
		// page), so an unauthenticated GET never reaches the handler at all
		// and the mux reports 404 before any identity check runs.
		{name: "anonymous GET", method: "GET", want: 404},
		{name: "anonymous POST", method: "POST", body: "target=7", want: 401},
		{name: "forged identity", method: "POST", body: "target=7&steamID=" + testSteamID, want: 401},
		{name: "unknown user", method: "POST", body: "target=7", authenticated: true, missingUser: true, want: 401},
		{name: "lookup failure", method: "POST", body: "target=7", authenticated: true, resolveErr: errors.New("database unavailable"), want: 500},
		{name: "missing target", method: "POST", authenticated: true, want: 400},
		{name: "non-numeric target", method: "POST", body: "target=not-a-number", authenticated: true, want: 400},
		{name: "invalid UUID", method: "POST", path: "/parties/not-a-uuid/step-down", body: "target=7", authenticated: true, want: 400},
		{name: "bad form", method: "POST", body: "target=%zz", authenticated: true, want: 400},
		{name: "wrong method", method: "PUT", body: "target=7", authenticated: true, want: 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &leaveStore{found: !tc.missingUser, resolveErr: tc.resolveErr}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			path := tc.path
			if path == "" {
				path = "/parties/" + testPartyID + "/step-down"
			}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest(tc.method, path, tc.body, tc.authenticated))
			if w.Code != tc.want || store.stepDownCall != 0 {
				t.Fatalf("status=%d want=%d, stepDownCalls=%d", w.Code, tc.want, store.stepDownCall)
			}
		})
	}
}

// CLM-1/14: the real router and middleware supply the actor; form identity is ignored.
func TestSendInviteRoutes(t *testing.T)   { testInviteRoute(t, "send") }
func TestAcceptInviteRoutes(t *testing.T) { testInviteRoute(t, "accept") }
func TestRejectInviteRoutes(t *testing.T) { testInviteRoute(t, "reject") }

func testInviteRoute(t *testing.T, action string) {
	t.Helper()
	store := &leaveStore{found: true}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	path := "/parties/" + testPartyID + "/invites/" + action
	for _, hx := range []bool{true, false} {
		w := httptest.NewRecorder()
		r := leaveRequest("POST", path, "target=7&user_id=999&steamID=forged", true)
		if hx {
			r.Header.Set("HX-Request", "true")
		}
		app.routes().ServeHTTP(w, r)
		if hx {
			if w.Code != http.StatusOK || w.Body.Len() != 0 {
				t.Fatalf("HTMX status=%d body=%s", w.Code, w.Body.String())
			}
		} else if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Fatalf("non-HTMX status=%d location=%s", w.Code, w.Header().Get("Location"))
		}
	}
	if store.identity != testSteamID {
		t.Fatalf("resolved identity=%s", store.identity)
	}
	switch action {
	case "send":
		if store.sendInviteCall != 2 || store.sendInviteParty != testPartyID || store.sendInviteActing != 42 || store.sendInviteTarget != 7 || store.acceptInviteCall != 0 || store.rejectInviteCall != 0 {
			t.Fatalf("unexpected send calls: %+v", store)
		}
	case "accept":
		if store.acceptInviteCall != 2 || store.acceptInviteParty != testPartyID || store.acceptInviteActing != 42 || store.sendInviteCall != 0 || store.rejectInviteCall != 0 {
			t.Fatalf("unexpected accept calls: %+v", store)
		}
	case "reject":
		if store.rejectInviteCall != 2 || store.rejectInviteParty != testPartyID || store.rejectInviteActing != 42 || store.sendInviteCall != 0 || store.acceptInviteCall != 0 {
			t.Fatalf("unexpected reject calls: %+v", store)
		}
	}
}

func TestInvitesRejectWithoutAction(t *testing.T) {
	for _, action := range []string{"send", "accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				method        string
				body          string
				authenticated bool
				missingUser   bool
				resolveErr    error
				invalidParty  bool
				want          int
			}{
				{name: "anonymous GET", method: "GET", want: 404},
				{name: "anonymous POST", method: "POST", body: "target=7", want: 401},
				{name: "forged identity", method: "POST", body: "target=7&steamID=" + testSteamID + "&user_id=42", want: 401},
				{name: "unknown user", method: "POST", body: "target=7", authenticated: true, missingUser: true, want: 401},
				{name: "lookup failure", method: "POST", body: "target=7", authenticated: true, resolveErr: errors.New("private database details"), want: 500},
				{name: "invalid UUID", method: "POST", body: "target=7", authenticated: true, invalidParty: true, want: 400},
				{name: "bad form", method: "POST", body: "target=%zz", authenticated: true, want: 400},
				{name: "wrong method", method: "PUT", body: "target=7", authenticated: true, want: 405},
			} {
				t.Run(tc.name, func(t *testing.T) {
					store := &leaveStore{found: !tc.missingUser, resolveErr: tc.resolveErr}
					app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
					party := testPartyID
					if tc.invalidParty {
						party = "not-a-uuid"
					}
					w := httptest.NewRecorder()
					app.routes().ServeHTTP(w, leaveRequest(tc.method, "/parties/"+party+"/invites/"+action, tc.body, tc.authenticated))
					if w.Code != tc.want || store.sendInviteCall+store.acceptInviteCall+store.rejectInviteCall != 0 || strings.Contains(w.Body.String(), "private") {
						t.Fatalf("status=%d want=%d, store=%+v body=%s", w.Code, tc.want, store, w.Body.String())
					}
				})
			}
		})
	}
}

func TestSendInviteRejectsInvalidTarget(t *testing.T) {
	for _, tc := range []struct{ body, query string }{
		{"", ""}, {"target=not-a-number", ""}, {"target=9223372036854775808", ""}, {"", "?target=7"},
	} {
		t.Run(tc.body+tc.query, func(t *testing.T) {
			store := &leaveStore{found: true}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest("POST", "/parties/"+testPartyID+"/invites/send"+tc.query, tc.body, true))
			if w.Code != http.StatusBadRequest || store.sendInviteCall != 0 {
				t.Fatalf("status=%d calls=%d", w.Code, store.sendInviteCall)
			}
		})
	}
}

func TestInviteStoreErrors(t *testing.T) {
	for _, action := range []string{"send", "accept", "reject"} {
		t.Run(action, func(t *testing.T) {
			private := errors.New("private database details")
			store := &leaveStore{found: true, sendInviteErr: private, acceptInviteErr: private, rejectInviteErr: private}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest("POST", "/parties/"+testPartyID+"/invites/"+action, "target=7", true))
			if w.Code != http.StatusInternalServerError || store.sendInviteCall+store.acceptInviteCall+store.rejectInviteCall != 1 || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("failed invite: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// The dashboard greets the user from the profile stored at sign-in; loading
// the page must not call Steam.
func TestSignedInHomeUsesStoredProfile(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected Steam request: %s", r.URL.Path)
		return nil, errors.New("no network in tests")
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	app := application{store: &leaveStore{found: true}, config: appConfig{SessionSecret: "leave-test-secret"}}
	for _, path := range []string{"/", "/parties"} {
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, path, "", true))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Nav tester") {
			t.Fatalf("%s: status=%d, stored profile missing from page: %s", path, w.Code, w.Body.String())
		}
	}
}
