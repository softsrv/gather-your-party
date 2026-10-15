package view

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gather-your-party/internal/component"
	"gather-your-party/internal/db"

	"github.com/softsrv/steamapi/steamapi"
)

type inviteFriendsStub struct {
	friends    []steamapi.Friend
	players    []steamapi.Player
	err        error
	playersErr error
	steamID    string
	playerIDs  []string
}

func (s *inviteFriendsStub) Friends(_ context.Context, steamID string) ([]steamapi.Friend, error) {
	s.steamID = steamID
	return s.friends, s.err
}

func (s *inviteFriendsStub) Players(_ context.Context, ids []string) ([]steamapi.Player, error) {
	s.playerIDs = append([]string(nil), ids...)
	if s.playersErr != nil {
		return nil, s.playersErr
	}
	return s.players, nil
}

type inviteCandidatesStub struct {
	candidates []db.InviteCandidate
	exclusions []string
	err        error
	exclErr    error
	calls      int
	partyID    string
	actor      int64
	friends    []string
}

func (s *inviteCandidatesStub) InviteCandidates(_ context.Context, partyID string, actor int64, friends []string) ([]db.InviteCandidate, error) {
	s.calls++
	s.partyID, s.actor, s.friends = partyID, actor, friends
	return s.candidates, s.err
}

func (s *inviteCandidatesStub) InviteExclusions(_ context.Context, _ string, _ []string) ([]string, error) {
	if s.exclErr != nil {
		return nil, s.exclErr
	}
	return s.exclusions, nil
}

func TestBuildInviteCandidates(t *testing.T) {
	service := &inviteFriendsStub{
		friends: []steamapi.Friend{
			{SteamID: "76561198000000001"},
			{SteamID: "76561198000000002"},
			{SteamID: "76561198000000003"},
			{SteamID: "76561198000000004"},
			{SteamID: "76561198000000005"},
		},
		players: []steamapi.Player{
			{SteamID: "76561198000000003", PersonaName: "Live player C", AvatarMedium: "https://example.com/avatar-c.jpg"},
		},
	}
	store := &inviteCandidatesStub{
		candidates: []db.InviteCandidate{
			{UserID: 101, SteamID: "76561198000000001", Name: "Stored A", AvatarURL: "https://example.com/avatar-a.jpg"},
			{UserID: 102, SteamID: "76561198000000002", Name: "Stored B", AvatarURL: "https://example.com/avatar-b.jpg"},
		},
		exclusions: []string{
			"76561198000000004",
			"76561198000000005",
		},
	}
	got, err := BuildInviteCandidates(context.Background(), service, store, "party", 123, "76561198000000099")
	if err != nil {
		t.Fatal(err)
	}
	if service.steamID != "76561198000000099" || store.calls != 1 || store.partyID != "party" || store.actor != 123 || !reflect.DeepEqual(store.friends, []string{"76561198000000001", "76561198000000002", "76561198000000003", "76561198000000004", "76561198000000005"}) {
		t.Fatalf("incorrect identity or lookup: service=%+v store=%+v", service, store)
	}
	want := []component.InviteCandidate{
		{UserID: 101, SteamID: "76561198000000001", Name: "Stored A", AvatarURL: "https://example.com/avatar-a.jpg"},
		{UserID: 102, SteamID: "76561198000000002", Name: "Stored B", AvatarURL: "https://example.com/avatar-b.jpg"},
		{UserID: 0, SteamID: "76561198000000003", Name: "Live player C", AvatarURL: "https://example.com/avatar-c.jpg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("picker model=%+v want=%+v", got, want)
	}
	if !reflect.DeepEqual(service.playerIDs, []string{"76561198000000003"}) {
		t.Fatalf("players requested=%v want [76561198000000003]", service.playerIDs)
	}
}

func TestBuildInviteCandidatesErrors(t *testing.T) {
	failure := errors.New("upstream unavailable")
	for _, fromFriends := range []bool{true, false} {
		service := &inviteFriendsStub{}
		store := &inviteCandidatesStub{}
		wantCalls := 1
		if fromFriends {
			service.err = failure
			wantCalls = 0
		} else {
			store.err = failure
		}
		got, err := BuildInviteCandidates(context.Background(), service, store, "party", 123, "verified")
		if !errors.Is(err, failure) || got != nil || store.calls != wantCalls {
			t.Fatalf("fromFriends=%t got=%v err=%v calls=%d", fromFriends, got, err, store.calls)
		}
	}

	// Exclusions lookup failure test
	serviceExcl := &inviteFriendsStub{
		friends: []steamapi.Friend{{SteamID: "76561198000000001"}},
	}
	storeExcl := &inviteCandidatesStub{exclErr: failure}
	got, err := BuildInviteCandidates(context.Background(), serviceExcl, storeExcl, "party", 123, "verified")
	if !errors.Is(err, failure) || got != nil {
		t.Fatalf("exclusions failure got=%v err=%v", got, err)
	}
}

func TestBuildInviteCandidatesPlayersErrorBestEffort(t *testing.T) {
	failure := errors.New("steam players api 500")
	service := &inviteFriendsStub{
		friends: []steamapi.Friend{
			{SteamID: "76561198000000001"},
			{SteamID: "76561198000000002"},
		},
		playersErr: failure,
	}
	store := &inviteCandidatesStub{
		candidates: []db.InviteCandidate{
			{UserID: 101, SteamID: "76561198000000001", Name: "Stored A", AvatarURL: "https://example.com/avatar-a.jpg"},
		},
	}
	got, err := BuildInviteCandidates(context.Background(), service, store, "party", 123, "verified")
	if err != nil {
		t.Fatalf("expected nil error on players failure, got: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %+v", len(got), got)
	}
	// First candidate is stored
	if got[0].UserID != 101 || got[0].SteamID != "76561198000000001" || got[0].Name != "Stored A" {
		t.Errorf("unexpected first candidate: %+v", got[0])
	}
	// Second candidate has no row, Players failed -> fallback to SteamID as Name and empty Avatar
	if got[1].UserID != 0 || got[1].SteamID != "76561198000000002" || got[1].Name != "76561198000000002" || got[1].AvatarURL != "" {
		t.Errorf("unexpected fallback candidate: %+v", got[1])
	}
}

func TestBuildInviteCandidatesExclusionsNotReadded(t *testing.T) {
	service := &inviteFriendsStub{
		friends: []steamapi.Friend{
			{SteamID: "76561198000000001"},
			{SteamID: "76561198000000002"},
			{SteamID: "76561198000000003"},
		},
		players: []steamapi.Player{
			{SteamID: "76561198000000002", PersonaName: "Live player 2"},
			{SteamID: "76561198000000003", PersonaName: "Live player 3"},
		},
	}
	store := &inviteCandidatesStub{
		candidates: nil,
		exclusions: []string{
			"76561198000000002", // member or pending invitee
		},
	}
	got, err := BuildInviteCandidates(context.Background(), service, store, "party", 123, "verified")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 76561198000000002 is in exclusions, so must not be in candidates.
	// 76561198000000001 and 76561198000000003 should be included.
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates (excluded 1), got %d: %+v", len(got), got)
	}
	for _, c := range got {
		if c.SteamID == "76561198000000002" {
			t.Errorf("excluded friend 76561198000000002 was re-added to candidates: %+v", c)
		}
	}
}
