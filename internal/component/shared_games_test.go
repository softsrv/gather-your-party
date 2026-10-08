package component

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

func TestSharedGamesWithCounts(t *testing.T) {
	var buf bytes.Buffer
	games := []SharedGameCount{
		{Game: steamapi.Game{AppID: 10, Name: "Alyx & friends"}, PlayerCount: 137},
		{Game: steamapi.Game{AppID: 20, Name: "Quiet <game>"}, PlayerCount: 0},
	}
	if err := SharedGamesWithCounts(games).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	rows := strings.Split(body, "</li>")
	if len(rows) != 3 {
		t.Fatalf("expected two game rows: %s", body)
	}
	for i, wants := range [][]string{
		{`data-appid="10"`, "Alyx &amp; friends", "137 players online now"},
		{`data-appid="20"`, "Quiet &lt;game&gt;", "0 players online now"},
	} {
		for _, want := range wants {
			if !strings.Contains(rows[i], want) {
				t.Errorf("row %d missing %q: %s", i, want, rows[i])
			}
		}
	}
}

func TestSharedGamesWithCountsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := SharedGamesWithCounts(nil).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "No games in common") || strings.Contains(buf.String(), "players online now") {
		t.Fatalf("incorrect empty state: %s", buf.String())
	}
}
