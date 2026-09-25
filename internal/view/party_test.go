package view

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
)

type partyStoreStub struct {
	resolved    string
	found       bool
	resolveErr  error
	mutationErr error
	calls       int
	actor       int64
	target      int64
	party       string
	name        string
}

func (s *partyStoreStub) ResolveUserID(_ context.Context, steamID string) (int64, bool, error) {
	s.resolved = steamID
	return 42, s.found, s.resolveErr
}

func (s *partyStoreStub) CreateParty(_ context.Context, name string, actor int64) (string, error) {
	s.calls++
	s.actor, s.name = actor, name
	return "new-party", s.mutationErr
}

func (s *partyStoreStub) LeaveParty(_ context.Context, party string, actor int64) error {
	s.calls++
	s.actor, s.party = actor, party
	return s.mutationErr
}

func (s *partyStoreStub) StepDown(_ context.Context, party string, actor, target int64) error {
	s.calls++
	s.actor, s.party, s.target = actor, party, target
	return s.mutationErr
}

func TestPartyActionIdentityAndResponses(t *testing.T) {
	for _, action := range []string{"create", "leave", "step down"} {
		for _, tc := range []struct {
			name        string
			identity    any
			found       bool
			resolveErr  error
			mutationErr error
			method      string
			htmx        bool
			status      int
			calls       int
		}{
			{name: "absent", method: "POST", status: 401},
			{name: "wrong type", identity: int64(42), method: "POST", status: 401},
			{name: "empty", identity: "", method: "POST", status: 401},
			{name: "unknown", identity: "verified", method: "POST", status: 401},
			{name: "resolution failure", identity: "verified", resolveErr: errors.New("private details"), method: "POST", status: 500},
			{name: "wrong method", identity: "verified", found: true, method: "GET", status: 405},
			{name: "success", identity: "verified", found: true, method: "POST", status: 303, calls: 1},
			{name: "htmx success", identity: "verified", found: true, method: "POST", htmx: true, status: 204, calls: 1},
			{name: "store failure", identity: "verified", found: true, method: "POST", mutationErr: errors.New("private details"), status: 500, calls: 1},
		} {
			t.Run(action+"/"+tc.name, func(t *testing.T) {
				store := &partyStoreStub{found: tc.found, resolveErr: tc.resolveErr, mutationErr: tc.mutationErr}
				actions := PartyActions{Store: store}
				handler := map[string]middleware.CustomHandler{"create": actions.CreateParty, "leave": actions.LeaveParty, "step down": actions.StepDown}[action]
				ctx := context.Background()
				if tc.identity != nil {
					ctx = context.WithValue(ctx, middleware.SteamID{}, tc.identity)
				}
				r := httptest.NewRequest(tc.method, "/parties", strings.NewReader("name=+Game+night+&targetUserID=73&userID=999"))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if tc.htmx {
					r.Header.Set("HX-Request", "true")
				}
				r.SetPathValue("partyID", "party-123")
				w := httptest.NewRecorder()
				handler(&middleware.CustomContext{Context: ctx}, w, r)
				if w.Code != tc.status || store.calls != tc.calls {
					t.Fatalf("status=%d calls=%d, want %d/%d", w.Code, store.calls, tc.status, tc.calls)
				}
				if strings.Contains(w.Body.String(), "private details") {
					t.Fatal("response leaked store error")
				}
				if tc.calls > 0 {
					if store.resolved != "verified" || store.actor != 42 {
						t.Fatal("mutation did not use the resolved identity")
					}
					if action == "create" && store.name != "Game night" {
						t.Fatalf("name = %q", store.name)
					}
					if action != "create" && store.party != "party-123" {
						t.Fatal("wrong party")
					}
					if action == "step down" && store.target != 73 {
						t.Fatal("wrong target")
					}
				}
				if w.Code == 303 && w.Header().Get("Location") != "/parties" {
					t.Fatal("missing redirect")
				}
				if w.Code == 204 && (w.Header().Get("HX-Redirect") != "/parties" || w.Body.Len() != 0) {
					t.Fatal("missing generic htmx completion")
				}
			})
		}
	}
}

func TestPartyActionsRejectInvalidForms(t *testing.T) {
	for _, tc := range []struct{ action, party, form string }{
		{"create", "", "name=+"},
		{"create", "", "name=%zz"},
		{"leave", "", ""},
		{"step down", "party", "targetUserID=bad"},
		{"step down", "party", "targetUserID=0"},
		{"step down", "party", "targetUserID=-1"},
		{"step down", "", "targetUserID=73"},
		{"step down", "party", "targetUserID=%zz"},
	} {
		t.Run(tc.action+"/"+tc.party+"/"+tc.form, func(t *testing.T) {
			store := &partyStoreStub{found: true}
			actions := PartyActions{Store: store}
			handler := map[string]middleware.CustomHandler{"create": actions.CreateParty, "leave": actions.LeaveParty, "step down": actions.StepDown}[tc.action]
			ctx := context.WithValue(context.Background(), middleware.SteamID{}, "verified")
			r := httptest.NewRequest("POST", "/parties", strings.NewReader(tc.form))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.SetPathValue("partyID", tc.party)
			w := httptest.NewRecorder()
			handler(&middleware.CustomContext{Context: ctx}, w, r)
			if w.Code != 400 || store.calls != 0 {
				t.Fatalf("invalid form: status=%d mutations=%d", w.Code, store.calls)
			}
		})
	}
}

func TestStepDownRefusalResponse(t *testing.T) {
	store := &partyStoreStub{found: true, mutationErr: db.ErrStepDownNotAllowed}
	ctx := context.WithValue(context.Background(), middleware.SteamID{}, "verified")
	r := httptest.NewRequest("POST", "/parties", strings.NewReader("targetUserID=73"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("partyID", "party-123")
	w := httptest.NewRecorder()
	PartyActions{Store: store}.StepDown(&middleware.CustomContext{Context: ctx}, w, r)
	if w.Code != http.StatusForbidden || w.Header().Get("HX-Redirect") != "" {
		t.Fatalf("refusal response = %d", w.Code)
	}
}
