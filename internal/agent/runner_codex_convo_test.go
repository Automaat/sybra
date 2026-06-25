package agent

import (
	"strings"
	"testing"
)

func TestBuildCodexConvoArgs_ReasoningEffort(t *testing.T) {
	t.Parallel()

	t.Run("flag_present_when_set", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex", ReasoningEffort: "xhigh"}
		args := buildCodexConvoArgs(a, RunConfig{}, "hello")
		found := false
		for i := range len(args) - 1 {
			if args[i] == "-c" && args[i+1] == "model_reasoning_effort=xhigh" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected -c model_reasoning_effort=xhigh in args; got %v", args)
		}
	})

	t.Run("flag_absent_when_empty", func(t *testing.T) {
		a := &Agent{ID: "a", Provider: "codex", ReasoningEffort: ""}
		args := buildCodexConvoArgs(a, RunConfig{}, "hello")
		for _, arg := range args {
			if strings.Contains(arg, "model_reasoning_effort=") {
				t.Errorf("model_reasoning_effort must be absent when empty; got %v", args)
			}
		}
	})
}

func TestCodexReasoningArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		effort string
		want   []string
	}{
		{"", nil},
		{"low", []string{"-c", "model_reasoning_effort=low"}},
		{"medium", []string{"-c", "model_reasoning_effort=medium"}},
		{"high", []string{"-c", "model_reasoning_effort=high"}},
		{"xhigh", []string{"-c", "model_reasoning_effort=xhigh"}},
	}
	for _, tc := range cases {
		t.Run(tc.effort, func(t *testing.T) {
			got := codexReasoningArgs(tc.effort)
			if len(got) != len(tc.want) {
				t.Fatalf("codexReasoningArgs(%q) = %v, want %v", tc.effort, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("codexReasoningArgs(%q)[%d] = %q, want %q", tc.effort, i, got[i], tc.want[i])
				}
			}
		})
	}
}
