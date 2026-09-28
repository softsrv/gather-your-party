package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/db"
)

type partyDetailStore struct {
	leaveStore
	member       bool
	memberErr    error
	membersErr   error
	memberCalls  int
	membersCalls int
	checkedParty string
	checkedUser  int64
	loadedParty  string
}

func (s *partyDetailStore) IsMember(_ context.Context, party string, user int64) (bool, error) {
	s.memberCalls++
	s.checkedParty, s.checkedUser = party, user
	return s.member, s.memberErr
}

func (s *partyDetailStore) PartyMembers(_ context.Context, party string) ([]db.PartyMember, error) {
	s.membersCalls++
	s.loadedParty = party
	return []db.PartyMember{
		{UserID: 42, Name: "Private Alyx", AvatarURL: "https://example.com/private-alyx.jpg"},
		{UserID: 7, Name: "Private Gordon", AvatarURL: "https://example.com/private-gordon.jpg"},
	}, s.membersErr
}

// A populated lookup remains available even for denial cases: removing the
// guard leaks these rows and makes the response-absence assertions fail.
func TestPartyDetailRoutes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		authenticated bool
		member        bool
		missingUser   bool
		invalidParty  bool
		resolveErr    error
		memberErr     error
		membersErr    error
		want          int
		checks        int
		loads         int
	}{
		{name: "member", authenticated: true, member: true, want: 200, checks: 1, loads: 1},
		{name: "non-member", authenticated: true, want: 403, checks: 1},
		{name: "anonymous", want: 401},
		{name: "unknown user", authenticated: true, missingUser: true, want: 401},
		{name: "invalid party", authenticated: true, invalidParty: true, want: 400},
		{name: "identity error", authenticated: true, resolveErr: errors.New("private database details"), want: 500},
		{name: "membership error", authenticated: true, member: true, memberErr: errors.New("private database details"), want: 500, checks: 1},
		{name: "roster error", authenticated: true, member: true, membersErr: errors.New("private database details"), want: 500, checks: 1, loads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &partyDetailStore{
				leaveStore: leaveStore{found: !tc.missingUser, resolveErr: tc.resolveErr},
				member:     tc.member, memberErr: tc.memberErr, membersErr: tc.membersErr,
			}
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			party := testPartyID
			if tc.invalidParty {
				party = "not-a-uuid"
			}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties/"+party+"?user_id=7&steamID=forged", "", tc.authenticated))
			body := w.Body.String()
			for _, value := range []string{"Private Alyx", "Private Gordon", "https://example.com/private-alyx.jpg", "https://example.com/private-gordon.jpg"} {
				if strings.Contains(body, value) != (tc.want == http.StatusOK) {
					t.Errorf("unexpected roster visibility for %q: %s", value, body)
				}
			}
			if w.Code != tc.want || store.memberCalls != tc.checks || store.membersCalls != tc.loads || strings.Contains(body, "private database details") {
				t.Fatalf("status=%d checks=%d loads=%d body=%s", w.Code, store.memberCalls, store.membersCalls, body)
			}
			if tc.checks > 0 && (store.checkedParty != testPartyID || store.checkedUser != 42 || store.identity != testSteamID) {
				t.Fatalf("membership checked for wrong identity: %+v", store)
			}
			if tc.loads > 0 && store.loadedParty != testPartyID {
				t.Fatalf("loaded wrong party: %s", store.loadedParty)
			}
			if tc.want == http.StatusOK && !strings.Contains(body, "<title>Party details</title>") {
				t.Fatal("detail route did not render the page wrapper")
			}
		})
	}
}
