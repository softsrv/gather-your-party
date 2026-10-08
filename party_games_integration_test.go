//go:build integration

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

// CLM-1/3/6: real session, membership query, Steam client and rendered page;
// only Steam's HTTP boundary is substituted. TestMain supplies real Postgres.
func TestPartyDetailSharedGamesRealStore(t *testing.T) {
	for _, tc := range []struct {
		name        string
		viewer      int
		private     bool
		privateName string
	}{
		{name: "ordinary member sees whole-roster intersection and live counts", viewer: 1},
		{name: "leader sees whole-roster intersection and live counts", viewer: 0},
		{name: "private library names offending member", viewer: 1, private: true, privateName: "Private Chell"},
		{name: "private library falls back to stored Steam ID", viewer: 1, private: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, pool, ctx := inviteIntegrationStore(t)
			steamIDs := []string{"76561198000000901", "76561198000000902", "76561198000000903"}
			names := []string{"Leader Alyx", "Member Gordon", tc.privateName}
			users := make([]int64, len(steamIDs))
			for i, steamID := range steamIDs {
				var err error
				users[i], err = store.UpsertUser(ctx, steamID, steamapi.Player{PersonaName: names[i]})
				if err != nil {
					t.Fatal(err)
				}
			}
			party, err := store.CreateParty(ctx, users[0], "Shared library party")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO memberships (party_id,user_id) VALUES ($1,$2),($1,$3)`, party, users[1], users[2]); err != nil {
				t.Fatal(err)
			}
			app := application{store: store, config: appConfig{SessionSecret: "shared-games-test-secret"}}
			token, err := store.CreateSession(ctx, users[tc.viewer])
			if err != nil {
				t.Fatal(err)
			}
			mac := hmac.New(sha256.New, []byte(app.config.SessionSecret))
			mac.Write([]byte(token))

			games := []steamapi.Game{{AppID: 10, Name: "Common Alpha"}, {AppID: 20, Name: "Common Beta"}, {AppID: 30, Name: "Only first two"}, {AppID: 40, Name: "Leader exclusive"}}
			libraries := map[string][]steamapi.Game{
				steamIDs[0]: games,
				steamIDs[1]: games[:3],
				steamIDs[2]: games[:2],
			}
			if tc.private {
				libraries[steamIDs[2]] = nil
			}
			counts := map[string]int{"10": 137, "20": 9042}
			var mu sync.Mutex
			ownedCalls, countCalls := map[string]int{}, map[string]int{}
			t.Setenv("STEAM_API_KEY", "party-games-test-key")
			old := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) <= 0 {
					t.Error("Steam request lacks a bounded five-second deadline")
				}
				if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
				}
				var body string
				switch {
				case r.URL.Host == "api.steampowered.com":
					if r.URL.Query().Get("key") != "party-games-test-key" {
						t.Error("missing configured API key")
					}
					switch r.URL.Path {
					case "/IPlayerService/GetOwnedGames/v0001/":
						id := r.URL.Query().Get("steamid")
						library, ok := libraries[id]
						if !ok {
							t.Errorf("library lookup for non-roster Steam ID %q", id)
						}
						if r.URL.Query().Get("include_appinfo") != "1" || r.URL.Query().Get("include_played_free_games") != "1" {
							t.Error("owned-games options missing")
						}
						mu.Lock()
						ownedCalls[id]++
						mu.Unlock()
						data, err := json.Marshal(steamapi.GamesResult{Response: steamapi.GamesList{Games: library}})
						if err != nil {
							return nil, err
						}
						body = string(data)
					case "/IPlayerService/GetNumberOfCurrentPlayers/v0001":
						id := r.URL.Query().Get("appid")
						count, ok := counts[id]
						if !ok {
							t.Errorf("count lookup for non-shared app %q", id)
						}
						mu.Lock()
						countCalls[id]++
						mu.Unlock()
						body = fmt.Sprintf(`{"response":{"player_count":%d,"result":1}}`, count)
					case "/ISteamUser/GetFriendList/v0001/":
						body = `{"friendslist":{"friends":[]}}`
					default:
						return nil, fmt.Errorf("unexpected Steam endpoint %s", r.URL.Path)
					}
				case r.URL.Host == "store.steampowered.com" && r.URL.Path == "/api/appdetails":
					body = fmt.Sprintf(`{%q:{"success":true,"data":{"categories":[{"id":1,"description":"Multi-player"}]}}}`, r.URL.Query().Get("appids"))
				default:
					return nil, fmt.Errorf("unexpected Steam URL %s", r.URL.Host+r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			// No friendID or other roster input: every identity must come from DB.
			r := httptest.NewRequest(http.MethodGet, "/parties/"+party, nil).WithContext(ctx)
			r.AddCookie(&http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))})
			w := httptest.NewRecorder()
			app.routes().ServeHTTP(w, r)
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.Contains(body, "<title>Party details</title>") {
				t.Fatalf("status=%d body=%s", w.Code, body)
			}
			for _, id := range steamIDs {
				if ownedCalls[id] != 1 {
					t.Errorf("library calls for %s=%d want=1", id, ownedCalls[id])
				}
			}
			if tc.private {
				name := tc.privateName
				if name == "" {
					name = steamIDs[2]
				}
				for _, want := range []string{"Shared games cannot be computed.", "no games found for user " + name + ". Their list may be private", `role="alert"`} {
					if !strings.Contains(body, want) {
						t.Errorf("missing %q in %s", want, body)
					}
				}
				for _, forbidden := range []string{"party-shared-games-grid", "No games in common", "no games found for user Leader Alyx", "no games found for user Member Gordon"} {
					if strings.Contains(body, forbidden) {
						t.Errorf("unexpected %q in private-library page", forbidden)
					}
				}
				if len(countCalls) != 0 {
					t.Errorf("private library triggered player counts: %v", countCalls)
				}
				return
			}
			items := regexp.MustCompile(`<li data-appid="([0-9]+)"[^>]*>(.*?)</li>`).FindAllStringSubmatch(body, -1)
			if len(items) != 2 {
				t.Fatalf("rendered games=%d want exactly 2: %s", len(items), body)
			}
			for i, item := range items {
				wantID := fmt.Sprint(games[i].AppID)
				if item[1] != wantID || !strings.Contains(item[2], games[i].Name) || !strings.Contains(item[2], fmt.Sprintf("%d players online now", counts[wantID])) {
					t.Errorf("game/count pairing incorrect: %s", item[0])
				}
				if countCalls[wantID] != 1 {
					t.Errorf("live count calls for %s=%d want=1", wantID, countCalls[wantID])
				}
			}
			for _, excluded := range []string{"Only first two", "Leader exclusive"} {
				if strings.Contains(body, excluded) {
					t.Errorf("non-shared game %q rendered", excluded)
				}
			}
		})
	}
}
