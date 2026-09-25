package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"gather-your-party/internal/template"

	"github.com/softsrv/steamapi/steamapi"
)

const testSteamID = "76561197960287930"
const testSessionToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHandleSteamLoginUsesConfiguredOrigin(t *testing.T) {
	app := application{config: appConfig{AppBaseURL: "https://example.com"}}
	r := httptest.NewRequest(http.MethodGet, "http://untrusted.invalid/auth/steam", nil)
	r.Header.Set("X-Forwarded-Host", "attacker.invalid")
	r.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	app.handleSteamLogin(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d", w.Code)
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Scheme != "https" || location.Host != "steamcommunity.com" || location.Path != "/openid/login" {
		t.Fatalf("unexpected endpoint: %s", location)
	}
	want := url.Values{
		"openid.ns":         {"http://specs.openid.net/auth/2.0"},
		"openid.mode":       {"checkid_setup"},
		"openid.identity":   {"http://specs.openid.net/auth/2.0/identifier_select"},
		"openid.claimed_id": {"http://specs.openid.net/auth/2.0/identifier_select"},
		"openid.return_to":  {"https://example.com/auth/steam/callback"},
		"openid.realm":      {"https://example.com"},
	}
	if !reflect.DeepEqual(location.Query(), want) {
		t.Fatalf("query = %v, want %v", location.Query(), want)
	}
}

func TestSteamID64FromClaimedID(t *testing.T) {
	valid := "https://steamcommunity.com/openid/id/" + testSteamID
	got, err := steamID64FromClaimedID(valid)
	if err != nil || got != testSteamID {
		t.Fatalf("identity = %q, err = %v", got, err)
	}
	for _, input := range []string{
		"", testSteamID, "http://steamcommunity.com/openid/id/" + testSteamID,
		"https://attacker.invalid/openid/id/" + testSteamID,
		"https://steamcommunity.com.attacker.invalid/openid/id/" + testSteamID,
		valid + "/", valid + "?steamID=other", valid + "#fragment",
		"https://steamcommunity.com/openid/id/7656119796028793x",
	} {
		if _, err := steamID64FromClaimedID(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestAssertionIsValid(t *testing.T) {
	for _, body := range []string{"is_valid:true\n", "ns:http://specs.openid.net/auth/2.0\r\nis_valid:true\r\n"} {
		if !assertionIsValid(body) {
			t.Errorf("rejected %q", body)
		}
	}
	for _, body := range []string{"", "is_valid:false\n", "error:is_valid:true\n", "is_valid:trueish\n", " is_valid:true\n"} {
		if assertionIsValid(body) {
			t.Errorf("accepted %q", body)
		}
	}
}

// Fields follow OpenID 2.0 section 10.1; only the external signature verifier
// is substituted. The Steam Web API response below follows steamapi.Player's JSON tags.
func callbackParams() url.Values {
	return url.Values{
		"openid.ns":             {"http://specs.openid.net/auth/2.0"},
		"openid.mode":           {"id_res"},
		"openid.op_endpoint":    {steamOpenIDEndpoint},
		"openid.claimed_id":     {"https://steamcommunity.com/openid/id/" + testSteamID},
		"openid.identity":       {"https://steamcommunity.com/openid/id/" + testSteamID},
		"openid.return_to":      {"https://example.com/auth/steam/callback"},
		"openid.response_nonce": {"2026-09-25T06:00:00Zunique"},
		"openid.assoc_handle":   {"test-association"},
		"openid.signed":         {"op_endpoint,claimed_id,identity,return_to,response_nonce,assoc_handle"},
		"openid.sig":            {"dGVzdC1zaWduYXR1cmU="},
		"steamID":               {"client-controlled-id"},
	}
}

type fakeSessionStore struct {
	calls      []string
	steamID    string
	player     steamapi.Player
	userID     int64
	upsertErr  error
	sessionErr error
}

func (s *fakeSessionStore) UpsertUser(ctx context.Context, id string, player steamapi.Player) (int64, error) {
	s.calls = append(s.calls, "upsert")
	s.steamID, s.player = id, player
	return 42, s.upsertErr
}

func (s *fakeSessionStore) CreateSession(ctx context.Context, userID int64) (string, error) {
	s.calls = append(s.calls, "session")
	s.userID = userID
	return testSessionToken, s.sessionErr
}

func TestCallbackRejectsInvalidAssertion(t *testing.T) {
	received := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Error("verification must be a form POST")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		received <- r.PostForm
		io.WriteString(w, "ns:http://specs.openid.net/auth/2.0\nis_valid:false\n")
	}))
	defer server.Close()
	oldEndpoint := steamOpenIDEndpoint
	steamOpenIDEndpoint = server.URL
	t.Cleanup(func() { steamOpenIDEndpoint = oldEndpoint })
	store := &fakeSessionStore{}
	app := application{store: store, config: appConfig{SessionSecret: "test-secret", AppBaseURL: "https://example.com"}}
	params := callbackParams()
	w := httptest.NewRecorder()
	app.handleSteamCallback(w, httptest.NewRequest(http.MethodGet, "/auth/steam/callback?"+params.Encode(), nil))
	want := callbackParams()
	want.Del("steamID")
	want.Set("openid.mode", "check_authentication")
	select {
	case got := <-received:
		if !reflect.DeepEqual(got, want) {
			t.Errorf("reposted fields = %v, want %v", got, want)
		}
	default:
		t.Fatal("callback did not verify the assertion with Steam")
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" || len(w.Result().Cookies()) != 0 || len(store.calls) != 0 {
		t.Fatalf("invalid assertion was not rejected: status=%d cookies=%v calls=%v", w.Code, w.Result().Cookies(), store.calls)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCallbackPersistsVerifiedIdentityAndSignsSession(t *testing.T) {
	for _, scenario := range []string{"success", "upsert error", "session error", "player error", "invalid assertion", "wrong origin", "unsigned identity", "duplicate identity", "empty secret", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			store := &fakeSessionStore{}
			app := application{store: store, config: appConfig{SessionSecret: "test-secret", AppBaseURL: "https://example.com"}}
			params := callbackParams()
			switch scenario {
			case "upsert error":
				store.upsertErr = errors.New("upsert failed")
			case "session error":
				store.sessionErr = errors.New("session failed")
			case "wrong origin":
				params.Set("openid.return_to", "https://attacker.invalid/auth/steam/callback")
			case "unsigned identity":
				params.Set("openid.signed", "op_endpoint,identity,return_to,response_nonce,assoc_handle")
			case "duplicate identity":
				params.Add("openid.claimed_id", "https://steamcommunity.com/openid/id/76561197960287931")
			case "empty secret":
				app.config.SessionSecret = ""
			case "cancelled":
				params.Set("openid.mode", "cancel")
			}
			playerCalls := 0
			oldTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Error("missing five-second deadline")
				}
				body := "ns:http://specs.openid.net/auth/2.0\nis_valid:true\n"
				if scenario == "invalid assertion" {
					body = "ns:http://specs.openid.net/auth/2.0\nis_valid:false\n"
				}
				if r.URL.String() != steamOpenIDEndpoint {
					playerCalls++
					if len(store.calls) != 0 || r.URL.Host != "api.steampowered.com" || r.URL.Query().Get("steamids") != testSteamID {
						t.Error("Player must fetch the verified identity before persistence")
					}
					if scenario == "player error" {
						return nil, errors.New("profile unavailable")
					}
					body = `{"response":{"players":[{"steamid":"ignored-profile-id","personaname":"Alyx","avatar":"small","avatarmedium":"medium","avatarfull":"full"}]}}`
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/auth/steam/callback?"+params.Encode(), nil)
			r.AddCookie(&http.Cookie{Name: "steam_id", Value: "client-cookie-id"})
			app.handleSteamCallback(w, r)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
				t.Fatalf("unexpected redirect: %d %s", w.Code, w.Header().Get("Location"))
			}
			var wantCalls []string
			switch scenario {
			case "success", "session error":
				wantCalls = []string{"upsert", "session"}
			case "upsert error":
				wantCalls = []string{"upsert"}
			}
			if !reflect.DeepEqual(store.calls, wantCalls) {
				t.Fatalf("store calls = %v, want %v", store.calls, wantCalls)
			}
			cookies := w.Result().Cookies()
			if scenario != "success" {
				if len(cookies) != 0 {
					t.Fatal("failed callback set a cookie")
				}
				return
			}
			wantPlayer := steamapi.Player{SteamID: "ignored-profile-id", PersonaName: "Alyx", AvatarSmall: "small", AvatarMedium: "medium", AvatarFull: "full"}
			if playerCalls != 1 || store.steamID != testSteamID || store.player != wantPlayer || store.userID != 42 {
				t.Fatalf("unexpected persistence: %+v, player calls=%d", store, playerCalls)
			}
			if len(cookies) != 1 {
				t.Fatalf("cookies = %v", cookies)
			}
			cookie := cookies[0]
			// Independent HMAC-SHA256 vector for test-secret and testSessionToken.
			wantValue := testSessionToken + ".bb8b1ab13589803b5109e1ff07b6955253c9b0d13e89e2b3de4d88879ba517bc"
			if cookie.Name != "session" || cookie.Value != wantValue || cookie.Path != "/" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("unexpected session cookie: %+v", cookie)
			}
		})
	}
}

func TestSignedOutHomeOffersSteamSignIn(t *testing.T) {
	var html strings.Builder
	if err := template.Home(steamapi.Player{}, "Gather Your Party", template.Signin).Render(context.Background(), &html); err != nil {
		t.Fatal(err)
	}
	output := html.String()
	if !strings.Contains(output, `href="/auth/steam"`) || strings.Count(output, "Sign in through Steam") != 1 {
		t.Fatal("missing single Steam sign-in action")
	}
	for _, forbidden := range []string{`hx-post="/login"`, `href="/login"`, `name="steamID"`, "<input", "<form"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("signed-out home contains %q", forbidden)
		}
	}
}
