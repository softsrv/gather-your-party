package template

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

func TestFriendsListRendersSharedGamesFormWithFriendCheckboxes(t *testing.T) {
	friends := []steamapi.Player{
		{SteamID: "friend-1", PersonaName: "Friend One", AvatarSmall: "avatar-1.jpg"},
		{SteamID: "friend-2", PersonaName: "Friend Two", AvatarSmall: "avatar-2.jpg"},
	}

	var body bytes.Buffer
	if err := FriendsList(friends).Render(context.Background(), &body); err != nil {
		t.Fatalf("render FriendsList: %v", err)
	}
	html := body.String()

	for _, want := range []string{
		`<form hx-get="/frag/shared" hx-target="#game-list">`,
		`type="checkbox" name="friendID" value="friend-1"`,
		`type="checkbox" name="friendID" value="friend-2"`,
		`avatar-1.jpg`,
		`Friend One`,
		`Show games in common`,
		`class="btn btn-active btn-primary w-full flex justify-center text-gray-100 p-3  rounded-full tracking-wide font-semibold  shadow-lg cursor-pointer transition ease-in duration-200"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("FriendsList HTML missing %q in %s", want, html)
		}
	}
}

func TestSharedGamesPromptRendersNoSelectionPrompt(t *testing.T) {
	var body bytes.Buffer
	if err := SharedGamesPrompt().Render(context.Background(), &body); err != nil {
		t.Fatalf("render SharedGamesPrompt: %v", err)
	}

	want := `Pick at least one friend, then click "Show games in common".`
	if !strings.Contains(body.String(), want) {
		t.Fatalf("SharedGamesPrompt HTML missing %q in %s", want, body.String())
	}
}
