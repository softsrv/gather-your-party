package component

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

// CLM-2/3: the picker submits exactly the supplied candidate set.
func TestInvitePickerWithCandidates(t *testing.T) {
	var buf bytes.Buffer
	if err := InvitePicker(stepDownTestPartyID, []InviteCandidate{{UserID: 7, Name: "Alice"}, {UserID: 891, Name: "<Bob>"}}).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	path := "/parties/" + stepDownTestPartyID + "/invites/send"
	for _, want := range []string{`<form method="post"`, `action="` + path + `"`, `hx-post="` + path + `"`, `type="submit" name="target" value="7"`, `type="submit" name="target" value="891"`, "Alice", "&lt;Bob&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Count(body, `name="target"`) != 2 || strings.Count(body, "<button") != 2 || strings.Contains(body, `value="42"`) {
		t.Fatalf("picker must render only the two supplied candidates: %s", body)
	}
}

func TestInvitePickerNoCandidates(t *testing.T) {
	var buf bytes.Buffer
	if err := InvitePicker(stepDownTestPartyID, nil).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"<form", "<button", "hx-post", "/invites/send", `name="target"`} {
		if strings.Contains(buf.String(), forbidden) {
			t.Errorf("empty picker contains %q", forbidden)
		}
	}
}

func TestInviteReplyControlsSubmit(t *testing.T) {
	for _, tc := range []struct {
		action  string
		control templ.Component
	}{
		{"accept", AcceptInviteControl(stepDownTestPartyID)},
		{"reject", RejectInviteControl(stepDownTestPartyID)},
	} {
		t.Run(tc.action, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.control.Render(context.Background(), &buf); err != nil {
				t.Fatal(err)
			}
			path := "/parties/" + stepDownTestPartyID + "/invites/" + tc.action
			for _, want := range []string{`<form method="post"`, `action="` + path + `"`, `hx-post="` + path + `"`, `<button type="submit"`} {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("missing %q in %s", want, buf.String())
				}
			}
		})
	}
}
