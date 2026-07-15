package agent

import (
	"testing"
)

func TestClaudePermissionArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		allowed      []string
		requirePerms bool
		mode         string
		want         []string
	}{
		{"allowlist wins over bypass", []string{"Bash", "Read"}, false, "bypass", []string{"--allowedTools", "Bash,Read"}},
		{"allowlist wins over auto", []string{"Bash"}, false, "auto", []string{"--allowedTools", "Bash"}},
		{"allowlist wins over requirePerms", []string{"Read"}, true, "auto", []string{"--allowedTools", "Read"}},
		{"requirePerms → nil", nil, true, "bypass", nil},
		{"requirePerms with auto → nil", nil, true, "auto", nil},
		{"auto mode", nil, false, "auto", []string{"--permission-mode", "auto"}},
		{"bypass mode", nil, false, "bypass", []string{"--dangerously-skip-permissions"}},
		{"empty mode → bypass", nil, false, "", []string{"--dangerously-skip-permissions"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := claudePermissionArgs(tc.allowed, tc.requirePerms, tc.mode)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestClassifyAutoModeDenial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tr   ToolResultBlock
		want bool
	}{
		{
			"exact match",
			ToolResultBlock{IsError: true, Content: "denied by the claude code auto mode classifier"},
			true,
		},
		{
			"mixed case",
			ToolResultBlock{IsError: true, Content: "DENIED BY THE CLAUDE CODE AUTO MODE CLASSIFIER"},
			true,
		},
		{
			"with surrounding text",
			ToolResultBlock{IsError: true, Content: "operation failed: denied by the claude code auto mode classifier for tool bash"},
			true,
		},
		{
			"multiline content",
			ToolResultBlock{IsError: true, Content: "line one\ndenied by the claude code auto mode classifier\nline three"},
			true,
		},
		{
			"not an error (IsError false)",
			ToolResultBlock{IsError: false, Content: "denied by the claude code auto mode classifier"},
			false,
		},
		{
			"unrelated error",
			ToolResultBlock{IsError: true, Content: "permission denied: file not found"},
			false,
		},
		{
			"empty",
			ToolResultBlock{IsError: true, Content: ""},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyAutoModeDenial(tc.tr); got != tc.want {
				t.Errorf("classifyAutoModeDenial = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNoteAndGetPermissionDenials(t *testing.T) {
	t.Parallel()
	a := &Agent{}

	if got := a.GetPermissionDenials(); got != nil {
		t.Fatalf("expected nil before any denials, got %v", got)
	}

	a.NotePermissionDenial("tool-1", "denied because rm -rf")
	a.NotePermissionDenial("tool-2", "denied because force push")

	denials := a.GetPermissionDenials()
	if len(denials) != 2 {
		t.Fatalf("expected 2 denials, got %d", len(denials))
	}
	if denials[0].ToolUseID != "tool-1" || denials[0].Reason != "denied because rm -rf" {
		t.Errorf("first denial mismatch: %+v", denials[0])
	}
	if denials[1].ToolUseID != "tool-2" {
		t.Errorf("second denial mismatch: %+v", denials[1])
	}

	// GetPermissionDenials returns a copy — mutations don't affect original
	denials[0].ToolUseID = "mutated"
	original := a.GetPermissionDenials()
	if len(original) == 0 {
		t.Fatal("expected copy to still have denials")
	}
	if original[0].ToolUseID == "mutated" {
		t.Error("GetPermissionDenials returned a reference, not a copy")
	}
}

func TestGetHeadlessPermissionMode(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	if got := a.GetHeadlessPermissionMode(); got != "" {
		t.Errorf("zero-value should return empty, got %q", got)
	}
	a.headlessPermissionMode = "auto"
	if got := a.GetHeadlessPermissionMode(); got != "auto" {
		t.Errorf("got %q, want auto", got)
	}
}
