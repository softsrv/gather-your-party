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
	for _, unwanted := range []string{"loading-spinner", "Syncing your library", "failed"} {
		if strings.Contains(ok, unwanted) {
			t.Errorf("success fragment contains %q: %s", unwanted, ok)
		}
	}

	failed := render(true, "steam down")
	if !strings.Contains(failed, `id="library-sync"`) || !strings.Contains(failed, "Library sync failed: steam down") {
		t.Errorf("failure fragment missing id or message: %s", failed)
	}
	if strings.Contains(failed, "loading-spinner") {
		t.Errorf("failure fragment contains the spinner: %s", failed)
	}
}
