package template

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gather-your-party/internal/component"

	"github.com/a-h/templ"
	"github.com/softsrv/steamapi/steamapi"
)

// CLM-2: PartyDetail with one member renders the invite note in the shared games
// section and avoids the empty-state and error-state messages.
func TestPartyDetailSharedGamesSoloMember(t *testing.T) {
	player := steamapi.Player{SteamID: "76561198000000001", PersonaName: "Leader"}
	soloMembers := []component.PartyMember{
		{Name: "Leader", IsLeader: true},
	}

	for _, tc := range []struct {
		name          string
		sharedGames   []component.SharedGameCount
		gamesMessages []string
	}{
		{name: "no games, no messages"},
		{name: "with messages", gamesMessages: []string{"Shared games cannot be computed."}},
		{name: "with shared games", sharedGames: []component.SharedGameCount{{Game: steamapi.Game{AppID: 10, Name: "Game"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			comp := PartyDetail(player, "party-123", "Test Party", soloMembers, true, nil, nil, tc.sharedGames, tc.gamesMessages, nil)
			if err := comp.Render(context.Background(), &buf); err != nil {
				t.Fatalf("render error: %v", err)
			}
			out := buf.String()

			const wantSection = `<section aria-label="Shared games"`
			if !strings.Contains(out, wantSection) {
				t.Fatalf("missing shared games section in output: %s", out)
			}

			const wantNote = "Invite someone to your party to see the games you have in common."
			if !strings.Contains(out, wantNote) {
				t.Fatalf("missing invite note in solo render: %s", out)
			}

			for _, forbidden := range []string{
				"No games in common",
				"Shared games cannot be computed.",
				"We could not load shared games right now. Please try again in a moment.",
			} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("forbidden text %q rendered in solo party detail: %s", forbidden, out)
				}
			}
		})
	}
}

func TestPartyDetailSharedGamesMultipleMembers(t *testing.T) {
	player := steamapi.Player{SteamID: "76561198000000001", PersonaName: "Leader"}
	twoMembers := []component.PartyMember{
		{Name: "Leader", IsLeader: true},
		{Name: "Member", IsLeader: false},
	}

	t.Run("empty shared games renders empty state", func(t *testing.T) {
		var buf bytes.Buffer
		comp := PartyDetail(player, "party-123", "Test Party", twoMembers, true, nil, nil, nil, nil, nil)
		if err := comp.Render(context.Background(), &buf); err != nil {
			t.Fatalf("render error: %v", err)
		}
		out := buf.String()

		const inviteNote = "Invite someone to your party to see the games you have in common."
		if strings.Contains(out, inviteNote) {
			t.Fatalf("invite note should not render for multi-member party: %s", out)
		}

		const wantEmpty = "No games in common"
		if !strings.Contains(out, wantEmpty) {
			t.Fatalf("expected %q empty state for multi-member party with no shared games: %s", wantEmpty, out)
		}
	})

	t.Run("error messages render error alert", func(t *testing.T) {
		var buf bytes.Buffer
		comp := PartyDetail(player, "party-123", "Test Party", twoMembers, true, nil, nil, nil, []string{"Shared games cannot be computed."}, nil)
		if err := comp.Render(context.Background(), &buf); err != nil {
			t.Fatalf("render error: %v", err)
		}
		out := buf.String()

		const inviteNote = "Invite someone to your party to see the games you have in common."
		if strings.Contains(out, inviteNote) {
			t.Fatalf("invite note should not render for multi-member party: %s", out)
		}

		if !strings.Contains(out, "Shared games cannot be computed.") {
			t.Fatalf("expected error message in multi-member party: %s", out)
		}
	})
}

func renderString(t *testing.T, comp templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := comp.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render error: %v", err)
	}
	return buf.String()
}

func renderPage(t *testing.T) string {
	t.Helper()
	child := templ.Raw("<p>child content</p>")
	ctx := templ.WithChildren(context.Background(), child)
	var buf bytes.Buffer
	if err := Page("t", component.NavState{}).Render(ctx, &buf); err != nil {
		t.Fatalf("render error: %v", err)
	}
	return buf.String()
}

// CLM-4: every full page carries exactly one footer after </main>, with the
// copyright line and the Valve disclaimer.
func TestPageFooterShowsCopyrightAndValveDisclaimer(t *testing.T) {
	player := steamapi.Player{SteamID: "76561198000000001", PersonaName: "Leader"}
	members := []component.PartyMember{{Name: "Leader", IsLeader: true}}

	pages := []struct {
		name string
		out  string
	}{
		{"Page", renderPage(t)},
		{"Home signin", renderString(t, Home(steamapi.Player{}, "t", Signin))},
		{"Home main", renderString(t, Home(player, "t", Main))},
		{"ErrorPage", renderString(t, ErrorPage("t", "boom"))},
		{"PartiesPage", renderString(t, PartiesPage(player, nil, nil))},
		{"PartyDetail", renderString(t, PartyDetail(player, "party-1", "Party", members, true, nil, nil, nil, nil, nil))},
		{"LeaveParty", renderString(t, LeaveParty(player, "party-1"))},
	}
	for _, tc := range pages {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.out
			if n := strings.Count(out, "<footer"); n != 1 {
				t.Fatalf("want exactly one <footer, got %d in: %s", n, out)
			}
			footerAt := strings.Index(out, "<footer")
			mainEnd := strings.LastIndex(out, "</main>")
			if mainEnd < 0 || footerAt < mainEnd {
				t.Fatalf("footer must come after </main> (main end %d, footer %d)", mainEnd, footerAt)
			}
			footer := out[footerAt:strings.Index(out, "</footer>")]
			if !strings.Contains(footer, "2026 softsrv") {
				t.Fatalf("footer lacks copyright line: %s", footer)
			}
			if !strings.Contains(footer, "\u00a9") && !strings.Contains(footer, "&copy;") && !strings.Contains(footer, "©") {
				t.Fatalf("footer lacks copyright symbol: %s", footer)
			}
			if !strings.Contains(footer, "Not affiliated with Valve") {
				t.Fatalf("footer lacks Valve disclaimer: %s", footer)
			}
		})
	}
}

func tagClasses(t *testing.T, out, tag string) []string {
	t.Helper()
	m := regexp.MustCompile(`<` + tag + `[^>]*\sclass="([^"]*)"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no <%s class=...> in output: %s", tag, out)
	}
	return strings.Fields(m[1])
}

func hasAll(have []string, want ...string) (missing string, ok bool) {
	set := map[string]bool{}
	for _, c := range have {
		set[c] = true
	}
	for _, w := range want {
		if !set[w] {
			return w, false
		}
	}
	return "", true
}

// CLM-5: the footer sits at the bottom of a short page.
func TestPageLayoutPinsFooterToWindowBottom(t *testing.T) {
	out := renderPage(t)

	if m, ok := hasAll(tagClasses(t, out, "body"), "min-h-screen", "flex", "flex-col"); !ok {
		t.Errorf("<body> class lacks %q", m)
	}
	if m, ok := hasAll(tagClasses(t, out, "main"), "flex-1", "mx-auto", "max-w-6xl"); !ok {
		t.Errorf("<main> class lacks %q", m)
	}
	if !strings.Contains(out, "</footer></body>") {
		t.Errorf("footer must be the last element in <body>")
	}

	// The committed stylesheet must define the utilities the layout relies on.
	css, err := os.ReadFile(filepath.Join("..", "..", "static", "css", "output.css"))
	if err != nil {
		t.Fatalf("read stylesheet: %v", err)
	}
	for _, class := range []string{".flex-col", ".flex-1", ".min-h-screen"} {
		if !regexp.MustCompile(regexp.QuoteMeta(class) + `\s*\{`).Match(css) {
			t.Errorf("stylesheet does not define %s", class)
		}
	}
}

func assertNoFooter(out string) (problems []string) {
	if strings.Contains(out, "<footer") {
		problems = append(problems, "has <footer")
	}
	if strings.Contains(out, "2026 softsrv") {
		problems = append(problems, "has 2026 softsrv")
	}
	return problems
}

// CLM-2: fragments swapped into an open page carry no footer of their own.
func TestFragmentsHaveNoFooter(t *testing.T) {
	friends := []steamapi.Player{{SteamID: "76561198000000002", PersonaName: "Friend"}}
	games := []steamapi.Game{{AppID: 10, Name: "Game"}}
	fragments := map[string]templ.Component{
		"ErrorMessage":      ErrorMessage("something failed"),
		"ErrorMessages":     ErrorMessages([]string{"one", "two"}),
		"SharedGamesList":   SharedGamesList(games),
		"FriendsList":       FriendsList(friends),
		"LibrarySyncStatus": component.LibrarySyncStatus(true, "sync failed"),
	}
	for name, comp := range fragments {
		t.Run(name, func(t *testing.T) {
			out := renderString(t, comp)
			if strings.TrimSpace(out) == "" {
				t.Fatal("fragment rendered no output")
			}
			if p := assertNoFooter(out); len(p) != 0 {
				t.Fatalf("fragment %v: %s", p, out)
			}
		})
	}

	// The check discriminates: against a full page it finds both.
	if p := assertNoFooter(renderPage(t)); len(p) != 2 {
		t.Fatalf("assertNoFooter should flag Page's footer and copyright, got %v", p)
	}
}

// scanTempl reports document-shell and @Base( uses outside their one owner:
// the shell belongs to templ Base, and only templ Page may invoke @Base.
func scanTempl(name, src string) (problems []string) {
	current := ""
	componentStart := regexp.MustCompile(`^templ\s+([A-Za-z0-9_]+)\(`)
	for i, line := range strings.Split(src, "\n") {
		if m := componentStart.FindStringSubmatch(line); m != nil {
			current = m[1]
		} else if line == "}" {
			current = ""
		}
		lower := strings.ToLower(line)
		if (strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<html")) && current != "Base" {
			problems = append(problems, name+":"+strconv.Itoa(i+1)+": document shell outside templ Base")
		}
		if strings.Contains(line, "@Base(") && current != "Page" {
			problems = append(problems, name+":"+strconv.Itoa(i+1)+": @Base( outside templ Page")
		}
	}
	return problems
}

// scanGo reports hand-written HTML document shells in non-generated Go code.
func scanGo(name, src string) (problems []string) {
	for i, line := range strings.Split(src, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<html") {
			problems = append(problems, name+":"+strconv.Itoa(i+1)+": raw document shell written in Go")
		}
	}
	return problems
}

// CLM-1: Base is the only document shell, only Page invokes it, and no handler
// writes a full HTML document by hand. The scans must flag violations.
func TestOnlyBaseEmitsDocumentShell(t *testing.T) {
	t.Run("scanners flag violations", func(t *testing.T) {
		good := "templ Base(title string) {\n\t<!DOCTYPE html>\n\t<html>\n}\n\ntempl Page(t string) {\n\t@Base(t) {\n\t}\n}\n"
		if p := scanTempl("good.templ", good); len(p) != 0 {
			t.Fatalf("clean fixture flagged: %v", p)
		}
		second := good + "\ntempl Other() {\n\t@Base(\"x\") {\n\t}\n}\n"
		if p := scanTempl("second.templ", second); len(p) != 1 {
			t.Fatalf("second @Base caller not flagged: %v", p)
		}
		shell := good + "\ntempl Other() {\n\t<!DOCTYPE html>\n}\n"
		if p := scanTempl("shell.templ", shell); len(p) != 1 {
			t.Fatalf("second document shell not flagged: %v", p)
		}
		if p := scanGo("raw.go", "w.Write([]byte(\"<!DOCTYPE html><html>\"))\n"); len(p) != 1 {
			t.Fatalf("raw DOCTYPE write not flagged: %v", p)
		}
		if p := scanGo("ok.go", "w.WriteHeader(200)\n"); len(p) != 0 {
			t.Fatalf("clean Go flagged: %v", p)
		}
	})

	t.Run("repository", func(t *testing.T) {
		root := filepath.Join("..", "..")
		scanned := 0
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "tmp", "vendor":
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			var scan func(string, string) []string
			switch {
			case strings.HasSuffix(name, ".templ"):
				scan = scanTempl
			case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_templ.go") && !strings.HasSuffix(name, "_test.go"):
				scan = scanGo
			default:
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for _, p := range scan(path, string(src)) {
				t.Error(p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if scanned == 0 {
			t.Fatal("scanned no files")
		}
	})
}

func extractElementByID(body, id, tag string) string {
	openPattern := regexp.MustCompile(`<` + tag + `[^>]*\bid="` + regexp.QuoteMeta(id) + `"[^>]*>`)
	loc := openPattern.FindStringIndex(body)
	if loc == nil {
		return ""
	}
	start := loc[0]
	endTag := "</" + tag + ">"
	endIdx := strings.Index(body[start:], endTag)
	if endIdx == -1 {
		return ""
	}
	return body[start : start+endIdx+len(endTag)]
}

// CLM-1, CLM-2, CLM-3, CLM-4, CLM-5: PartyDetail tabs structure and content isolation
func TestPartyDetailTabs(t *testing.T) {
	player := steamapi.Player{SteamID: "76561198000000001", PersonaName: "Leader"}
	leaderMember := component.PartyMember{Name: "LeaderPerson", IsLeader: true}
	otherMember := component.PartyMember{Name: "OtherPerson", IsLeader: false}
	twoMembers := []component.PartyMember{leaderMember, otherMember}
	games := []component.SharedGameCount{{Game: steamapi.Game{AppID: 10, Name: "Counter-Strike"}}}
	inviteCandidates := []component.InviteCandidate{{SteamID: "76561198000000003", Name: "CandidateFriend"}}
	stepDownCandidates := []component.StepDownCandidate{{UserID: 2, Name: "OtherPerson"}}

	// CLM-1 & CLM-3: Structure, radio attributes, and exact DaisyUI classes
	t.Run("tablist structure and classes (CLM-1, CLM-3)", func(t *testing.T) {
		body := renderString(t, PartyDetail(player, "party-123", "Test Party", twoMembers, true, inviteCandidates, stepDownCandidates, games, nil, nil))

		const wantTablist = `<div role="tablist" class="tabs tabs-border">`
		if strings.Count(body, wantTablist) != 1 {
			t.Fatalf("expected exactly one tablist %q, got count %d", wantTablist, strings.Count(body, wantTablist))
		}

		tablistStart := strings.Index(body, wantTablist)
		if tablistStart == -1 {
			t.Fatalf("could not find tablist div: %s", wantTablist)
		}
		// The tablist div closes with </div> right before <section aria-label="Shared games"
		sharedGamesStart := strings.Index(body[tablistStart:], `<section aria-label="Shared games"`)
		if sharedGamesStart == -1 {
			t.Fatalf("could not find shared games section after tablist")
		}
		tablistContent := body[tablistStart : tablistStart+sharedGamesStart]

		for _, want := range []string{
			`type="radio" name="party-tabs" id="party-members-tab" role="tab" class="tab whitespace-nowrap font-medium" aria-label="Members" aria-controls="party-members-panel" checked`,
			`id="party-members-panel" role="tabpanel" aria-labelledby="party-members-tab" class="tab-content rounded-box border-base-300 bg-base-100 p-4 sm:p-6"`,
			`type="radio" name="party-tabs" id="party-events-tab" role="tab" class="tab whitespace-nowrap font-medium" aria-label="Events" aria-controls="party-events-panel"`,
			`id="party-events-panel" role="tabpanel" aria-labelledby="party-events-tab" class="tab-content rounded-box border-base-300 bg-base-100 p-4 sm:p-6"`,
		} {
			if !strings.Contains(tablistContent, want) {
				t.Errorf("tablist missing expected substring: %s", want)
			}
		}

		if strings.Contains(tablistContent, "parties-tabs") {
			t.Errorf("party tabs must not use radio name 'parties-tabs'")
		}

		if strings.Contains(tablistContent, `id="party-events-tab" role="tab" class="tab whitespace-nowrap font-medium" aria-label="Events" aria-controls="party-events-panel" checked`) {
			t.Errorf("events tab radio should not be checked")
		}

		// Check that each tab-content section immediately follows its radio input
		membersRadioIdx := strings.Index(tablistContent, `id="party-members-tab"`)
		membersPanelIdx := strings.Index(tablistContent, `id="party-members-panel"`)
		eventsRadioIdx := strings.Index(tablistContent, `id="party-events-tab"`)
		eventsPanelIdx := strings.Index(tablistContent, `id="party-events-panel"`)

		if membersRadioIdx == -1 || membersPanelIdx == -1 || eventsRadioIdx == -1 || eventsPanelIdx == -1 {
			t.Fatalf("missing radio or panel elements in tablist")
		}
		if !(membersRadioIdx < membersPanelIdx && membersPanelIdx < eventsRadioIdx && eventsRadioIdx < eventsPanelIdx) {
			t.Fatalf("ordering mismatch: expected members radio < members panel < events radio < events panel")
		}

		// Check members appear inside members panel
		membersPanel := extractElementByID(body, "party-members-panel", "section")
		if !strings.Contains(membersPanel, "LeaderPerson") || !strings.Contains(membersPanel, "OtherPerson") {
			t.Errorf("members panel missing member names: %s", membersPanel)
		}
	})

	// CLM-2: Content isolation between Members and Events panels across leader/non-leader x 1/0 events
	t.Run("content isolation (CLM-2)", func(t *testing.T) {
		oneEvent := []component.PartyEvent{{
			GameName: "Counter-Strike",
		}}

		cases := []struct {
			name     string
			isLeader bool
			events   []component.PartyEvent
		}{
			{name: "leader with one event", isLeader: true, events: oneEvent},
			{name: "leader with zero events", isLeader: true, events: nil},
			{name: "non-leader with one event", isLeader: false, events: oneEvent},
			{name: "non-leader with zero events", isLeader: false, events: nil},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				body := renderString(t, PartyDetail(player, "party-123", "Test Party", twoMembers, tc.isLeader, inviteCandidates, stepDownCandidates, games, nil, tc.events))

				membersPanel := extractElementByID(body, "party-members-panel", "section")
				eventsPanel := extractElementByID(body, "party-events-panel", "section")

				if membersPanel == "" {
					t.Fatalf("could not extract members panel")
				}
				if eventsPanel == "" {
					t.Fatalf("could not extract events panel")
				}

				// Events panel content
				if len(tc.events) > 0 {
					if !strings.Contains(eventsPanel, "Counter-Strike") {
						t.Errorf("events panel missing event game name")
					}
				} else {
					if !strings.Contains(eventsPanel, "No events scheduled") {
						t.Errorf("events panel missing 'No events scheduled' empty state")
					}
				}

				const createFormAction = `action="/parties/party-123/events"`
				if tc.isLeader {
					if !strings.Contains(eventsPanel, createFormAction) {
						t.Errorf("leader events panel missing create event form")
					}
				} else {
					if strings.Contains(eventsPanel, createFormAction) {
						t.Errorf("non-leader events panel contains create event form")
					}
				}

				// No member name in Events panel
				if strings.Contains(eventsPanel, "LeaderPerson") || strings.Contains(eventsPanel, "OtherPerson") {
					t.Errorf("events panel contains member names")
				}

				// No event content in Members panel
				if strings.Contains(membersPanel, "No events scheduled") || strings.Contains(membersPanel, "Counter-Strike") || strings.Contains(membersPanel, createFormAction) {
					t.Errorf("members panel contains event content")
				}
			})
		}
	})

	// CLM-4: Shared games, invite picker, step-down control, leave link outside tablist
	t.Run("elements outside tablist (CLM-4)", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			isLeader bool
		}{
			{name: "leader", isLeader: true},
			{name: "non-leader", isLeader: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				body := renderString(t, PartyDetail(player, "party-123", "Test Party", twoMembers, tc.isLeader, inviteCandidates, stepDownCandidates, games, nil, nil))

				const wantTablist = `<div role="tablist" class="tabs tabs-border">`
				tablistStart := strings.Index(body, wantTablist)
				if tablistStart == -1 {
					t.Fatalf("missing tablist")
				}
				sharedGamesStart := strings.Index(body[tablistStart:], `<section aria-label="Shared games"`)
				if sharedGamesStart == -1 {
					t.Fatalf("missing shared games section after tablist")
				}
				tablistSpan := body[tablistStart : tablistStart+sharedGamesStart]

				// Shared games section must be present and outside tablist
				const sharedGamesAttr = `aria-label="Shared games"`
				if !strings.Contains(body, sharedGamesAttr) {
					t.Fatalf("missing shared games section in page")
				}
				if strings.Contains(tablistSpan, sharedGamesAttr) {
					t.Errorf("shared games section must not be inside tablist")
				}

				// Leave link must be present and outside tablist
				const leaveLink = `href="/parties/party-123/leave"`
				if !strings.Contains(body, leaveLink) {
					t.Fatalf("missing leave link in page")
				}
				if strings.Contains(tablistSpan, leaveLink) {
					t.Errorf("leave link must not be inside tablist")
				}

				// Invite and step-down controls for leader
				const inviteAction = `action="/parties/party-123/invites/send"`
				const stepDownAction = `action="/parties/party-123/step-down"`
				if tc.isLeader {
					if !strings.Contains(body, inviteAction) {
						t.Errorf("leader page missing invite action")
					}
					if strings.Contains(tablistSpan, inviteAction) {
						t.Errorf("invite form must not be inside tablist")
					}
					if !strings.Contains(body, stepDownAction) {
						t.Errorf("leader page missing step-down action")
					}
					if strings.Contains(tablistSpan, stepDownAction) {
						t.Errorf("step-down form must not be inside tablist")
					}
				}
			})
		}
	})
}
