package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The Profile page and the Google Calendar routes are gone: through
// app.routes() each answers exactly as an address that was never registered,
// signed in or not, and no handler of their own runs.
func TestRemovedProfileAndCalendarRoutesAreUnregistered(t *testing.T) {
	const secret = "removed-routes-secret"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(testSessionToken))
	validCookie := &http.Cookie{Name: "session", Value: testSessionToken + "." + hex.EncodeToString(mac.Sum(nil))}

	type response struct {
		status int
		body   string
		calls  []string
	}
	serve := func(method, path string, cookie *http.Cookie) response {
		t.Helper()
		store := &fakeSessionStore{resolvedSteamID: testSteamID}
		app := application{store: store, config: appConfig{SessionSecret: secret, AppBaseURL: "https://example.com"}}
		req := httptest.NewRequest(method, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		return response{status: w.Code, body: w.Body.String(), calls: store.calls}
	}

	const controlPath = "/no-such-page"
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/profile"},
		{http.MethodGet, "/auth/google/calendar/connect"},
		{http.MethodGet, "/auth/google/calendar/callback?state=s&code=c"},
		{http.MethodPost, "/auth/google/calendar/disconnect"},
	} {
		for _, session := range []struct {
			name   string
			cookie *http.Cookie
		}{
			{"signed in", validCookie},
			{"anonymous", nil},
		} {
			t.Run(tc.method+" "+tc.path+" "+session.name, func(t *testing.T) {
				got := serve(tc.method, tc.path, session.cookie)
				control := serve(tc.method, controlPath, session.cookie)
				if tc.method == http.MethodGet && got.status != http.StatusNotFound {
					t.Errorf("status = %d, want %d", got.status, http.StatusNotFound)
				}
				if got.status != control.status || got.body != control.body {
					t.Errorf("response = %d %q, want the never-registered %s response %d %q", got.status, got.body, controlPath, control.status, control.body)
				}
				if !reflect.DeepEqual(got.calls, control.calls) {
					t.Errorf("store calls = %v, want the never-registered path's %v", got.calls, control.calls)
				}
			})
		}
	}
}
