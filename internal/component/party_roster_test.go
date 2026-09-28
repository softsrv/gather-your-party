package component

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestPartyRosterNamesAndAvatars(t *testing.T) {
	var buf bytes.Buffer
	members := []PartyMember{
		{Name: "Alyx & friends", AvatarURL: "https://example.com/alyx.jpg"},
		{Name: "Gordon <Freeman>", AvatarURL: "https://example.com/gordon.jpg"},
	}
	if err := PartyRoster(members).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{
		`<img src="https://example.com/alyx.jpg"`, `<span>Alyx &amp; friends</span>`,
		`<img src="https://example.com/gordon.jpg"`, `<span>Gordon &lt;Freeman&gt;</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("roster missing %q: %s", want, body)
		}
	}
	if strings.Count(body, "<li ") != len(members) || strings.Count(body, "<img ") != len(members) {
		t.Fatalf("expected one row and avatar per member: %s", body)
	}
}

func TestPartyRosterEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := PartyRoster(nil).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "<li") || strings.Contains(buf.String(), "<img") {
		t.Fatalf("empty roster contains members: %s", buf.String())
	}
}
