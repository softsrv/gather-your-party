package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CLM-13: party events can only be created. The router registers exactly one
// events route, POST /parties/{partyID}/events, and no store statement updates
// or deletes party_events rows.
func TestPartyEventsCreateOnly(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	var eventRoutes []string
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		if strings.Contains(m[1], "/events") {
			eventRoutes = append(eventRoutes, m[1])
		}
	}
	if len(eventRoutes) != 1 || eventRoutes[0] != "POST /parties/{partyID}/events" {
		t.Fatalf("events routes = %v, want only POST /parties/{partyID}/events", eventRoutes)
	}

	files, err := filepath.Glob(filepath.Join("internal", "db", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	mutation := regexp.MustCompile(`(?is)\b(UPDATE\s+party_events|DELETE\s+FROM\s+party_events)\b`)
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		code, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if loc := mutation.FindIndex(code); loc != nil {
			t.Errorf("%s mutates party_events: %q", file, code[loc[0]:loc[1]])
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no store files")
	}
	// The scan discriminates.
	if !mutation.MatchString("DELETE FROM party_events WHERE id = $1") || !mutation.MatchString("update party_events set") {
		t.Fatal("mutation pattern does not match a party_events mutation")
	}
}
