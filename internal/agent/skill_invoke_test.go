package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

func TestDiscoverCodexSkillsUsesPluginListAndSkipsCodexCache(t *testing.T) {
	home := t.TempDir()
	mkLocalSkill(t, filepath.Join(home, ".codex", "skills"), "codex-direct")
	mkLocalSkill(t, filepath.Join(home, ".claude", "skills"), "claude-direct")
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "stale", "1.0.0", "stale-codex-cache")
	mkSkill(t, filepath.Join(home, ".claude", "plugins", "cache"), "sai", "council", "1.0.0", "claude-plugin")

	pluginRoot := filepath.Join(home, "marketplaces", "sai", "plugins", "plugin-json")
	skillRoot := filepath.Clean(filepath.Join(pluginRoot, "../../codex/plugin-json"))
	mkSkillRoot(t, skillRoot)
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "../../codex/plugin-json")
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"claude-direct", "claude-plugin", "codex-direct", "plugin-json"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListCodexPluginSourceSkillsRejectsInvalidFrontmatterName(t *testing.T) {
	pluginRoot := t.TempDir()
	singleSkillRoot := filepath.Join(pluginRoot, "opaque-dir")
	mkNamedSkillRoot(t, singleSkillRoot, "tmp/secret", "\n")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "opaque-dir")

	got, ok := listCodexPluginSourceSkills(pluginRoot)
	if !ok {
		t.Fatal("listCodexPluginSourceSkills returned ok=false")
	}
	want := []string{"opaque-dir"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListCodexPluginSourceSkillsRejectsSpacedFrontmatterName(t *testing.T) {
	pluginRoot := t.TempDir()
	singleSkillRoot := filepath.Join(pluginRoot, "staff-code-review")
	mkNamedSkillRoot(t, singleSkillRoot, "Staff Code Review", "\n")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "staff-code-review")

	got, ok := listCodexPluginSourceSkills(pluginRoot)
	if !ok {
		t.Fatal("listCodexPluginSourceSkills returned ok=false")
	}
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackToCodexCache(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	withCodexPluginListError(t, errors.New("codex plugin list unavailable"))

	got := discoverCodexSkillsInHome(home)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListCodexPluginSourceSkills(t *testing.T) {
	pluginRoot := filepath.Join(t.TempDir(), "marketplaces", "sai", "plugins", "plugin")
	mkLocalSkill(t, filepath.Join(pluginRoot, "skills"), "direct-skill")
	mkLocalSkill(t, filepath.Join(pluginRoot, "copilot-skills", "nested"), "recursive-skill")
	writePluginManifest(t, filepath.Join(pluginRoot, "plugin.json"), "copilot-skills/")
	singleSkillRoot := filepath.Clean(filepath.Join(pluginRoot, "../../codex/opaque-dir"))
	mkNamedSkillRoot(t, singleSkillRoot, "single-skill", "\n")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "../../codex/opaque-dir")
	claudePluginSkillRoot := filepath.Join(pluginRoot, "claude-skills", "claude-plugin-skill")
	mkSkillRoot(t, claudePluginSkillRoot)
	writePluginManifest(t, filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), "claude-skills/")

	got, ok := listCodexPluginSourceSkills(pluginRoot)
	if !ok {
		t.Fatal("listCodexPluginSourceSkills returned ok=false")
	}
	want := []string{"claude-plugin-skill", "direct-skill", "recursive-skill", "single-skill"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListCodexPluginSourceSkillsReadsCRLFFrontmatter(t *testing.T) {
	pluginRoot := t.TempDir()
	singleSkillRoot := filepath.Join(pluginRoot, "opaque-dir")
	mkNamedSkillRoot(t, singleSkillRoot, "single-skill", "\r\n")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "opaque-dir")

	got, ok := listCodexPluginSourceSkills(pluginRoot)
	if !ok {
		t.Fatal("listCodexPluginSourceSkills returned ok=false")
	}
	want := []string{"single-skill"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsKeepsStdoutFromNonzeroPluginList(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	pluginRoot := filepath.Join(home, "plugin")
	mkLocalSkill(t, filepath.Join(pluginRoot, "skills"), "new-skill")
	withCodexPluginListOutput(t, codexPluginListPayload(t, pluginRoot, true), errors.New("codex plugin list exited nonzero"))

	got := discoverCodexSkillsInHome(home)
	want := []string{"new-skill", "staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackOnMalformedManifest(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	goodPluginRoot := filepath.Join(home, "good-plugin")
	mkLocalSkill(t, filepath.Join(goodPluginRoot, "skills"), "good-skill")
	pluginRoot := filepath.Join(home, "broken-plugin")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	withCodexPluginListJSON(t, codexPluginListPayloadForRoots(t, true, goodPluginRoot, pluginRoot))

	got := discoverCodexSkillsInHome(home)
	want := []string{"good-skill", "staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackOnUnsupportedManifestSkills(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	pluginRoot := filepath.Join(home, "unsupported-plugin")
	mkLocalSkill(t, filepath.Join(pluginRoot, "skills"), "direct-skill")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte(`{"skills":{"path":"skills"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"direct-skill", "staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackOnMissingPluginSourcePath(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	payload := struct {
		Installed []codexPlugin `json:"installed"`
	}{
		Installed: []codexPlugin{{}},
	}
	enabled := true
	payload.Installed[0].Enabled = &enabled
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	withCodexPluginListJSON(t, data)

	got := discoverCodexSkillsInHome(home)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackOnStalePluginSourcePath(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	withCodexPluginListJSON(t, codexPluginListPayload(t, filepath.Join(home, "deleted-plugin"), true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackOnEmptyPluginSourcePath(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	emptyPluginRoot := filepath.Join(home, "empty-plugin")
	if err := os.MkdirAll(emptyPluginRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	withCodexPluginListJSON(t, codexPluginListPayload(t, emptyPluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsDoesNotFallbackForNoSkillManifest(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "old-skill", "1.0.0", "old-skill")
	pluginRoot := filepath.Join(home, "command-only-plugin")
	writePluginManifestJSON(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte(`{"name":"command-only"}`))
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestDiscoverCodexSkillsRejectsUnsafeManifestPath(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	pluginRoot := filepath.Join(home, "plugin")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "/")
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsRejectsUnsafeAbsolutePeerPath(t *testing.T) {
	home := t.TempDir()
	pluginRoot := filepath.Join(home, "plugin")
	otherRoot := filepath.Join(home, "other")
	mkLocalSkill(t, otherRoot, "evil")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), otherRoot)
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestDiscoverCodexSkillsFallsBackOnStaleManifestTarget(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "staff-code-review", "1.0.0", "staff-code-review")
	pluginRoot := filepath.Join(home, "plugin")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "../../codex/deleted-skill")
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsDoesNotFallbackForDuplicateManifestSkill(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "old-skill", "1.0.0", "old-skill")
	pluginRoot := filepath.Join(home, "plugin")
	mkLocalSkill(t, filepath.Join(pluginRoot, "skills"), "current-skill")
	writePluginManifest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), "skills")
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"current-skill"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsFallsBackForMissingManifestArrayEntry(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "missing-skill", "1.0.0", "missing-skill")
	pluginRoot := filepath.Join(home, "plugin")
	mkLocalSkill(t, filepath.Join(pluginRoot, "skills-a"), "current-skill")
	writePluginManifestJSON(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), []byte(`{"skills":["skills-a","skills-b"]}`))
	withCodexPluginListJSON(t, codexPluginListPayload(t, pluginRoot, true))

	got := discoverCodexSkillsInHome(home)
	want := []string{"current-skill", "missing-skill"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiscoverCodexSkillsScopesFallbackToFailedPlugins(t *testing.T) {
	home := t.TempDir()
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "old-skill", "1.0.0", "old-skill")
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "sai", "missing-plugin", "1.0.0", "missing-plugin")
	mkSkill(t, filepath.Join(home, ".codex", "plugins", "cache"), "other", "missing-plugin", "1.0.0", "wrong-marketplace-skill")
	noSkillPluginRoot := filepath.Join(home, "old-skill")
	writePluginManifestJSON(t, filepath.Join(noSkillPluginRoot, ".codex-plugin", "plugin.json"), []byte(`{"name":"old-skill"}`))
	missingPluginRoot := filepath.Join(home, "deleted-plugin")
	payload := struct {
		Installed []codexPlugin `json:"installed"`
	}{
		Installed: []codexPlugin{
			codexPluginWithNamedSource("old-skill", noSkillPluginRoot, true),
			codexPluginWithNamedSource("missing-plugin", missingPluginRoot, true),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	withCodexPluginListJSON(t, data)

	got := discoverCodexSkillsInHome(home)
	want := []string{"missing-plugin"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseCodexPluginListSkills(t *testing.T) {
	enabledRoot := filepath.Join(t.TempDir(), "enabled")
	disabledRoot := filepath.Join(t.TempDir(), "disabled")
	mkLocalSkill(t, filepath.Join(enabledRoot, "skills"), "enabled-skill")
	mkLocalSkill(t, filepath.Join(disabledRoot, "skills"), "disabled-skill")

	var enabled = true
	var disabled = false
	payload := struct {
		Installed []codexPlugin `json:"installed"`
	}{
		Installed: []codexPlugin{
			codexPluginWithSource(enabledRoot, &enabled),
			codexPluginWithSource(disabledRoot, &disabled),
			codexPluginWithSource("", &disabled),
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	got, fallbackPlugins, err := parseCodexPluginListSkills(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(fallbackPlugins) > 0 {
		t.Fatalf("parseCodexPluginListSkills returned fallbackPlugins=%v", fallbackPlugins)
	}
	want := []string{"enabled-skill"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListPluginSkillDirsReadsPluginManifestLayouts(t *testing.T) {
	root := t.TempDir()
	versionDir := filepath.Join(root, "sai", "staff-code-review", "0.1.0")
	mkLocalSkill(t, filepath.Join(versionDir, "copilot-skills"), "staff-code-review")
	writePluginManifest(t, filepath.Join(versionDir, "plugin.json"), "copilot-skills/")

	got := listPluginSkillDirs(root)
	want := []string{"staff-code-review"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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

func mkLocalSkill(t *testing.T, root, skill string) {
	t.Helper()
	skillDir := filepath.Join(root, skill)
	mkSkillRoot(t, skillDir)
}

func mkSkillRoot(t *testing.T, skillDir string) {
	t.Helper()
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkNamedSkillRoot(t *testing.T, skillDir, name, newline string) {
	t.Helper()
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("---" + newline + "name: " + name + newline + "---" + newline)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePluginManifest(t *testing.T, path, skills string) {
	t.Helper()
	data, err := json.Marshal(struct {
		Skills string `json:"skills"`
	}{Skills: skills})
	if err != nil {
		t.Fatal(err)
	}
	writePluginManifestJSON(t, path, data)
}

func writePluginManifestJSON(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func withCodexPluginListJSON(t *testing.T, data []byte) {
	t.Helper()
	orig := runCodexPluginListJSON
	runCodexPluginListJSON = func() ([]byte, error) { return data, nil }
	t.Cleanup(func() { runCodexPluginListJSON = orig })
}

func withCodexPluginListError(t *testing.T, err error) {
	t.Helper()
	orig := runCodexPluginListJSON
	runCodexPluginListJSON = func() ([]byte, error) { return nil, err }
	t.Cleanup(func() { runCodexPluginListJSON = orig })
}

func withCodexPluginListOutput(t *testing.T, data []byte, err error) {
	t.Helper()
	orig := runCodexPluginListJSON
	runCodexPluginListJSON = func() ([]byte, error) { return data, err }
	t.Cleanup(func() { runCodexPluginListJSON = orig })
}

func codexPluginListPayload(t *testing.T, pluginRoot string, enabled bool) []byte {
	t.Helper()
	return codexPluginListPayloadForRoots(t, enabled, pluginRoot)
}

func codexPluginListPayloadForRoots(t *testing.T, enabled bool, pluginRoots ...string) []byte {
	t.Helper()
	plugins := make([]codexPlugin, 0, len(pluginRoots))
	for _, pluginRoot := range pluginRoots {
		plugins = append(plugins, codexPluginWithSource(pluginRoot, &enabled))
	}
	payload := struct {
		Installed []codexPlugin `json:"installed"`
	}{
		Installed: plugins,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func codexPluginWithSource(path string, enabled *bool) codexPlugin {
	var plugin codexPlugin
	plugin.Enabled = enabled
	plugin.Source.Path = path
	return plugin
}

func codexPluginWithNamedSource(name, path string, enabled bool) codexPlugin {
	var plugin codexPlugin
	plugin.Name = name
	plugin.MarketplaceName = "sai"
	plugin.Enabled = &enabled
	plugin.Source.Path = path
	return plugin
}
