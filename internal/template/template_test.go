package template

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gather-your-party/internal/component"

	"github.com/softsrv/steamapi/steamapi"
)

// CLM-2: PartyDetail with one member renders the invite note in the shared games
// section and avoids the empty-state and error-state messages.
func TestPartyDetailSharedGamesSoloMember(t *testing.T) {
	player := steamapi.Player{SteamID: "76561198000000001", PersonaName: "Leader"}
	soloMembers := []component.PartyMember{
		{Name: "Leader", IsLeader: true},
	}

	for _, tc := range []struct {
		name          string
		sharedGames   []component.SharedGameCount
		gamesMessages []string
	}{
		{name: "no games, no messages"},
		{name: "with messages", gamesMessages: []string{"Shared games cannot be computed."}},
		{name: "with shared games", sharedGames: []component.SharedGameCount{{Game: steamapi.Game{AppID: 10, Name: "Game"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			comp := PartyDetail(player, "party-123", "Test Party", soloMembers, true, nil, nil, tc.sharedGames, tc.gamesMessages)
			if err := comp.Render(context.Background(), &buf); err != nil {
				t.Fatalf("render error: %v", err)
			}
			out := buf.String()

			const wantSection = `<section aria-label="Shared games"`
			if !strings.Contains(out, wantSection) {
				t.Fatalf("missing shared games section in output: %s", out)
			}

			const wantNote = "Invite someone to your party to see the games you have in common."
			if !strings.Contains(out, wantNote) {
				t.Fatalf("missing invite note in solo render: %s", out)
			}

			for _, forbidden := range []string{
				"No games in common",
				"Shared games cannot be computed.",
				"We could not load shared games right now. Please try again in a moment.",
			} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("forbidden text %q rendered in solo party detail: %s", forbidden, out)
				}
			}
		})
	}
}

func TestPartyDetailSharedGamesMultipleMembers(t *testing.T) {
	player := steamapi.Player{SteamID: "76561198000000001", PersonaName: "Leader"}
	twoMembers := []component.PartyMember{
		{Name: "Leader", IsLeader: true},
		{Name: "Member", IsLeader: false},
	}

	t.Run("empty shared games renders empty state", func(t *testing.T) {
		var buf bytes.Buffer
		comp := PartyDetail(player, "party-123", "Test Party", twoMembers, true, nil, nil, nil, nil)
		if err := comp.Render(context.Background(), &buf); err != nil {
			t.Fatalf("render error: %v", err)
		}
		out := buf.String()

		const inviteNote = "Invite someone to your party to see the games you have in common."
		if strings.Contains(out, inviteNote) {
			t.Fatalf("invite note should not render for multi-member party: %s", out)
		}

		const wantEmpty = "No games in common"
		if !strings.Contains(out, wantEmpty) {
			t.Fatalf("expected %q empty state for multi-member party with no shared games: %s", wantEmpty, out)
		}
	})

	t.Run("error messages render error alert", func(t *testing.T) {
		var buf bytes.Buffer
		comp := PartyDetail(player, "party-123", "Test Party", twoMembers, true, nil, nil, nil, []string{"Shared games cannot be computed."})
		if err := comp.Render(context.Background(), &buf); err != nil {
			t.Fatalf("render error: %v", err)
		}
		out := buf.String()

		const inviteNote = "Invite someone to your party to see the games you have in common."
		if strings.Contains(out, inviteNote) {
			t.Fatalf("invite note should not render for multi-member party: %s", out)
		}

		if !strings.Contains(out, "Shared games cannot be computed.") {
			t.Fatalf("expected error message in multi-member party: %s", out)
		}
	})
}
