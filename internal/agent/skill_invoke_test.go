package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRewriteSkillInvocations(t *testing.T) {
	t.Parallel()
	skills := []string{"plan-critic", "sybra-triage", "sybra-plan", "staff-code-review"}
	tests := []struct {
		name   string
		in     string
		want   string
		skills []string // nil = use package-level skills
	}{
		{
			name: "leading slash invocation",
			in:   "/plan-critic /tmp/sybra-plan-abc.md",
			want: "$plan-critic /tmp/sybra-plan-abc.md",
		},
		{
			name: "mid-sentence invocation",
			in:   "Triage task 123 using /sybra-triage skill.",
			want: "Triage task 123 using $sybra-triage skill.",
		},
		{
			name: "multiple invocations",
			in:   "Run /staff-code-review then /plan-critic to finish.",
			want: "Run $staff-code-review then $plan-critic to finish.",
		},
		{
			name: "path must not be rewritten",
			in:   "Save to /tmp/sybra-plan-xxx.md and read /home/user/sybra-triage/log",
			want: "Save to /tmp/sybra-plan-xxx.md and read /home/user/sybra-triage/log",
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
			name:   "no skill names leaves prompt untouched",
			in:     "/plan-critic here",
			want:   "/plan-critic here",
			skills: []string{},
		},
		{
			// Validates descending-length sort: "plan" must not consume the
			// prefix of "plan-critic" when both are present.
			name:   "overlapping names: longer match wins",
			in:     "Run /plan-critic then /plan to finish.",
			want:   "Run $plan-critic then $plan to finish.",
			skills: []string{"plan", "plan-critic"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			names := skills
			if tt.skills != nil {
				names = tt.skills
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

func TestListPluginSkillDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Valid plugin layout: <root>/sai/staff-code-review/1.7.0/skills/staff-code-review/SKILL.md
	mkSkill(t, root, "sai", "staff-code-review", "1.7.0", "staff-code-review")
	// Older version of same skill — must dedupe.
	mkSkill(t, root, "sai", "staff-code-review", "1.6.0", "staff-code-review")
	// Different plugin under same marketplace.
	mkSkill(t, root, "sai", "council", "1.5.0", "council")
	// Different marketplace.
	mkSkill(t, root, "anthropic-agent-skills", "document-skills", "f458cee31a75", "template")
	// Hidden marketplace ignored.
	mkSkill(t, root, ".hidden", "foo", "1.0.0", "foo")
	// Skill dir missing SKILL.md is ignored.
	if err := os.MkdirAll(filepath.Join(root, "sai", "no-skill", "1.0.0", "skills", "no-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := listPluginSkillDirs(root)
	want := map[string]bool{
		"staff-code-review": true,
		"council":           true,
		"template":          true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected skill %q in %v", name, got)
		}
	}
}

func TestListPluginSkillDirsMissingRoot(t *testing.T) {
	t.Parallel()
	if got := listPluginSkillDirs("/nonexistent/plugins/cache"); got != nil {
		t.Errorf("got %v, want nil for missing root", got)
	}
}

func mkSkill(t *testing.T, root, marketplace, plugin, version, skill string) {
	t.Helper()
	skillDir := filepath.Join(root, marketplace, plugin, version, "skills", skill)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
