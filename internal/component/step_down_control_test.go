package component

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

const stepDownTestPartyID = "61daf402-9c06-4c3b-923f-5f7dfd4369c7"

// TestStepDownControlWithCandidates verifies the enabled state (CLM-10):
// with at least one eligible candidate, the control renders a real
// submitting action (hx-post/form action to the step-down endpoint)
// carrying the chosen target's users.id in a field named "target".
func TestStepDownControlWithCandidates(t *testing.T) {
	candidates := []StepDownCandidate{
		{UserID: 7, Name: "Alice"},
		{UserID: 8, Name: "Bob"},
	}

	var buf bytes.Buffer
	if err := StepDownControl(stepDownTestPartyID, candidates).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	body := buf.String()

	wantPath := "/parties/" + stepDownTestPartyID + "/step-down"
	if !strings.Contains(body, `hx-post="`+wantPath+`"`) {
		t.Errorf("expected hx-post targeting %q, got: %s", wantPath, body)
	}
	if !strings.Contains(body, wantPath) {
		t.Errorf("expected form action containing %q, got: %s", wantPath, body)
	}
	if !strings.Contains(body, `name="target" value="7"`) {
		t.Errorf("expected name=\"target\" carrying candidate userID 7, got: %s", body)
	}
	if !strings.Contains(body, `name="target" value="8"`) {
		t.Errorf("expected name=\"target\" carrying candidate userID 8, got: %s", body)
	}
	if !strings.Contains(body, "<form") {
		t.Errorf("expected a real <form> element, got: %s", body)
	}
}

// TestStepDownControlNoCandidates verifies the disabled state (CLM-11):
// when there is no eligible candidate (party of one), the control must
// render no submitting step-down action at all.
func TestStepDownControlNoCandidates(t *testing.T) {
	var buf bytes.Buffer
	if err := StepDownControl(stepDownTestPartyID, nil).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	body := buf.String()

	wantPath := "/parties/" + stepDownTestPartyID + "/step-down"
	if strings.Contains(body, wantPath) {
		t.Errorf("expected no reference to step-down endpoint %q, got: %s", wantPath, body)
	}
	if strings.Contains(body, "hx-post") {
		t.Errorf("expected no hx-post attribute, got: %s", body)
	}
	if strings.Contains(body, "<form") {
		t.Errorf("expected no <form> element, got: %s", body)
	}
	if strings.Contains(body, `name="target"`) {
		t.Errorf("expected no name=\"target\" field, got: %s", body)
	}
}
