package agent

import (
	"slices"
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

// TestBuildCodexConvoArgs_HooksPresent verifies that hook overrides and
// --dangerously-bypass-hook-trust are injected into per-turn convo args when
// sybra-cli is on PATH and TaskID is set.
func TestBuildCodexConvoArgs_HooksPresent(t *testing.T) {
	makeFakeSybraCLI(t)

	a := &Agent{ID: "a", Provider: "codex", TaskID: "task-abc123"}
	args := buildCodexConvoArgs(a, RunConfig{}, "hello")

	if !slices.Contains(args, "--dangerously-bypass-hook-trust") {
		t.Errorf("--dangerously-bypass-hook-trust missing; args=%v", args)
	}
	found := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "hooks.SessionStart=") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("hooks.SessionStart= override not found; args=%v", args)
	}
}

// TestBuildCodexConvoArgs_HooksAbsentWithEmptyTaskID verifies that no hook args
// appear when TaskID is empty (fail-open invariant).
func TestBuildCodexConvoArgs_HooksAbsentWithEmptyTaskID(t *testing.T) {
	makeFakeSybraCLI(t)

	a := &Agent{ID: "a", Provider: "codex"} // TaskID empty
	args := buildCodexConvoArgs(a, RunConfig{}, "hello")

	for _, arg := range args {
		if strings.Contains(arg, "hooks.") {
			t.Errorf("hook override must be absent when TaskID empty; got %q", arg)
		}
		if arg == "--dangerously-bypass-hook-trust" {
			t.Errorf("--dangerously-bypass-hook-trust must be absent when TaskID empty")
		}
	}
}

// TestBuildCodexConvoArgs_HooksBeforePrompt verifies that hook -c args appear
// before the prompt (last positional argument).
func TestBuildCodexConvoArgs_HooksBeforePrompt(t *testing.T) {
	makeFakeSybraCLI(t)

	a := &Agent{ID: "a", Provider: "codex", TaskID: "task-abc123"}
	const prompt = "do the work"
	args := buildCodexConvoArgs(a, RunConfig{}, prompt)

	promptIdx := -1
	for i, arg := range args {
		if arg == prompt {
			promptIdx = i
			break
		}
	}
	if promptIdx < 0 {
		t.Fatalf("prompt not found in args; args=%v", args)
	}

	for i, arg := range args {
		if strings.HasPrefix(arg, "hooks.") && i > promptIdx {
			t.Errorf("hook override at index %d appears after prompt at index %d; args=%v", i, promptIdx, args)
		}
	}
}
