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

	"gather-your-party/internal/view"

	"github.com/softsrv/steamapi/steamapi"
)

type profileStubStore struct {
	fakeSessionStore
	resolveUserFound bool
	resolveUserErr   error
	profilePlayer    steamapi.Player
	profileFound     bool
	profileErr       error
	resolvedSteamID  string
}

func (s *profileStubStore) ResolveSession(context.Context, string) (string, bool, error) {
	if s.resolvedSteamID != "" {
		return s.resolvedSteamID, true, nil
	}
	return "", false, nil
}

func (s *profileStubStore) ResolveUserID(context.Context, string) (int64, bool, error) {
	return 42, s.resolveUserFound, s.resolveUserErr
}

func (s *profileStubStore) UserProfile(context.Context, string) (steamapi.Player, bool, error) {
	return s.profilePlayer, s.profileFound, s.profileErr
}

func profileTestCookie(secret string, token string) *http.Cookie {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(token))
	return &http.Cookie{Name: "session", Value: token + "." + hex.EncodeToString(mac.Sum(nil))}
}

// CLM-3 unit test: ResolveUserID returning found=false with a valid cookie produces 401.
func TestProfileRouteResolveUserNotFound(t *testing.T) {
	secret := "profile-unit-secret"
	store := &profileStubStore{
		resolvedSteamID:  testSteamID,
		resolveUserFound: false,
	}
	app := application{
		store:  store,
		config: appConfig{SessionSecret: secret},
		steam:  func() view.SteamService { return &fakeSteamRouteService{} },
	}

	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.AddCookie(profileTestCookie(secret, testSessionToken))
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
	body := strings.TrimSpace(w.Body.String())
	if body != "sign in to manage parties" {
		t.Fatalf("body = %q, want %q", body, "sign in to manage parties")
	}
	if strings.Contains(w.Body.String(), `id="calendars"`) {
		t.Errorf("unauthorized response must not contain calendars section: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<title>Profile</title>") {
		t.Errorf("unauthorized response must not contain Profile title: %s", w.Body.String())
	}
}

// CLM-4 unit test: ResolveUserID returning an error with a valid cookie produces 500 with ErrorPage
// and does NOT return plain-text "unable to resolve user" or calendars section.
func TestProfileRouteResolveUserError(t *testing.T) {
	secret := "profile-unit-secret"
	store := &profileStubStore{
		resolvedSteamID: testSteamID,
		resolveUserErr:  errors.New("db error"),
	}
	app := application{
		store:  store,
		config: appConfig{SessionSecret: secret},
		steam:  func() view.SteamService { return &fakeSteamRouteService{} },
	}

	req := httptest.NewRequest(http.MethodGet, "/profile", nil)
	req.AddCookie(profileTestCookie(secret, testSessionToken))
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	body := w.Body.String()
	if !strings.Contains(body, "unable to load your profile") {
		t.Errorf("body missing 'unable to load your profile': %s", body)
	}
	if strings.Contains(body, "unable to resolve user") {
		t.Errorf("body must not contain 'unable to resolve user': %s", body)
	}
	if strings.Contains(body, `id="calendars"`) {
		t.Errorf("error page must not contain calendars section: %s", body)
	}
}

// CLM-4 unit tests: UserProfile store error or found=false returns 500 with ErrorPage
// and no calendars section; positive control succeeds with 200.
func TestProfileRouteUserProfileErrorsAndSuccess(t *testing.T) {
	secret := "profile-unit-secret"

	t.Run("UserProfile error returns 500", func(t *testing.T) {
		store := &profileStubStore{
			resolvedSteamID:  testSteamID,
			resolveUserFound: true,
			profileErr:       errors.New("db failure"),
		}
		app := application{
			store:  store,
			config: appConfig{SessionSecret: secret},
			steam:  func() view.SteamService { return &fakeSteamRouteService{} },
		}

		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		req.AddCookie(profileTestCookie(secret, testSessionToken))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
		}
		body := w.Body.String()
		if !strings.Contains(body, "unable to load your profile") {
			t.Errorf("body missing 'unable to load your profile': %s", body)
		}
		if strings.Contains(body, `id="calendars"`) {
			t.Errorf("error page must not contain calendars section: %s", body)
		}
	})

	t.Run("UserProfile not found returns 500", func(t *testing.T) {
		store := &profileStubStore{
			resolvedSteamID:  testSteamID,
			resolveUserFound: true,
			profileFound:     false,
		}
		app := application{
			store:  store,
			config: appConfig{SessionSecret: secret},
			steam:  func() view.SteamService { return &fakeSteamRouteService{} },
		}

		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		req.AddCookie(profileTestCookie(secret, testSessionToken))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
		}
		body := w.Body.String()
		if !strings.Contains(body, "unable to load your profile") {
			t.Errorf("body missing 'unable to load your profile': %s", body)
		}
		if strings.Contains(body, `id="calendars"`) {
			t.Errorf("error page must not contain calendars section: %s", body)
		}
	})

	t.Run("UserProfile success returns 200", func(t *testing.T) {
		store := &profileStubStore{
			resolvedSteamID:  testSteamID,
			resolveUserFound: true,
			profileFound:     true,
			profilePlayer: steamapi.Player{
				SteamID:      testSteamID,
				PersonaName:  "Gordon",
				AvatarFull:   "https://example.com/avatar_full.jpg",
				AvatarMedium: "https://example.com/avatar_med.jpg",
			},
		}
		app := application{
			store:  store,
			config: appConfig{SessionSecret: secret},
			steam:  func() view.SteamService { return &fakeSteamRouteService{} },
		}

		req := httptest.NewRequest(http.MethodGet, "/profile", nil)
		req.AddCookie(profileTestCookie(secret, testSessionToken))
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		body := w.Body.String()
		if !strings.Contains(body, "Gordon") {
			t.Errorf("body missing PersonaName Gordon: %s", body)
		}
		if !strings.Contains(body, `id="calendars"`) {
			t.Errorf("success page missing calendars section: %s", body)
		}
	})
}
