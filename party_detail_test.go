package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gather-your-party/internal/db"
)

type partyDetailStore struct {
	leaveStore
	leader       bool
	solo         bool
	member       bool
	memberErr    error
	membersErr   error
	memberCalls  int
	membersCalls int
	checkedParty string
	checkedUser  int64
	loadedParty  string
	// sharedErr, when set, makes the shared-games sync and library reads fail.
	sharedErr   error
	sharedCalls int
}

func (s *partyDetailStore) GameSyncStates(ctx context.Context, ids []string) (map[string]db.GameSyncState, error) {
	s.sharedCalls++
	if s.sharedErr != nil {
		return nil, s.sharedErr
	}
	return s.leaveStore.GameSyncStates(ctx, ids)
}

func (s *partyDetailStore) SharedStoredGames(ctx context.Context, first string, rest []string) ([]db.StoredGame, error) {
	s.sharedCalls++
	if s.sharedErr != nil {
		return nil, s.sharedErr
	}
	return s.leaveStore.SharedStoredGames(ctx, first, rest)
}

func (s *partyDetailStore) IsMember(_ context.Context, party string, user int64) (bool, error) {
	s.memberCalls++
	s.checkedParty, s.checkedUser = party, user
	return s.member, s.memberErr
}

func (s *partyDetailStore) PartyMembers(_ context.Context, party string) ([]db.PartyMember, error) {
	s.membersCalls++
	s.loadedParty = party
	members := []db.PartyMember{
		{UserID: 42, SteamID64: testSteamID, Name: "Private Alyx", AvatarURL: "https://example.com/private-alyx.jpg", IsLeader: s.leader},
	}
	if !s.solo {
		members = append(members, db.PartyMember{UserID: 7, SteamID64: "76561198000000007", Name: "Private Gordon", AvatarURL: "https://example.com/private-gordon.jpg", IsLeader: !s.leader})
	}
	return members, s.membersErr
}

func TestPartyDetailLeaderControls(t *testing.T) {
	const friendID = "76561198000000007"
	for _, tc := range []struct {
		name           string
		leader         bool
		solo           bool
		empty          bool
		friendsError   bool
		candidateError bool
	}{
		{name: "leader", leader: true},
		{name: "non-leader"},
		{name: "solo leader without invite candidates", leader: true, solo: true, empty: true},
		{name: "friends failure", leader: true, friendsError: true},
		{name: "candidate failure", leader: true, candidateError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &partyDetailStore{
				leaveStore: leaveStore{found: true, candidates: []db.InviteCandidate{{UserID: 89, Name: "Eligible friend"}}},
				member:     true, leader: tc.leader, solo: tc.solo,
			}
			if tc.empty {
				store.candidates = nil
			}
			if tc.candidateError {
				store.candidateErr = errors.New("private database details")
			}
			t.Setenv("STEAM_API_KEY", "detail-test-key")
			friendCalls, playerCalls := 0, 0
			oldTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
					t.Error("missing bounded five-second deadline")
				}
				if r.Method != http.MethodGet || r.URL.Host != "api.steampowered.com" || r.URL.Query().Get("key") != "detail-test-key" {
					t.Error("unexpected Steam request or API key")
				}
				var body string
				switch {
				case strings.Contains(r.URL.Path, "/GetOwnedGames/"):
					body = `{"response":{"games":[]}}`
				case strings.Contains(r.URL.Path, "/GetFriendList/"):
					friendCalls++
					if r.URL.Query().Get("steamid") != testSteamID {
						t.Error("friends request did not use the verified session identity")
					}
					if tc.friendsError {
						return nil, errors.New("private Steam details")
					}
					body = `{"friendslist":{"friends":[{"steamid":"` + friendID + `","relationship":"friend","friend_since":1}]}}`
				case strings.Contains(r.URL.Path, "/GetPlayerSummaries/"):
					playerCalls++
					if r.URL.Query().Get("steamids") != friendID {
						t.Error("player lookup did not use the live friend list")
					}
					body = `{"response":{"players":[{"steamid":"` + friendID + `","personaname":"Live friend"}]}}`
				default:
					t.Fatalf("unexpected Steam endpoint: %s", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties/"+testPartyID+"?user_id=7&steamID=forged&isLeader=true", "", true))
			body := w.Body.String()
			wantStatus := http.StatusOK
			if tc.friendsError || tc.candidateError {
				wantStatus = http.StatusInternalServerError
			}
			if w.Code != wantStatus || strings.Contains(body, "private database details") || strings.Contains(body, "private Steam details") {
				t.Fatalf("status=%d body=%s", w.Code, body)
			}
			if tc.leader {
				if friendCalls != 1 {
					t.Fatalf("friends requests=%d", friendCalls)
				}
				if !tc.friendsError && (playerCalls != 0 || store.candidateCall != 1 || store.candidateParty != testPartyID || store.candidateActing != 42 || len(store.candidateFriends) != 1 || store.candidateFriends[0] != friendID) {
					t.Fatalf("incorrect candidate lookup: players=%d store=%+v", playerCalls, store)
				}
			} else if friendCalls != 0 || playerCalls != 0 || store.candidateCall != 0 {
				t.Fatal("non-leader triggered a leader-only lookup")
			}
			if tc.friendsError && store.candidateCall != 0 {
				t.Fatal("failed friends lookup reached candidate store")
			}
			if wantStatus != http.StatusOK {
				if strings.Contains(body, `name="target"`) {
					t.Fatal("lookup failure rendered submitting controls")
				}
				return
			}
			if !strings.Contains(body, `aria-label="Party members"`) || !strings.Contains(body, "Private Alyx") {
				t.Fatalf("missing roster: %s", body)
			}
			for _, section := range []string{`aria-label="Invite friends"`, `id="step-down-control"`} {
				if strings.Contains(body, section) != tc.leader {
					t.Errorf("incorrect visibility for %s: %s", section, body)
				}
			}
			if !tc.leader || tc.solo {
				for _, forbidden := range []string{`name="target"`, "/invites/send", "/step-down"} {
					if strings.Contains(body, forbidden) {
						t.Errorf("unexpected action %q: %s", forbidden, body)
					}
				}
				return
			}
			for _, want := range []string{
				`action="/parties/` + testPartyID + `/invites/send"`,
				`action="/parties/` + testPartyID + `/step-down"`,
				`type="submit" name="target" value="89"`,
				`type="submit" name="target" value="7"`,
				"Eligible friend", "Private Gordon",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q: %s", want, body)
				}
			}
			if strings.Count(body, `name="target"`) != 2 || strings.Contains(body, `name="target" value="42"`) {
				t.Fatalf("controls must offer only the eligible friend and the other member: %s", body)
			}
		})
	}
}

const sharedGamesUnavailableMessage = "We could not load shared games right now. Please try again in a moment."

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written, so a server-side diagnostic can be asserted without leaking
// into the HTTP response body.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %s", err)
	}
	os.Stdout = w
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %s", err)
	}
	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %s", err)
	}
	return string(captured)
}

// TestPartyDetailSharedGamesFailure proves a hard BuildSharedGames error
// degrades the page instead of 500ing: the roster still renders, a generic
// message replaces the games section, the underlying error is diagnosable
// server-side via stdout, and the raw error never reaches the response body.
func TestPartyDetailSharedGamesFailure(t *testing.T) {
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private Steam failure details")
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	store := &partyDetailStore{leaveStore: leaveStore{found: true}, member: true}
	store.syncedGames = []db.StoredGame{{AppID: 10, Name: "Stored", DataFetched: true, HasData: true, Multiplayer: true}}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	w := httptest.NewRecorder()
	stdout := captureStdout(t, func() {
		app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties/"+testPartyID, "", true))
	})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
	if !strings.Contains(body, `aria-label="Party members"`) || !strings.Contains(body, "Private Alyx") {
		t.Fatalf("missing roster on degraded page: %s", body)
	}
	if !strings.Contains(body, sharedGamesUnavailableMessage) {
		t.Fatalf("missing generic shared-games-unavailable message: %s", body)
	}
	if strings.Contains(body, "private Steam failure details") {
		t.Fatalf("response body leaked the underlying Steam error: %s", body)
	}
	if !strings.Contains(stdout, "private Steam failure details") {
		t.Fatalf("underlying error was not recorded server-side: stdout=%s", stdout)
	}
}

// TestPartyDetailSharedGamesFailureLeaderControls proves the leader's
// invite/step-down controls also render when only the shared-games
// computation hard-fails; the GetFriendList/GetPlayerSummaries calls that
// BuildInviteCandidates depends on still succeed.
func TestPartyDetailSharedGamesFailureLeaderControls(t *testing.T) {
	const friendID = "76561198000000007"
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/GetNumberOfCurrentPlayers/v0001"):
			return nil, errors.New("private Steam failure details")
		case strings.Contains(r.URL.Path, "/GetFriendList/"):
			body := `{"friendslist":{"friends":[{"steamid":"` + friendID + `","relationship":"friend","friend_since":1}]}}`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		case strings.Contains(r.URL.Path, "/GetPlayerSummaries/"):
			body := `{"response":{"players":[{"steamid":"` + friendID + `","personaname":"Live friend"}]}}`
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		default:
			t.Fatalf("unexpected Steam endpoint: %s", r.URL.Path)
			return nil, errors.New("unexpected Steam endpoint")
		}
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	store := &partyDetailStore{
		leaveStore: leaveStore{found: true, candidates: []db.InviteCandidate{{UserID: 89, Name: "Eligible friend"}}},
		member:     true, leader: true,
	}
	store.syncedGames = []db.StoredGame{{AppID: 10, Name: "Stored", DataFetched: true, HasData: true, Multiplayer: true}}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	w := httptest.NewRecorder()
	stdout := captureStdout(t, func() {
		app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties/"+testPartyID+"?user_id=7&steamID=forged&isLeader=true", "", true))
	})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
	if !strings.Contains(body, `aria-label="Party members"`) || !strings.Contains(body, "Private Alyx") {
		t.Fatalf("missing roster on degraded page: %s", body)
	}
	if !strings.Contains(body, `aria-label="Invite friends"`) || !strings.Contains(body, `id="step-down-control"`) {
		t.Fatalf("missing leader controls on degraded page: %s", body)
	}
	if !strings.Contains(body, sharedGamesUnavailableMessage) {
		t.Fatalf("missing generic shared-games-unavailable message: %s", body)
	}
	if strings.Contains(body, "private Steam failure details") {
		t.Fatalf("response body leaked the underlying Steam error: %s", body)
	}
	if !strings.Contains(stdout, "private Steam failure details") {
		t.Fatalf("underlying error was not recorded server-side: stdout=%s", stdout)
	}
}

// mockEmptyPartyLibraries keeps roster/authorization tests independent of Steam.
func mockEmptyPartyLibraries(t *testing.T) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/GetOwnedGames/") {
			t.Errorf("unexpected Steam endpoint: %s", r.URL.Path)
			return nil, errors.New("unexpected Steam endpoint")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"response":{"games":[]}}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
}

// A populated lookup remains available even for denial cases: removing the
// guard leaks these rows and makes the response-absence assertions fail.
func TestPartyDetailRoutes(t *testing.T) {
	mockEmptyPartyLibraries(t)
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

func TestPartyDetailTwoMemberPartyOmitsInviteNote(t *testing.T) {
	mockEmptyPartyLibraries(t)
	store := &partyDetailStore{
		leaveStore: leaveStore{found: true},
		member:     true,
		solo:       false,
	}
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties/"+testPartyID, "", true))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "Invite someone to your party") {
		t.Fatalf("two-member party unexpectedly showed solo invite note: %s", body)
	}
}

const soloInviteNote = "Invite someone to your party to see the games you have in common."

// serveSoloLeaderDetail renders the party page for a solo leader against a
// Steam fake whose friends lookup succeeds and whose player-count call fails.
// It returns the response and the number of friends requests made.
func serveSoloLeaderDetail(t *testing.T, store *partyDetailStore) (*httptest.ResponseRecorder, int) {
	t.Helper()
	const friendID = "76561198000000007"
	t.Setenv("STEAM_API_KEY", "detail-test-key")
	friendCalls := 0
	old := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.Contains(r.URL.Path, "/GetNumberOfCurrentPlayers/"):
			return nil, errors.New("private Steam failure details")
		case strings.Contains(r.URL.Path, "/GetFriendList/"):
			friendCalls++
			body = `{"friendslist":{"friends":[{"steamid":"` + friendID + `","relationship":"friend","friend_since":1}]}}`
		case strings.Contains(r.URL.Path, "/GetPlayerSummaries/"):
			body = `{"response":{"players":[{"steamid":"` + friendID + `","personaname":"Live friend"}]}}`
		default:
			t.Errorf("unexpected Steam endpoint: %s", r.URL.Path)
			return nil, errors.New("unexpected Steam endpoint")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	app := application{store: store, config: appConfig{SessionSecret: "leave-test-secret"}}
	w := httptest.NewRecorder()
	stdout := captureStdout(t, func() {
		app.routes().ServeHTTP(w, leaveRequest(http.MethodGet, "/parties/"+testPartyID, "", true))
	})
	if strings.Contains(stdout, "shared games error") {
		t.Errorf("solo party computed and logged a shared-games error: %s", stdout)
	}
	if store.sharedCalls != 0 {
		t.Errorf("solo party read shared-games store data %d times, want 0", store.sharedCalls)
	}
	return w, friendCalls
}

func assertSoloInviteNote(t *testing.T, w *httptest.ResponseRecorder, friendCalls int) {
	t.Helper()
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
	if !strings.Contains(body, `aria-label="Shared games"`) || !strings.Contains(body, soloInviteNote) {
		t.Fatalf("missing shared games invite note: %s", body)
	}
	for _, forbidden := range []string{sharedGamesUnavailableMessage, "Shared games cannot be computed."} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("solo party showed %q: %s", forbidden, body)
		}
	}
	if friendCalls != 1 {
		t.Fatalf("invite picker friends lookup ran %d times, want 1", friendCalls)
	}
}

// A solo leader sees the invite note even when every shared-games dependency
// would fail: the sync and library reads, and Steam's player counts.
func TestPartyDetailSoloLeaderFailingSharedGamesShowsInviteNote(t *testing.T) {
	store := &partyDetailStore{
		leaveStore: leaveStore{found: true},
		member:     true, leader: true, solo: true,
		sharedErr: errors.New("private database details"),
	}
	store.syncedGames = []db.StoredGame{{AppID: 10, Name: "Stored", DataFetched: true, HasData: true, Multiplayer: true}}
	w, friendCalls := serveSoloLeaderDetail(t, store)
	assertSoloInviteNote(t, w, friendCalls)
}

// A solo leader who never synced sees the invite note, not a sync message.
func TestPartyDetailSoloLeaderNeverSyncedShowsInviteNote(t *testing.T) {
	store := &partyDetailStore{
		leaveStore: leaveStore{found: true},
		member:     true, leader: true, solo: true,
	}
	w, friendCalls := serveSoloLeaderDetail(t, store)
	assertSoloInviteNote(t, w, friendCalls)
}
