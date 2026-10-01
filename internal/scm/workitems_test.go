package scm

import (
	"reflect"
	"testing"
)

func TestExtractWorkItems(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		texts []string
		want  []string
	}{
		{"intent and branch", []string{"Implement AB#420295, supports AB#420193.", "refs/heads/AB#420295-detail"}, []string{"420295", "420193"}},
		{"branch alone", []string{"ordinary intent", "feature/ab#420295"}, []string{"420295"}},
		{"invalid and incidental", []string{"AB#0 AB#-1 AB#12x CAB#12 420295"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ExtractWorkItems(test.texts...); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("references = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWorkItemTitle(t *testing.T) {
	t.Parallel()
	for _, title := range []string{"fix: typed response", "fix: AB#420295 typed response", "fix: typed response (AB#420295)", "fix: typed response (AB#420295, AB#420193)"} {
		got := WorkItemTitle(title, []string{"420295"})
		if got != "fix: typed response (AB#420295)" || WorkItemTitle(got, []string{"420295"}) != got {
			t.Fatalf("title %q -> %q", title, got)
		}
	}
}
