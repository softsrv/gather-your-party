package component

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestNavbarLibrarySyncElement(t *testing.T) {
	render := func(nav NavState) string {
		var buf bytes.Buffer
		if err := Navbar(nav).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render failed: %v", err)
		}
		return buf.String()
	}

	due := render(NavState{SignedIn: true, LibrarySyncDue: true})
	for _, want := range []string{
		"Syncing your library", "loading-spinner", `hx-trigger="load"`,
		`hx-get="/library/sync"`, `id="library-sync"`, `hx-swap="outerHTML"`,
	} {
		if !strings.Contains(due, want) {
			t.Errorf("sync-due navbar missing %q: %s", want, due)
		}
	}
	if !strings.Contains(due, `class="user-menu dropdown dropdown-end ml-auto"`) {
		t.Errorf("user-menu missing ml-auto: %s", due)
	}
	if strings.Contains(due, `id="library-sync" hx-get="/library/sync" hx-trigger="load" hx-swap="outerHTML" class="ml-auto`) ||
		strings.Contains(due, `class="ml-auto flex items-center gap-2 text-xs text-base-content/60"`) {
		t.Errorf("library-sync spinner unexpectedly carries ml-auto: %s", due)
	}
	if count := strings.Count(due, "ml-auto"); count != 1 {
		t.Errorf("expected exactly 1 ml-auto in sync-due navbar, got %d: %s", count, due)
	}

	for name, nav := range map[string]NavState{
		"signed in, not due": {SignedIn: true},
		"signed out, due":    {LibrarySyncDue: true},
	} {
		body := render(nav)
		for _, unwanted := range []string{"Syncing your library", "loading-spinner", `hx-trigger="load"`, "/library/sync"} {
			if strings.Contains(body, unwanted) {
				t.Errorf("%s: navbar unexpectedly contains %q", name, unwanted)
			}
		}
	}
}

func TestLibrarySyncStatus(t *testing.T) {
	render := func(failed bool, reason string) string {
		var buf bytes.Buffer
		if err := LibrarySyncStatus(failed, reason).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render failed: %v", err)
		}
		return buf.String()
	}

	ok := render(false, "")
	if !strings.Contains(ok, `id="library-sync"`) {
		t.Errorf("success fragment lost the swap target id: %s", ok)
	}
	if strings.Contains(ok, "ml-auto") {
		t.Errorf("success fragment contains ml-auto: %s", ok)
	}
	for _, unwanted := range []string{"loading-spinner", "Syncing your library", "failed"} {
		if strings.Contains(ok, unwanted) {
			t.Errorf("success fragment contains %q: %s", unwanted, ok)
		}
	}

	failed := render(true, "steam down")
	if !strings.Contains(failed, `id="library-sync"`) || !strings.Contains(failed, "Library sync failed: steam down") {
		t.Errorf("failure fragment missing id or message: %s", failed)
	}
	if strings.Contains(failed, "ml-auto") {
		t.Errorf("failure fragment contains ml-auto: %s", failed)
	}
	if strings.Contains(failed, "loading-spinner") {
		t.Errorf("failure fragment contains the spinner: %s", failed)
	}
}

// The signed-in account menu has no Profile entry; the parties link and the
// sign-out form still render as they did before the calendar change.
func TestNavbarAccountMenuHasNoProfileLink(t *testing.T) {
	var buf bytes.Buffer
	if err := Navbar(NavState{SignedIn: true}).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	signedIn := buf.String()

	dropdownAt := strings.Index(signedIn, `class="menu dropdown-content`)
	if dropdownAt == -1 {
		t.Fatalf("signed-in navbar missing account menu: %s", signedIn)
	}
	menu := signedIn[dropdownAt:]
	if end := strings.Index(menu, "</ul>"); end != -1 {
		menu = menu[:end]
	}

	if strings.Contains(signedIn, "/profile") {
		t.Errorf("signed-in navbar still links to /profile: %s", signedIn)
	}
	if strings.Contains(signedIn, ">Profile<") {
		t.Errorf("signed-in navbar still has a Profile label: %s", signedIn)
	}

	wantLogoutForm := `<li role="none"><form action="/auth/steam/logout" method="post"><button type="submit">log out</button></form></li>`
	if !strings.Contains(menu, wantLogoutForm) {
		t.Errorf("account menu missing sign-out form %q: %s", wantLogoutForm, menu)
	}
	if n := strings.Count(menu, "<li"); n != 1 {
		t.Errorf("account menu should hold only the sign-out entry, got %d entries: %s", n, menu)
	}

	wantPartiesLink := `<a href="/parties" class="`
	if !strings.Contains(signedIn, wantPartiesLink) || !strings.Contains(signedIn, "Parties</a>") {
		t.Errorf("signed-in navbar missing the parties link: %s", signedIn)
	}
}
