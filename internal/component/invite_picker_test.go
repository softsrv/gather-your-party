package component

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestInvitePicker(t *testing.T) {
	t.Run("zero candidates does not contain signed-in wording", func(t *testing.T) {
		var buf bytes.Buffer
		if err := InvitePicker("party-123", nil).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render failed: %v", err)
		}
		output := buf.String()

		signedInWordingSnippets := []string{
			"signed in",
			"signed-in",
			"sign in",
			"once they've signed in",
			"who use Gather Your Party",
		}
		for _, snippet := range signedInWordingSnippets {
			if strings.Contains(strings.ToLower(output), strings.ToLower(snippet)) {
				t.Errorf("expected output not to contain %q, but got: %s", snippet, output)
			}
		}

		if !strings.Contains(output, "No friends are available to invite.") {
			t.Errorf("expected empty-state copy 'No friends are available to invite.', got: %s", output)
		}
	})

	t.Run("one candidate with no users id carries Steam id as target", func(t *testing.T) {
		candidate := InviteCandidate{
			UserID:    0,
			SteamID:   "76561198000000001",
			Name:      "Gordon Freeman",
			AvatarURL: "https://example.com/avatar.jpg",
		}

		var buf bytes.Buffer
		if err := InvitePicker("party-456", []InviteCandidate{candidate}).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render failed: %v", err)
		}
		output := buf.String()

		wantTarget := `name="target" value="76561198000000001"`
		if !strings.Contains(output, wantTarget) {
			t.Errorf("expected form target field %q, got: %s", wantTarget, output)
		}

		signedInWordingSnippets := []string{
			"signed in",
			"signed-in",
			"who use Gather Your Party",
		}
		for _, snippet := range signedInWordingSnippets {
			if strings.Contains(strings.ToLower(output), strings.ToLower(snippet)) {
				t.Errorf("expected output not to contain %q, but got: %s", snippet, output)
			}
		}
	})
}
