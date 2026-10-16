package component

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/softsrv/steamapi/steamapi"
)

var (
	htmlTagRegex = regexp.MustCompile(`<[^>]*>`)
)

func TestSharedGamesWithCounts(t *testing.T) {
	var buf bytes.Buffer
	games := []SharedGameCount{
		{Game: steamapi.Game{AppID: 10, Name: "Alyx & friends"}},
		{Game: steamapi.Game{AppID: 20, Name: "Quiet <game>"}},
	}
	if err := SharedGamesWithCounts(games).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if strings.Contains(strings.ToLower(body), "players online now") {
		t.Errorf("expected 'players online now' to be absent case-insensitively: %s", body)
	}
	for _, unwanted := range []string{"137", "0 players", "online now"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(unwanted)) {
			t.Errorf("expected count text %q to be absent: %s", unwanted, body)
		}
	}
	rows := strings.Split(body, "</li>")
	if len(rows) != 3 {
		t.Fatalf("expected two game rows: %s", body)
	}
	for i, g := range games {
		row := rows[i]
		appIDStr := strconv.Itoa(g.Game.AppID)
		wantDataAttr := fmt.Sprintf(`data-appid="%s"`, appIDStr)
		if !strings.Contains(row, wantDataAttr) {
			t.Errorf("row %d missing %q: %s", i, wantDataAttr, row)
		}
		wantImg := fmt.Sprintf(`src="%s"`, gameHeaderURL(g.Game.AppID))
		if !strings.Contains(row, wantImg) {
			t.Errorf("row %d missing header image %q: %s", i, wantImg, row)
		}
		if strings.Contains(strings.ToLower(row), "players online now") {
			t.Errorf("row %d contains 'players online now': %s", i, row)
		}
		if strings.Contains(row, "players") || strings.Contains(row, "online now") {
			t.Errorf("row %d contains count wording: %s", i, row)
		}

		// For every rendered <li data-appid>, assert its text content with whitespace trimmed
		// (tags stripped, HTML entities unescaped) equals exactly the expected game name.
		// A bare number such as '(42)' fails the test.
		textWithoutTags := htmlTagRegex.ReplaceAllString(row, "")
		unescapedText := html.UnescapeString(textWithoutTags)
		trimmedText := strings.TrimSpace(unescapedText)
		if trimmedText != g.Game.Name {
			t.Errorf("row %d text content = %q, want exact game name %q", i, trimmedText, g.Game.Name)
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
