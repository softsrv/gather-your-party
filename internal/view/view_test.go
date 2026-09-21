package view

import (
	"errors"
	"reflect"
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

func TestNoGamesMessagesUsesPersonaNamesFromRoster(t *testing.T) {
	roster := []steamapi.Player{
		{SteamID: "friend-1", PersonaName: "Alyx"},
		{SteamID: "friend-2", PersonaName: "Barney"},
	}

	got := noGamesMessages([]string{"friend-1", "friend-2"}, roster)
	want := []string{
		"no games found for user Alyx. Their list may be private",
		"no games found for user Barney. Their list may be private",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("noGamesMessages() = %#v, want %#v", got, want)
	}
}

func TestNoGamesMessagesFallsBackToSteamID(t *testing.T) {
	got := noGamesMessages([]string{"unknown-id"}, []steamapi.Player{{SteamID: "friend-1", PersonaName: "Alyx"}})
	want := []string{"no games found for user unknown-id. Their list may be private"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("noGamesMessages() = %#v, want %#v", got, want)
	}
}

func TestSharedGamesNoGamesMessagesOnlyHandlesNoGamesError(t *testing.T) {
	roster := []steamapi.Player{{SteamID: "friend-1", PersonaName: "Alyx"}}

	got, ok := sharedGamesNoGamesMessages(&steamapi.NoGamesError{SteamIDs: []string{"friend-1"}}, roster)
	if !ok {
		t.Fatal("sharedGamesNoGamesMessages() ok = false, want true for NoGamesError")
	}
	want := []string{"no games found for user Alyx. Their list may be private"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sharedGamesNoGamesMessages() = %#v, want %#v", got, want)
	}

	got, ok = sharedGamesNoGamesMessages(errors.New("plain steamapi error"), roster)
	if ok {
		t.Fatalf("sharedGamesNoGamesMessages() ok = true, want false for plain error with messages %#v", got)
	}
}
