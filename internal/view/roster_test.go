package view

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"

	"github.com/softsrv/steamapi/steamapi"
)

type rosterTransport func(*http.Request) (*http.Response, error)

func (f rosterTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func mockRosterTransport(t *testing.T, fetch rosterTransport) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = rosterTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
			t.Error("missing bounded five-second deadline")
		}
		return fetch(r)
	})
	t.Cleanup(func() { http.DefaultTransport = old })
}

func rosterResponse(body string) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func rosterContext() *middleware.CustomContext {
	return &middleware.CustomContext{Context: context.WithValue(context.Background(), middleware.SteamID{}, "leader")}
}

const rawFriends = `{"friendslist":{"friends":[{"steamid":"friend-1","relationship":"friend","friend_since":1},{"steamid":"friend-2","relationship":"friend","friend_since":2}]}}`
const hydratedFriends = `{"response":{"players":[{"steamid":"friend-1","personaname":"Alyx","avatar":"https://example.com/alyx.jpg"},{"steamid":"friend-2","personaname":"Barney","avatar":"https://example.com/barney.jpg"}]}}`
const ownedGame = `{"response":{"games":[{"appid":10,"name":"Shared adventure"}]}}`

func TestGamesListIncludesAppInfoAndPlayedFreeGames(t *testing.T) {
	calls := 0
	mockRosterTransport(t, func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if !strings.Contains(r.URL.Path, "/GetOwnedGames/") || q.Get("steamid") != "leader" || q.Get("include_appinfo") != "1" || q.Get("include_played_free_games") != "1" {
			t.Errorf("unexpected games request: %s", r.URL.Path)
		}
		return rosterResponse(ownedGame)
	})
	w := httptest.NewRecorder()
	GamesList(rosterContext(), w, httptest.NewRequest(http.MethodGet, "/frag/games", nil))
	if calls != 1 || !strings.Contains(w.Body.String(), "Shared adventure") {
		t.Fatalf("calls=%d body=%s", calls, w.Body.String())
	}
}

func TestFriendsListHydratesOneBatch(t *testing.T) {
	for _, failPlayers := range []bool{false, true} {
		name := "success"
		if failPlayers {
			name = "players error"
		}
		t.Run(name, func(t *testing.T) {
			friendsCalls, playersCalls := 0, 0
			mockRosterTransport(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "/GetFriendList/"):
					friendsCalls++
					return rosterResponse(rawFriends)
				case strings.Contains(r.URL.Path, "/GetPlayerSummaries/"):
					playersCalls++
					if r.URL.Query().Get("steamids") != "friend-1,friend-2" {
						t.Error("summaries must request the entire friend roster in one batch")
					}
					if failPlayers {
						return nil, errors.New("summaries unavailable")
					}
					return rosterResponse(hydratedFriends)
				default:
					return nil, errors.New("unexpected Steam endpoint")
				}
			})
			w := httptest.NewRecorder()
			FriendsList(rosterContext(), w, httptest.NewRequest(http.MethodGet, "/frag/friends", nil))
			body := w.Body.String()
			if friendsCalls != 1 || playersCalls != 1 {
				t.Fatalf("friends=%d summaries=%d", friendsCalls, playersCalls)
			}
			if failPlayers {
				if !strings.Contains(body, "summaries unavailable") || strings.Contains(body, "friends-list-form") {
					t.Fatalf("unexpected error rendering: %s", body)
				}
				return
			}
			for _, want := range []string{"Alyx", "Barney", "https://example.com/alyx.jpg", "https://example.com/barney.jpg"} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q in %s", want, body)
				}
			}
		})
	}
}

func sharedGamesRequest(form string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/frag/shared-games", strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestSharedGamesListUnauthenticatedRendersSignin(t *testing.T) {
	h := NewSharedGamesList(newMemStore(), func() SteamService { t.Fatal("Steam used without a session"); return nil })
	w := httptest.NewRecorder()
	h.Serve(&middleware.CustomContext{Context: context.Background()}, w, sharedGamesRequest(""))
	if !strings.Contains(w.Body.String(), "Gather Your Party") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestSharedGamesListSyncsCallerAndNeverSyncedFriend(t *testing.T) {
	store := newMemStore()
	store.addUser("leader", db.GameSyncSynced)
	var mu sync.Mutex
	var gameIDs []string
	steam := steamStub{
		friends: func(context.Context, string) ([]steamapi.Friend, error) {
			return []steamapi.Friend{{SteamID: "friend-1"}, {SteamID: "friend-2"}}, nil
		},
		players: func(_ context.Context, ids []string) ([]steamapi.Player, error) {
			if !reflect.DeepEqual(ids, []string{"friend-1", "friend-2"}) {
				t.Errorf("hydration ids=%v", ids)
			}
			return []steamapi.Player{{SteamID: "friend-1", PersonaName: "Alyx"}, {SteamID: "friend-2", PersonaName: "Barney"}}, nil
		},
		games: func(_ context.Context, id string, _, _ bool) ([]steamapi.Game, error) {
			mu.Lock()
			gameIDs = append(gameIDs, id)
			mu.Unlock()
			return []steamapi.Game{{AppID: 10, Name: "Shared adventure"}}, nil
		},
		gamesDetails: allMultiplayer,
	}
	h := NewSharedGamesList(store, func() SteamService { return steam })
	w := httptest.NewRecorder()
	h.Serve(rosterContext(), w, sharedGamesRequest("friendID=friend-2&friendID=stranger"))
	slices.Sort(gameIDs)
	if !reflect.DeepEqual(gameIDs, []string{"friend-2", "leader"}) {
		t.Fatalf("library fetches=%v", gameIDs)
	}
	if _, ok := store.users["stranger"]; ok {
		t.Fatal("non-friend created")
	}
	if p := store.users["friend-2"].profile; p.PersonaName != "Barney" {
		t.Fatalf("friend profile=%+v", p)
	}
	if body := w.Body.String(); !strings.Contains(body, "Shared adventure") {
		t.Fatalf("body=%s", body)
	}
}

func TestSharedGamesListFriendsErrorRendered(t *testing.T) {
	steam := steamStub{friends: func(context.Context, string) ([]steamapi.Friend, error) {
		return nil, errors.New("friends unavailable")
	}}
	h := NewSharedGamesList(newMemStore(), func() SteamService { return steam })
	w := httptest.NewRecorder()
	h.Serve(rosterContext(), w, sharedGamesRequest(""))
	if !strings.Contains(w.Body.String(), "friends unavailable") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestSharedGamesSyncBudgetIsAboutTwentySeconds(t *testing.T) {
	if sharedGamesSyncBudget != 20*time.Second {
		t.Fatalf("budget=%s", sharedGamesSyncBudget)
	}
	if h := NewSharedGamesList(nil, nil); h.SyncBudget != sharedGamesSyncBudget {
		t.Fatalf("constructor budget=%s", h.SyncBudget)
	}
}
