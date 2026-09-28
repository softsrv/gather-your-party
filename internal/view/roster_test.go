package view

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gather-your-party/internal/middleware"
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

func TestSharedGamesListHydratesConcurrently(t *testing.T) {
	for _, scenario := range []string{"success", "no games", "players error", "games error", "both errors"} {
		t.Run(scenario, func(t *testing.T) {
			playersStarted, gamesStarted := make(chan struct{}), make(chan struct{})
			var playersOnce, gamesOnce sync.Once
			var friendsCalls, playersCalls atomic.Int32
			var gameIDs []string
			mockRosterTransport(t, func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.Contains(r.URL.Path, "/GetFriendList/"):
					friendsCalls.Add(1)
					return rosterResponse(rawFriends)
				case strings.Contains(r.URL.Path, "/GetPlayerSummaries/"):
					playersCalls.Add(1)
					if r.URL.Query().Get("steamids") != "friend-1,friend-2" {
						t.Error("hydration must include unselected friends too")
					}
					playersOnce.Do(func() { close(playersStarted) })
					// Neither fetch may complete until the other has started.
					select {
					case <-gamesStarted:
					case <-r.Context().Done():
						t.Error("games fetch did not overlap hydration")
						return nil, r.Context().Err()
					}
					if scenario == "players error" || scenario == "both errors" {
						return nil, errors.New("summaries unavailable")
					}
					return rosterResponse(hydratedFriends)
				case strings.Contains(r.URL.Path, "/GetOwnedGames/"):
					id := r.URL.Query().Get("steamid")
					gameIDs = append(gameIDs, id)
					gamesOnce.Do(func() { close(gamesStarted) })
					select {
					case <-playersStarted:
					case <-r.Context().Done():
						t.Error("hydration did not overlap games fetch")
						return nil, r.Context().Err()
					}
					if scenario == "games error" || scenario == "both errors" {
						return nil, errors.New("games unavailable")
					}
					if scenario == "no games" && id == "friend-2" {
						return rosterResponse(`{"response":{"games":[]}}`)
					}
					return rosterResponse(ownedGame)
				default:
					return nil, errors.New("unexpected Steam endpoint")
				}
			})
			r := httptest.NewRequest(http.MethodPost, "/frag/shared-games", strings.NewReader("friendID=friend-2"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			SharedGamesList(rosterContext(), w, r)
			if friendsCalls.Load() != 1 || playersCalls.Load() != 1 {
				t.Fatalf("friends=%d summaries=%d", friendsCalls.Load(), playersCalls.Load())
			}
			wantIDs := []string{"leader", "friend-2"}
			if scenario == "games error" || scenario == "both errors" {
				wantIDs = []string{"leader"}
			}
			if !reflect.DeepEqual(gameIDs, wantIDs) {
				t.Fatalf("game IDs=%v, want only submitted IDs plus leader: %v", gameIDs, wantIDs)
			}
			want := "Shared adventure"
			switch scenario {
			case "no games":
				want = "no games found for user Barney. Their list may be private"
			case "players error", "both errors":
				want = "summaries unavailable"
			case "games error":
				want = "games unavailable"
			}
			body := w.Body.String()
			if !strings.Contains(body, want) || (scenario != "success" && strings.Contains(body, "Shared adventure")) {
				t.Fatalf("want %q in body=%s", want, body)
			}
		})
	}
}
