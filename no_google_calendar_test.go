package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenGoogleCalendarTokens name the removed Google Calendar feature and the
// Profile page. There is deliberately no bare "googleapis.com" token: the Google
// Fonts links (fonts.googleapis.com) predate the feature and stay.
var forbiddenGoogleCalendarTokens = []string{
	"accounts.google.com",
	"oauth2.googleapis.com",
	"oauth2.googleapis.com/revoke",
	"googleapis.com/auth/calendar",
	"www.googleapis.com/auth",
	"googleapis.com/calendar/",
	"GOOGLE_CLIENT_ID",
	"GOOGLE_CLIENT_SECRET",
	"google_calendar",
	"GoogleCalendar",
	"CalendarState",
	"ProfilePage",
	"handleProfile",
	`"/profile`,
	` /profile"`,
}

// No non-test .go or .templ file in the module (generated *_templ.go included)
// refers to the removed feature, so no code path can send anything to Google.
func TestNoGoogleCalendarReferences(t *testing.T) {
	scanned := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") || (!strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".templ")) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, token := range forbiddenGoogleCalendarTokens {
			if strings.Contains(string(raw), token) {
				t.Errorf("%s refers to the removed Google Calendar feature: %q", path, token)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("scanned no source files")
	}

	page, err := os.ReadFile(filepath.Join("internal", "template", "template.templ"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "fonts.googleapis.com") {
		t.Error("template.templ lost the Google Fonts links (fonts.googleapis.com)")
	}
}
