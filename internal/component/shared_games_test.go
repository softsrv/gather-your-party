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
	htmlTagRegex       = regexp.MustCompile(`<[^>]*>`)
	liElementRegex     = regexp.MustCompile(`(?s)<li\b([^>]*)>(.*?)</li>`)
	quotedAttrRegex    = regexp.MustCompile(`"[^"]*"`)
	openingHiddenRegex = regexp.MustCompile(`(?i)(^|\s)hidden(\s|=|$)`)
	openingStyleRegex  = regexp.MustCompile(`(?i)style="[^"]*display:\s*none`)
)

func hasHiddenAttribute(attrs string) bool {
	stripped := quotedAttrRegex.ReplaceAllString(attrs, `""`)
	return openingHiddenRegex.MatchString(stripped)
}

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
	gridIdx := strings.Index(body, `id="party-shared-games-grid"`)
	if gridIdx == -1 {
		t.Fatalf("expected id=\"party-shared-games-grid\" in body: %s", body)
	}

	if strings.Contains(strings.ToLower(body), "players online now") {
		t.Errorf("expected 'players online now' to be absent case-insensitively: %s", body)
	}
	for _, unwanted := range []string{"137", "0 players", "online now"} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(unwanted)) {
			t.Errorf("expected count text %q to be absent: %s", unwanted, body)
		}
	}

	// Restrict row parsing to the <ul> markup after id="party-shared-games-grid" up to </ul>
	afterGrid := body[gridIdx:]
	endUlIdx := strings.Index(afterGrid, "</ul>")
	if endUlIdx == -1 {
		t.Fatalf("expected closing </ul> after grid, but not found in body: %s", body)
	}
	ulMarkup := afterGrid[:endUlIdx]

	matches := liElementRegex.FindAllStringSubmatch(ulMarkup, -1)
	if len(matches) != len(games) {
		t.Fatalf("expected %d game <li> elements, got %d in grid markup: %s", len(games), len(matches), ulMarkup)
	}

	// CLM-2: exactly one visible <li> per input in order, no hidden attribute or display:none initially on opening tag
	if !hasHiddenAttribute(` hidden data-x="1"`) {
		t.Fatal("expected hasHiddenAttribute to return true for real hidden attribute")
	}
	if hasHiddenAttribute(` class="overflow-hidden"`) {
		t.Fatal("expected hasHiddenAttribute to return false for class=\"overflow-hidden\"")
	}

	for i, g := range games {
		openingAttrs := matches[i][1]
		innerContent := matches[i][2]

		// CLM-2: exactly one visible <li> per input in order, no hidden attribute or display:none initially on opening tag
		if hasHiddenAttribute(openingAttrs) {
			t.Errorf("row %d has hidden attribute on opening tag: %s", i, openingAttrs)
		}
		if openingStyleRegex.MatchString(openingAttrs) {
			t.Errorf("row %d has display:none on opening tag: %s", i, openingAttrs)
		}

		appIDStr := strconv.Itoa(g.Game.AppID)
		wantDataAttr := fmt.Sprintf(`data-appid="%s"`, appIDStr)
		if !strings.Contains(openingAttrs, wantDataAttr) {
			t.Errorf("row %d opening tag missing %q: %s", i, wantDataAttr, openingAttrs)
		}

		// CLM-5: each <li> carries lowercased data-name
		wantDataName := fmt.Sprintf(`data-name="%s"`, strings.ToLower(g.Game.Name))
		if !strings.Contains(openingAttrs, wantDataName) {
			t.Errorf("row %d opening tag missing %q: %s", i, wantDataName, openingAttrs)
		}

		wantImg := fmt.Sprintf(`src="%s"`, gameHeaderURL(g.Game.AppID))
		if !strings.Contains(innerContent, wantImg) {
			t.Errorf("row %d missing header image %q: %s", i, wantImg, innerContent)
		}
		if strings.Contains(strings.ToLower(innerContent), "players online now") {
			t.Errorf("row %d contains 'players online now': %s", i, innerContent)
		}
		if strings.Contains(innerContent, "players") || strings.Contains(innerContent, "online now") {
			t.Errorf("row %d contains count wording: %s", i, innerContent)
		}

		// Verify game name is rendered in slice order from inside the li only, stripping tags
		textWithoutTags := htmlTagRegex.ReplaceAllString(innerContent, "")
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
