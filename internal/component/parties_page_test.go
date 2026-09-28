package component_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gather-your-party/internal/component"
	"gather-your-party/internal/template"
)

// CLM-2/6/7/9: render the full page through its public templ surface.
func TestPartiesPage(t *testing.T) {
	var buf bytes.Buffer
	parties := []component.UserParty{{PartyID: "party-a", Name: "A & friends"}, {PartyID: "party-b", Name: "B <crew>"}}
	invites := []component.UserPendingInvite{{PartyID: "invite-a", Name: "Invite A"}, {PartyID: "invite-b", Name: "Invite B"}}
	if err := template.PartiesPage(parties, invites).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, want := range []string{
		`<title>Parties</title>`, `<script src="/static/script/htmx.min.js"`,
		`<div role="tablist" class="tabs tabs-lifted">`,
		`type="radio" name="parties-tabs" id="current-parties-tab" role="tab"`,
		`aria-label="Current parties" aria-controls="current-parties-panel" checked`,
		`type="radio" name="parties-tabs" id="invites-tab" role="tab"`,
		`aria-label="Invites" aria-controls="invites-panel"`,
		`id="current-parties-panel" role="tabpanel" aria-labelledby="current-parties-tab" class="tab-content`,
		`id="invites-panel" role="tabpanel" aria-labelledby="invites-tab" class="tab-content`,
		`<a href="/parties/party-a">A &amp; friends</a>`,
		`<a href="/parties/party-b">B &lt;crew&gt;</a>`,
		`<form method="post" action="/parties" hx-post="/parties">`,
		`<label for="party-name">Party name</label>`,
		`<input type="text" id="party-name" name="name"`,
		`<button type="submit" class="btn btn-primary">Create party</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q: %s", want, body)
		}
	}
	panels := strings.Split(body, `</section>`)
	if len(panels) != 3 {
		t.Fatalf("expected two distinct panels: %s", body)
	}
	if strings.Count(panels[0], "<li>") != 2 || strings.Contains(panels[0], "Invite A") {
		t.Fatal("current parties in wrong panel")
	}
	for _, invite := range invites {
		start := strings.Index(panels[1], "<span>"+invite.Name+"</span>")
		if start < 0 {
			t.Fatalf("missing invite %q", invite.Name)
		}
		entry := strings.SplitN(panels[1][start:], "</li>", 2)[0]
		for _, action := range []string{"accept", "reject"} {
			path := "/parties/" + invite.PartyID + "/invites/" + action
			if !strings.Contains(entry, `<form method="post" action="`+path+`" hx-post="`+path+`"`) {
				t.Errorf("invite %q missing %s form", invite.Name, action)
			}
		}
		if strings.Count(entry, `type="submit"`) != 2 {
			t.Errorf("invite %q must have both submit buttons", invite.Name)
		}
	}
	if strings.Count(body, `role="tab"`) != 2 || strings.Count(body, `role="tabpanel"`) != 2 || strings.Count(body, " checked") != 1 || strings.Contains(body, `type="button"`) {
		t.Fatal("tabs or submitting controls have unexpected structure")
	}
}

func TestPartiesPageEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := template.PartiesPage(nil, nil).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if strings.Count(body, `role="tabpanel"`) != 2 || strings.Count(body, "<form ") != 1 || strings.Contains(body, "<li>") || strings.Contains(body, "<li ") || strings.Contains(body, "/invites/") || strings.Contains(body, `href="/parties/`) {
		t.Fatalf("empty tabs must have no entries or invite controls, but retain create form: %s", body)
	}
}
