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
		{Game: steamapi.Game{AppID: 10, Name: "Counter-Strike 2"}},
		{Game: steamapi.Game{AppID: 20, Name: "Dota 2"}},
	}
	if err := SharedGamesWithCounts(games).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()

	// Assert SearchInput with data-items="#party-shared-games-grid li" is present
	wantInputAttr := `data-items="#party-shared-games-grid li"`
	if !strings.Contains(body, wantInputAttr) {
		t.Errorf("expected search input with %q, but not found in body: %s", wantInputAttr, body)
	}
	if !strings.Contains(body, `placeholder="Search games"`) {
		t.Errorf("expected placeholder=\"Search games\" in body: %s", body)
	}
	if !strings.Contains(body, `id="party-shared-games-grid"`) {
		t.Errorf("expected id=\"party-shared-games-grid\" in body: %s", body)
	}

	if strings.Contains(strings.ToLower(body), "players online now") {
		t.Errorf("expected 'players online now' to be absent case-insensitively: %s", body)
	}
	for _, unwanted := range []string{"137", "0 players", "online now"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(unwanted)) {
			t.Errorf("expected count text %q to be absent: %s", unwanted, body)
		}
	}
	rows := strings.Split(body, "</li>")
	// strings.Split produces len(games) item strings followed by trailing remainder after the last </li>
	if len(rows) != len(games)+1 {
		t.Fatalf("expected %d game rows, got %d: %s", len(games), len(rows)-1, body)
	}
	for i, g := range games {
		row := rows[i]

		// CLM-2: exactly one visible <li> per input in order, no hidden attribute or display:none initially
		if strings.Contains(row, "hidden") {
			t.Errorf("row %d has hidden attribute: %s", i, row)
		}
		if strings.Contains(row, "display:none") || strings.Contains(row, "display: none") {
			t.Errorf("row %d has display:none style: %s", i, row)
		}

		appIDStr := strconv.Itoa(g.Game.AppID)
		wantDataAttr := fmt.Sprintf(`data-appid="%s"`, appIDStr)
		if !strings.Contains(row, wantDataAttr) {
			t.Errorf("row %d missing %q: %s", i, wantDataAttr, row)
		}

		// CLM-5: each <li> carries lowercased data-name
		wantDataName := fmt.Sprintf(`data-name="%s"`, strings.ToLower(g.Game.Name))
		if !strings.Contains(row, wantDataName) {
			t.Errorf("row %d missing %q: %s", i, wantDataName, row)
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

		// Verify game name is rendered in slice order
		textWithoutTags := htmlTagRegex.ReplaceAllString(row, "")
		unescapedText := html.UnescapeString(textWithoutTags)
		trimmedText := strings.TrimSpace(unescapedText)
		if trimmedText != g.Game.Name {
			t.Errorf("row %d text content = %q, want exact game name %q", i, trimmedText, g.Game.Name)
		}
	}
}

// assertNoSearchAndGrid validates that neither <input nor party-shared-games-grid appears in the render.
// Returns an error if the invariants are violated, so it can discriminate when run against non-empty render.
func assertNoSearchAndGrid(body string) error {
	if strings.Contains(body, "<input") {
		return fmt.Errorf("unexpected <input found in output")
	}
	if strings.Contains(body, "party-shared-games-grid") {
		return fmt.Errorf("unexpected party-shared-games-grid found in output")
	}
	return nil
}

func TestSharedGamesWithCountsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := SharedGamesWithCounts(nil).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	emptyBody := buf.String()
	if !strings.Contains(emptyBody, "No games in common") || strings.Contains(emptyBody, "players online now") {
		t.Fatalf("incorrect empty state: %s", emptyBody)
	}

	// CLM-1: renders no search <input> and no party-shared-games-grid in empty state
	if err := assertNoSearchAndGrid(emptyBody); err != nil {
		t.Fatalf("empty state assertion failed: %v", err)
	}

	// Discriminating check: assertNoSearchAndGrid must fail when run against non-empty render
	var nonEmptyBuf bytes.Buffer
	games := []SharedGameCount{
		{Game: steamapi.Game{AppID: 10, Name: "Counter-Strike 2"}},
	}
	if err := SharedGamesWithCounts(games).Render(context.Background(), &nonEmptyBuf); err != nil {
		t.Fatal(err)
	}
	nonEmptyBody := nonEmptyBuf.String()
	if err := assertNoSearchAndGrid(nonEmptyBody); err == nil {
		t.Fatalf("expected assertNoSearchAndGrid to discriminate and fail on non-empty render, but it passed")
	}
}
