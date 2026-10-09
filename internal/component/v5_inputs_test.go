package component_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gather-your-party/internal/component"

	"github.com/a-h/templ"
)

func TestInputsUseV5DefaultBorder(t *testing.T) {
	for _, tt := range []struct {
		name string
		view templ.Component
		want string
	}{
		{"create party", component.CreatePartyForm(), `class="input w-full bg-base-200"`},
		{"search", component.SearchInput("Search friends", "#friends-list-form li"), `class="input input-sm flex w-full items-center gap-2 bg-base-200 sm:w-64"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tt.view.Render(context.Background(), &buf); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("input missing v5 classes %q: %s", tt.want, buf.String())
			}
		})
	}
}
