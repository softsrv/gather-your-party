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

func TestNavbarProfileMenuItem(t *testing.T) {
	render := func(nav NavState) string {
		var buf bytes.Buffer
		if err := Navbar(nav).Render(context.Background(), &buf); err != nil {
			t.Fatalf("render failed: %v", err)
		}
		return buf.String()
	}

	signedIn := render(NavState{SignedIn: true})
	wantProfileLink := `<a href="/profile">Profile</a>`
	if !strings.Contains(signedIn, wantProfileLink) {
		t.Errorf("signed-in navbar missing %q: %s", wantProfileLink, signedIn)
	}

	wantLogoutForm := `<form action="/auth/steam/logout" method="post"><button type="submit">log out</button></form>`
	if !strings.Contains(signedIn, wantLogoutForm) {
		t.Errorf("signed-in navbar missing logout form markup %q: %s", wantLogoutForm, signedIn)
	}

	// Assert that Profile link is inside dropdown-content ul before the logout form
	dropdownIdx := strings.Index(signedIn, `class="menu dropdown-content`)
	if dropdownIdx == -1 {
		t.Fatalf("signed-in navbar missing dropdown-content: %s", signedIn)
	}
	profileIdx := strings.Index(signedIn[dropdownIdx:], wantProfileLink)
	if profileIdx == -1 {
		t.Errorf("profile link not inside dropdown-content: %s", signedIn)
	}
	logoutIdx := strings.Index(signedIn[dropdownIdx:], wantLogoutForm)
	if logoutIdx == -1 {
		t.Errorf("logout form not inside dropdown-content: %s", signedIn)
	}
	if profileIdx >= logoutIdx {
		t.Errorf("expected profile link before logout form in dropdown-content: %s", signedIn)
	}

	signedOut := render(NavState{SignedIn: false})
	if strings.Contains(signedOut, `href="/profile"`) {
		t.Errorf("signed-out navbar unexpectedly contains href=\"/profile\": %s", signedOut)
	}
}
