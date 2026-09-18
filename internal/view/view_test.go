package view

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gather-your-party/internal/middleware"
)

func TestSharedGamesListNoSelectionRendersPrompt(t *testing.T) {
	ctx := &middleware.CustomContext{Context: context.WithValue(context.Background(), "steamID", "user-1")}
	req := httptest.NewRequest(http.MethodGet, "/frag/shared", nil)
	res := httptest.NewRecorder()

	SharedGamesList(ctx, res, req)

	body := res.Body.String()
	if !strings.Contains(body, `Pick at least one friend, then click "Show games in common".`) {
		t.Fatalf("expected no-selection prompt, got %s", body)
	}
}

func TestSharedGamesListRequiresSigninWhenSteamIDMissing(t *testing.T) {
	ctx := &middleware.CustomContext{Context: context.Background()}
	req := httptest.NewRequest(http.MethodGet, "/frag/shared?friendID=friend-1", nil)
	res := httptest.NewRecorder()

	SharedGamesList(ctx, res, req)

	body := res.Body.String()
	if !strings.Contains(body, "Please provide your steam ID") {
		t.Fatalf("expected signin fragment, got %s", body)
	}
	if strings.Contains(body, "Pick at least one friend") {
		t.Fatalf("missing steamID should render signin, not shared-games prompt: %s", body)
	}
}

func TestSharedGameIDsPrependsPlayerToSelectedFriendIDs(t *testing.T) {
	ids := sharedGameIDs("user-1", []string{"friend-1", "friend-2"})

	want := []string{"user-1", "friend-1", "friend-2"}
	if len(ids) != len(want) {
		t.Fatalf("expected %d ids, got %d: %#v", len(want), len(ids), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids[%d] = %q, want %q in %#v", i, ids[i], want[i], ids)
		}
	}
}
