package template

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestLeaveConfirmation(t *testing.T) {
	var output bytes.Buffer
	if err := LeaveConfirmation("party-123").Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		"Are you sure you want to leave this party?",
		`hx-post="/parties/party-123/leave"`,
		`<dialog`, `showModal()`, `>Confirm</button>`, `>Cancel</button>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("confirmation missing %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{"delete", "deletion", "last member"} {
		if strings.Contains(strings.ToLower(html), forbidden) {
			t.Errorf("confirmation contains %q", forbidden)
		}
	}
	// The sole request is inside the dialog. Opening it and cancelling have no
	// htmx request attribute; cancellation only closes the dialog.
	if strings.Count(html, "hx-post=") != 1 || strings.Contains(html[:strings.Index(html, "<dialog")], "hx-post") {
		t.Fatal("leave can be submitted outside the confirmation")
	}
	cancelEnd := strings.Index(html, ">Cancel</button>")
	cancelStart := strings.LastIndex(html[:cancelEnd], "<button")
	cancel := html[cancelStart:cancelEnd]
	if !strings.Contains(cancel, `type="button"`) || !strings.Contains(cancel, "close()") || strings.Contains(cancel, "hx-") {
		t.Fatalf("cancel must only close the dialog: %s", cancel)
	}
}

func TestStepDownControl(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3} {
		members := []PartyMember{{UserID: 17, Name: "Senior"}, {UserID: 23, Name: "Junior"}, {UserID: 31, Name: "Other"}}[:count]
		var output bytes.Buffer
		if err := StepDownControl("party-123", members).Render(context.Background(), &output); err != nil {
			t.Fatal(err)
		}
		html := output.String()
		if count <= 1 {
			if !strings.Contains(html, "disabled") || strings.Contains(html, "hx-post") || strings.Contains(html, "<form") {
				t.Fatalf("size %d offers a step down: %s", count, html)
			}
		} else {
			if strings.Contains(html, "disabled") || !strings.Contains(html, `hx-post="/parties/party-123/step-down"`) || !strings.Contains(html, `name="targetUserID"`) || !strings.Contains(html, `value="23"`) {
				t.Fatalf("size %d must allow a chosen member: %s", count, html)
			}
		}
	}
}
