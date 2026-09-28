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
	friends []steamapi.Friend
	err     error
	steamID string
}

func (s *inviteFriendsStub) Friends(_ context.Context, steamID string) ([]steamapi.Friend, error) {
	s.steamID = steamID
	return s.friends, s.err
}

type inviteCandidatesStub struct {
	candidates []db.InviteCandidate
	err        error
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

func TestBuildInviteCandidates(t *testing.T) {
	service := &inviteFriendsStub{friends: []steamapi.Friend{{SteamID: "76561198000000002"}, {SteamID: "76561198000000003"}}}
	store := &inviteCandidatesStub{candidates: []db.InviteCandidate{{UserID: 987, Name: "Stored name"}}}
	got, err := BuildInviteCandidates(context.Background(), service, store, "party", 123, "76561198000000001")
	if err != nil {
		t.Fatal(err)
	}
	if service.steamID != "76561198000000001" || store.calls != 1 || store.partyID != "party" || store.actor != 123 || !reflect.DeepEqual(store.friends, []string{"76561198000000002", "76561198000000003"}) {
		t.Fatalf("incorrect identity or lookup: service=%+v store=%+v", service, store)
	}
	if !reflect.DeepEqual(got, []component.InviteCandidate{{UserID: 987, Name: "Stored name"}}) {
		t.Fatalf("picker model=%+v", got)
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
}
