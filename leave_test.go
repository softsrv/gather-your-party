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
)

const testPartyID = "61daf402-9c06-4c3b-923f-5f7dfd4369c7"

type leaveStore struct {
	fakeSessionStore
	identity   string
	found      bool
	resolveErr error
	leaveErr   error
	partyID    string
	actingID   int64
	leaveCalls int
}

func (s *leaveStore) ResolveSession(context.Context, string) (string, bool, error) {
	return testSteamID, true, nil
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
		`this.closest(&#39;section&#39;).remove(); return false;`,
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
