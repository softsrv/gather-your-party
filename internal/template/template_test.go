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
			comp := PartyDetail(player, "party-123", "Test Party", soloMembers, true, nil, nil, tc.sharedGames, tc.gamesMessages)
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
		comp := PartyDetail(player, "party-123", "Test Party", twoMembers, true, nil, nil, nil, nil)
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
		comp := PartyDetail(player, "party-123", "Test Party", twoMembers, true, nil, nil, nil, []string{"Shared games cannot be computed."})
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
		{"ProfilePage", renderString(t, ProfilePage(player, component.CalendarState{}))},
		{"PartyDetail", renderString(t, PartyDetail(player, "party-1", "Party", members, true, nil, nil, nil, nil))},
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

// CLM-1: ProfilePage renders inside the shared Page chrome (navbar with the
// account menu and the shared footer), has a profile header section
// (id="profile-header") showing the player's name and full-size avatar, and
// contains a Calendars section with a stable identifier (id="calendars") and a
// 'Calendars' heading where calendar connections are listed.
func TestProfilePage(t *testing.T) {
	player := steamapi.Player{
		SteamID:      "76561198000000001",
		PersonaName:  "TestUser",
		AvatarMedium: "https://example.com/avatar_medium.jpg",
		AvatarFull:   "https://example.com/avatar_full.jpg",
	}

	out := renderString(t, ProfilePage(player, component.CalendarState{GoogleAvailable: true}))

	// Page footer ('© 2026 softsrv')
	if !strings.Contains(out, "© 2026 softsrv") {
		t.Fatalf("missing Page footer in ProfilePage output: %s", out)
	}

	// Navbar (contains <nav and the navbar account menu rendering AvatarMedium)
	if !strings.Contains(out, "<nav") {
		t.Fatalf("missing navbar in ProfilePage output: %s", out)
	}
	if !strings.Contains(out, player.AvatarMedium) {
		t.Fatalf("navbar should render AvatarMedium: %s", out)
	}

	// id="profile-header" element contains PersonaName and AvatarFull URL
	profileHeaderRe := regexp.MustCompile(`(?s)<section[^>]*id="profile-header"[^>]*>(.*?)</section>`)
	headerMatch := profileHeaderRe.FindStringSubmatch(out)
	if headerMatch == nil {
		t.Fatalf("missing id=\"profile-header\" section in ProfilePage: %s", out)
	}
	headerHTML := headerMatch[1]
	if !strings.Contains(headerHTML, player.PersonaName) {
		t.Fatalf("profile-header section does not contain player PersonaName %q: %s", player.PersonaName, headerHTML)
	}
	if !strings.Contains(headerHTML, player.AvatarFull) {
		t.Fatalf("profile-header section does not contain player AvatarFull %q: %s", player.AvatarFull, headerHTML)
	}

	// element id="calendars" contains 'Calendars' heading
	calendarsRe := regexp.MustCompile(`(?s)<section[^>]*id="calendars"[^>]*>(.*?)</section>`)
	calendarsMatch := calendarsRe.FindStringSubmatch(out)
	if calendarsMatch == nil {
		t.Fatalf("missing id=\"calendars\" section in ProfilePage: %s", out)
	}
	calHTML := calendarsMatch[1]
	if !strings.Contains(calHTML, "Calendars") {
		t.Fatalf("calendars section does not contain 'Calendars' heading: %s", calHTML)
	}
}

func calendarsSection(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<section[^>]*id="calendars"[^>]*>(.*?)</section>`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("missing id=\"calendars\" section: %s", out)
	}
	return m[1]
}

func profileCalendarPlayer() steamapi.Player {
	return steamapi.Player{SteamID: "76561198000000001", PersonaName: "TestUser", AvatarFull: "https://example.com/a.jpg", AvatarMedium: "https://example.com/m.jpg"}
}

// CLM-1: with Google available and no connection, the section reads 'not
// connected' with a connect control and shows no error.
func TestProfilePageCalendarNotConnected(t *testing.T) {
	out := renderString(t, ProfilePage(profileCalendarPlayer(), component.CalendarState{GoogleAvailable: true}))
	cal := calendarsSection(t, out)
	if !strings.Contains(cal, "not connected") {
		t.Fatalf("want 'not connected': %s", cal)
	}
	if !strings.Contains(cal, `href="`+component.GoogleCalendarConnectPath+`"`) {
		t.Fatalf("want connect control targeting %s: %s", component.GoogleCalendarConnectPath, cal)
	}
	if strings.Contains(strings.ToLower(out), "couldn't connect") || strings.Contains(out, "role=\"alert\"") {
		t.Fatalf("no error expected: %s", cal)
	}
}

// CLM-2: unavailable beats not-connected; connected beats unavailable.
func TestProfilePageCalendarPrecedence(t *testing.T) {
	player := profileCalendarPlayer()

	unavailable := renderString(t, ProfilePage(player, component.CalendarState{}))
	cal := calendarsSection(t, unavailable)
	if !strings.Contains(cal, "unavailable") {
		t.Fatalf("want 'unavailable': %s", cal)
	}
	if strings.Contains(unavailable, component.GoogleCalendarConnectPath) || strings.Contains(unavailable, "accounts.google.com") || strings.Contains(cal, "not connected") {
		t.Fatalf("unavailable must offer no connect control or google link: %s", cal)
	}

	// positive control
	available := calendarsSection(t, renderString(t, ProfilePage(player, component.CalendarState{GoogleAvailable: true})))
	if !strings.Contains(available, "not connected") || strings.Contains(available, "unavailable") {
		t.Fatalf("available must flip to not-connected: %s", available)
	}

	connected := calendarsSection(t, renderString(t, ProfilePage(player, component.CalendarState{Connected: true, Email: "me@example.com"})))
	if !strings.Contains(connected, "connected") || strings.Contains(connected, "not connected") {
		t.Fatalf("want connected: %s", connected)
	}
	if !strings.Contains(connected, "me@example.com") {
		t.Fatalf("want email: %s", connected)
	}
	if !strings.Contains(connected, `action="`+component.GoogleCalendarDisconnectPath+`"`) || !strings.Contains(connected, `method="post"`) {
		t.Fatalf("want disconnect POST form: %s", connected)
	}
	if strings.Contains(connected, "unavailable") {
		t.Fatalf("connected must not say unavailable: %s", connected)
	}
}

// CLM-3: the failed flag adds fixed copy beside whichever state applies.
func TestProfilePageCalendarConnectFailed(t *testing.T) {
	player := profileCalendarPlayer()
	states := []struct {
		name  string
		state component.CalendarState
		want  string
	}{
		{"not connected", component.CalendarState{GoogleAvailable: true}, "not connected"},
		{"connected", component.CalendarState{GoogleAvailable: true, Connected: true, Email: "me@example.com"}, "me@example.com"},
		{"unavailable", component.CalendarState{}, "unavailable"},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			failed := tc.state
			failed.ConnectFailed = true
			cal := calendarsSection(t, renderString(t, ProfilePage(player, failed)))
			if !strings.Contains(cal, "Couldn't connect") {
				t.Fatalf("want couldn't-connect message: %s", cal)
			}
			if !strings.Contains(cal, tc.want) {
				t.Fatalf("want state render %q alongside failure: %s", tc.want, cal)
			}
			if tc.name == "connected" && !strings.Contains(cal, component.GoogleCalendarDisconnectPath) {
				t.Fatalf("connected still shows disconnect control: %s", cal)
			}
			ok := calendarsSection(t, renderString(t, ProfilePage(player, tc.state)))
			if strings.Contains(ok, "Couldn't connect") {
				t.Fatalf("message must be absent when flag unset: %s", ok)
			}
		})
	}
}

// CLM-4: no Apple calendar option in connected or unconnected states.
func TestProfilePageNoAppleOption(t *testing.T) {
	player := profileCalendarPlayer()
	if strings.Contains(strings.ToLower(player.PersonaName), "apple") {
		t.Fatal("sample persona must not contain 'apple'")
	}
	for name, state := range map[string]component.CalendarState{
		"connected":   {GoogleAvailable: true, Connected: true, Email: "me@example.com"},
		"unconnected": {GoogleAvailable: true},
		"unavailable": {},
		"failed":      {GoogleAvailable: true, ConnectFailed: true},
	} {
		t.Run(name, func(t *testing.T) {
			out := renderString(t, ProfilePage(player, state))
			if strings.Contains(strings.ToLower(calendarsSection(t, out)), "apple") {
				t.Fatalf("calendars section mentions apple: %s", out)
			}
			if regexp.MustCompile(`(?i)href="[^"]*apple[^"]*"`).MatchString(out) {
				t.Fatalf("apple href in body: %s", out)
			}
		})
	}
}
