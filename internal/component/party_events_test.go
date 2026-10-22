package component

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/softsrv/steamapi/steamapi"
)

func renderPartyEvents(t *testing.T, events []PartyEvent, isLeader, solo bool, games []SharedGameCount, messages []string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := PartyEvents("party-1", events, isLeader, solo, games, messages).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// eventForm returns the create-event form markup, or "" when there is none.
func eventForm(body string) string {
	start := strings.Index(body, `<form method="post" action="/parties/party-1/events"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</form>")
	if end < 0 {
		return body[start:]
	}
	return body[start : start+end]
}

// enabledSubmit matches a submit button that carries no disabled attribute.
var submitButton = regexp.MustCompile(`<button[^>]*type="submit"[^>]*>`)

func hasEnabledSubmit(form string) bool {
	for _, button := range submitButton.FindAllString(form, -1) {
		if !strings.Contains(button, " disabled") {
			return true
		}
	}
	return false
}

// CLM-7: with no games to pick, the form names the reason, offers no game
// option and no enabled submit; with shared games it offers both.
func TestPartyEventFormWithoutGamesShowsReason(t *testing.T) {
	notSynced := "Shared games cannot be computed."
	private := "no games found for user Gordon. Their list may be private"
	unavailable := "We could not load shared games right now. Please try again in a moment."
	for _, tc := range []struct {
		name     string
		solo     bool
		messages []string
		want     []string
	}{
		{name: "solo party", solo: true, want: []string{NoEventGamesSoloMessage}},
		{name: "unsynced or private member", messages: []string{notSynced, private}, want: []string{NoEventGamesMembersMessage}},
		{name: "shared games could not be loaded", messages: []string{unavailable}, want: []string{unavailable}},
		{name: "no multiplayer games in common", want: []string{NoEventGamesInCommonMessage}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := eventForm(renderPartyEvents(t, nil, true, tc.solo, nil, tc.messages))
			if form == "" {
				t.Fatal("leader page has no create-event form")
			}
			for _, want := range tc.want {
				if !strings.Contains(form, want) {
					t.Errorf("form lacks reason %q: %s", want, form)
				}
			}
			if strings.Contains(form, "<option") || strings.Contains(form, "<select") {
				t.Errorf("form offers a game picker without games: %s", form)
			}
			if hasEnabledSubmit(form) {
				t.Errorf("form offers an enabled submit without games: %s", form)
			}
		})
	}

	t.Run("shared games offer a picker and submit", func(t *testing.T) {
		games := []SharedGameCount{{Game: steamapi.Game{AppID: 10, Name: "Common Alpha"}}, {Game: steamapi.Game{AppID: 20, Name: "Common Beta"}}}
		form := eventForm(renderPartyEvents(t, nil, true, false, games, nil))
		for _, want := range []string{
			`<select name="game_app_id"`, `<option value="10">Common Alpha</option>`, `<option value="20">Common Beta</option>`,
			`name="date"`, `name="start_time"`, `name="end_time"`, `name="utc_offset"`,
		} {
			if !strings.Contains(form, want) {
				t.Errorf("form lacks %q: %s", want, form)
			}
		}
		if !hasEnabledSubmit(form) {
			t.Errorf("form with games has no enabled submit: %s", form)
		}
		for _, reason := range []string{NoEventGamesSoloMessage, NoEventGamesInCommonMessage} {
			if strings.Contains(form, reason) {
				t.Errorf("form with games shows reason %q", reason)
			}
		}
	})
}

// CLM-8: an empty events area says "No events scheduled" for every member, and
// only the leader gets the create form.
func TestPartyEventsEmptyState(t *testing.T) {
	games := []SharedGameCount{{Game: steamapi.Game{AppID: 10, Name: "Common Alpha"}}}
	for _, tc := range []struct {
		name     string
		isLeader bool
	}{{name: "leader", isLeader: true}, {name: "non-leader"}} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderPartyEvents(t, nil, tc.isLeader, false, games, nil)
			if !strings.Contains(body, "No events scheduled") {
				t.Errorf("missing empty state: %s", body)
			}
			if (eventForm(body) != "") != tc.isLeader {
				t.Errorf("form present = %v, want %v: %s", eventForm(body) != "", tc.isLeader, body)
			}
		})
	}
	body := renderPartyEvents(t, []PartyEvent{{GameName: "Common Alpha", StartAt: time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC), EndAt: time.Date(2030, 1, 2, 5, 0, 0, 0, time.UTC)}}, false, false, games, nil)
	if strings.Contains(body, "No events scheduled") || !strings.Contains(body, "Common Alpha") {
		t.Errorf("listed event rendered as empty: %s", body)
	}
}

// CLM-11 (unit part): each event's start and end carry the instant in RFC3339
// UTC, whatever zone the time value arrives in, with UTC fallback text, and the
// page ships the script that localizes them and fills the offset.
func TestPartyEventsTimesAreMachineReadableUTC(t *testing.T) {
	east := time.FixedZone("+05:30", 5*3600+30*60)
	events := []PartyEvent{
		{GameName: "Common Alpha", StartAt: time.Date(2030, 1, 2, 20, 0, 0, 0, east), EndAt: time.Date(2030, 1, 3, 6, 0, 0, 0, east)},
		{GameName: "Common Beta", StartAt: time.Date(2030, 6, 1, 23, 30, 0, 0, time.UTC), EndAt: time.Date(2030, 6, 2, 1, 0, 0, 0, time.UTC)},
	}
	body := renderPartyEvents(t, events, true, false, []SharedGameCount{{Game: steamapi.Game{AppID: 10, Name: "Common Alpha"}}}, nil)
	for _, want := range []string{
		`<time datetime="2030-01-02T14:30:00Z" data-local-time="datetime">Wed, 2 Jan 2030 14:30 UTC</time>`,
		`<time datetime="2030-01-03T00:30:00Z" data-local-time="datetime">Thu, 3 Jan 2030 00:30 UTC</time>`,
		`<time datetime="2030-06-01T23:30:00Z"`,
		`<time datetime="2030-06-02T01:00:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q: %s", want, body)
		}
	}
	if n := len(regexp.MustCompile(`<time datetime="\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ"`).FindAllString(body, -1)); n != 4 {
		t.Errorf("want 4 UTC <time datetime> elements, got %d: %s", n, body)
	}
	for _, want := range []string{"<script>", "toLocaleString", `time[data-local-time]`, "getTimezoneOffset", `input[data-utc-offset]`, "DOMContentLoaded"} {
		if !strings.Contains(body, want) {
			t.Errorf("localization script lacks %q", want)
		}
	}
}
