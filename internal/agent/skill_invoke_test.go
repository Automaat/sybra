package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteSkillInvocations(t *testing.T) {
	t.Parallel()
	skills := []string{"plan-critic", "synapse-triage", "synapse-plan", "staff-code-review"}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "leading slash invocation",
			in:   "/plan-critic /tmp/synapse-plan-abc.md",
			want: "$plan-critic /tmp/synapse-plan-abc.md",
		},
		{
			name: "mid-sentence invocation",
			in:   "Triage task 123 using /synapse-triage skill.",
			want: "Triage task 123 using $synapse-triage skill.",
		},
		{
			name: "multiple invocations",
			in:   "Run /staff-code-review then /plan-critic to finish.",
			want: "Run $staff-code-review then $plan-critic to finish.",
		},
		{
			name: "path must not be rewritten",
			in:   "Save to /tmp/synapse-plan-xxx.md and read /home/user/synapse-triage/log",
			want: "Save to /tmp/synapse-plan-xxx.md and read /home/user/synapse-triage/log",
		},
		{
			name: "unknown slash command left alone",
			in:   "Run /unknown-skill now",
			want: "Run /unknown-skill now",
		},
		{
			name: "trailing punctuation",
			in:   "Invoke: /plan-critic.",
			want: "Invoke: $plan-critic.",
		},
		{
			name: "empty prompt",
			in:   "",
			want: "",
		},
		{
			name: "no skill names leaves prompt untouched",
			in:   "/plan-critic here",
			want: "/plan-critic here",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			names := skills
			if tt.name == "no skill names leaves prompt untouched" {
				names = nil
			}
			got := rewriteSkillInvocations(tt.in, names)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListSkillDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Valid skill dir.
	validDir := filepath.Join(root, "plan-critic")
	if err := os.MkdirAll(validDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validDir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Dir without SKILL.md (skipped).
	if err := os.MkdirAll(filepath.Join(root, "no-skill-md"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Hidden dir (skipped).
	hidden := filepath.Join(root, ".system")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Plain file (skipped).
	if err := os.WriteFile(filepath.Join(root, "foo.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := listSkillDirs(root)
	if len(got) != 1 || got[0] != "plan-critic" {
		t.Errorf("got %v, want [plan-critic]", got)
	}
}

func TestListSkillDirsMissingRoot(t *testing.T) {
	t.Parallel()
	got := listSkillDirs("/nonexistent/path/for/skills")
	if got != nil {
		t.Errorf("got %v, want nil for missing root", got)
	}
}
