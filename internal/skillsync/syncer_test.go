package skillsync_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/skills"
	"github.com/Automaat/sybra/internal/skillsync"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func newSyncer() *skillsync.Syncer {
	return &skillsync.Syncer{Logger: discardLogger()}
}

func TestSyncFile(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "sub")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "test.md")
	if err := os.WriteFile(srcFile, []byte("# hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstFile := filepath.Join(t.TempDir(), "out", "test.md")
	newSyncer().SyncFile(srcFile, dstFile)

	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("dst not written: %v", err)
	}
	if string(data) != "# hello" {
		t.Errorf("content = %q, want %q", string(data), "# hello")
	}
}

func TestSyncFileMissingSrc(t *testing.T) {
	dstFile := filepath.Join(t.TempDir(), "should-not-exist.md")
	newSyncer().SyncFile("/nonexistent/file.md", dstFile)

	if _, err := os.Stat(dstFile); !os.IsNotExist(err) {
		t.Error("dst should not be created when src missing")
	}
}

func TestSyncDir(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	frontmatter := func(name string) []byte {
		return []byte("---\nname: " + name + "\ndescription: test\n---\n\n# " + name)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(srcDir, name+".md"), frontmatter(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(srcDir, "c.txt"), []byte("content-c"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "no-frontmatter.md"), []byte("# nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	newSyncer().SyncDir(srcDir, dstDir)

	for _, name := range []string{"a", "b"} {
		skillMD := filepath.Join(dstDir, name, "SKILL.md")
		if _, err := os.Stat(skillMD); err != nil {
			t.Errorf("%s/SKILL.md missing at dst: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstDir, "no-frontmatter")); !os.IsNotExist(err) {
		t.Errorf("malformed skill should not be written: stat err=%v", err)
	}
}

func TestSyncDirRemovesOrphans(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := []byte("---\nname: keep\ndescription: test\n---\n\n# keep")
	if err := os.WriteFile(filepath.Join(srcDir, "keep.md"), keep, 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	orphan := filepath.Join(dstDir, "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "SKILL.md"), []byte("---\nname: orphan\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	newSyncer().SyncDir(srcDir, dstDir)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan skill dir should be removed: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "keep", "SKILL.md")); err != nil {
		t.Errorf("keep/SKILL.md missing: %v", err)
	}
}

func TestSyncDirCopiesDirectoryStyleSkill(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "skills")
	skillDir := filepath.Join(srcDir, "plan-critic")
	refDir := filepath.Join(skillDir, "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := []byte("---\nname: plan-critic\ndescription: test\n---\n\n# plan-critic")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), skillMD, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "checklist.md"), []byte("# checklist"), 0o644); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(srcDir, "bad-skill")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte("# no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	newSyncer().SyncDir(srcDir, dstDir)

	if _, err := os.Stat(filepath.Join(dstDir, "plan-critic", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing at dst: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "plan-critic", "references", "checklist.md")); err != nil {
		t.Errorf("nested reference missing at dst: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "bad-skill")); !os.IsNotExist(err) {
		t.Errorf("bad skill dir should not be copied: stat err=%v", err)
	}
}

func TestSyncDirDirectoryStyleAndFrontmatter(t *testing.T) {
	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := []byte("---\nname: good-flat\ndescription: t\n---\n\n# flat")
	if err := os.WriteFile(filepath.Join(srcDir, "good-flat.md"), flat, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "bad-flat.md"), []byte("# no-fm"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirSkill := filepath.Join(srcDir, "good-dir")
	if err := os.MkdirAll(dirSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	dirMD := []byte("---\nname: good-dir\ndescription: t\n---\n\n# dir")
	if err := os.WriteFile(filepath.Join(dirSkill, "SKILL.md"), dirMD, 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "codex-skills")
	preExisting := filepath.Join(dstDir, "bad-flat")
	if err := os.MkdirAll(preExisting, 0o755); err != nil {
		t.Fatal(err)
	}
	preExistingContent := []byte("---\nname: bad-flat\ndescription: valid\n---\n\n# valid")
	if err := os.WriteFile(filepath.Join(preExisting, "SKILL.md"), preExistingContent, 0o644); err != nil {
		t.Fatal(err)
	}

	newSyncer().SyncDir(srcDir, dstDir)

	if _, err := os.Stat(filepath.Join(dstDir, "good-flat", "SKILL.md")); err != nil {
		t.Errorf("good-flat not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "good-dir", "SKILL.md")); err != nil {
		t.Errorf("good-dir not written: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstDir, "bad-flat", "SKILL.md"))
	if err == nil && string(got) == "# no-fm" {
		t.Errorf("malformed skill overwrote valid dst: %q", got)
	}
}

func TestSyncDirMissingSrc(t *testing.T) {
	dstDir := filepath.Join(t.TempDir(), "should-not-exist")
	newSyncer().SyncDir("/nonexistent/dir", dstDir)

	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Error("dst dir should not be created when src missing")
	}
}

func TestRunPrefersEmbeddedWhenNoGoMod(t *testing.T) {
	embeddedSrc := filepath.Join(t.TempDir(), "embedded")
	dataDir := filepath.Join(embeddedSrc, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := []byte("---\nname: embedded-skill\ndescription: from embed\n---\n\n# embed")
	if err := os.WriteFile(filepath.Join(dataDir, "embedded-skill.md"), embedded, 0o644); err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "rogue.md"), []byte("# no-fm"), 0o644); err != nil {
		t.Fatal(err)
	}
	primaryDst := filepath.Join(t.TempDir(), "app-skills")

	newSyncer().Run(skillsync.Options{
		RepoDir:    repoDir,
		SkillsFS:   os.DirFS(embeddedSrc),
		PrimaryDst: primaryDst,
	})

	if _, err := os.Stat(filepath.Join(primaryDst, "embedded-skill", "SKILL.md")); err != nil {
		t.Errorf("embedded skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(primaryDst, "rogue")); !os.IsNotExist(err) {
		t.Errorf("rogue skill leaked into app skills dir")
	}
}

func TestRunMergesRepoAndEmbeddedWhenGoModPresent(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	repoSkill := []byte("---\nname: repo-skill\ndescription: t\n---\n")
	if err := os.WriteFile(filepath.Join(skillsSrc, "repo-skill.md"), repoSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	repoOverride := []byte("---\nname: shared\ndescription: repo\n---\n")
	if err := os.WriteFile(filepath.Join(skillsSrc, "shared.md"), repoOverride, 0o644); err != nil {
		t.Fatal(err)
	}

	embeddedSrc := filepath.Join(t.TempDir(), "embedded")
	dataDir := filepath.Join(embeddedSrc, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "embed-only.md"), []byte("---\nname: embed-only\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "shared.md"), []byte("---\nname: shared\ndescription: embed\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	primaryDst := filepath.Join(t.TempDir(), "app-skills")

	newSyncer().Run(skillsync.Options{
		RepoDir:    repoDir,
		SkillsFS:   os.DirFS(embeddedSrc),
		PrimaryDst: primaryDst,
	})

	if _, err := os.Stat(filepath.Join(primaryDst, "repo-skill", "SKILL.md")); err != nil {
		t.Errorf("repo skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(primaryDst, "embed-only", "SKILL.md")); err != nil {
		t.Errorf("embedded skill missing: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(primaryDst, "shared", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "description: repo") {
		t.Errorf("repo skill should override embedded duplicate, got %q", got)
	}
}

func TestRunShipsWorkflowSkillsFromEmbeddedBundle(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}

	primaryDst := filepath.Join(t.TempDir(), "app-skills")
	newSyncer().Run(skillsync.Options{
		RepoDir:    repoDir,
		SkillsFS:   skills.FS,
		PrimaryDst: primaryDst,
	})

	for _, name := range []string{"plan-critic", "plan-fork", "sybra-plan", "sybra-test-plan", "sybra-triage"} {
		if _, err := os.Stat(filepath.Join(primaryDst, name, "SKILL.md")); err != nil {
			t.Errorf("%s skill missing: %v", name, err)
		}
	}
}

func TestRunNoRepoDir(t *testing.T) {
	primaryDst := filepath.Join(t.TempDir(), "app-skills")
	// Should not panic; falls back to cwd.
	newSyncer().Run(skillsync.Options{PrimaryDst: primaryDst})
}

func TestRunWithRepoDirAndOrchestrator(t *testing.T) {
	repoDir := t.TempDir()
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	skillContent := []byte("---\nname: skill\ndescription: test\n---\n\n# skill")
	if err := os.WriteFile(filepath.Join(skillsSrc, "skill.md"), skillContent, 0o644); err != nil {
		t.Fatal(err)
	}
	orchDir := filepath.Join(repoDir, "orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "CLAUDE.md"), []byte("# orchestrator"), 0o644); err != nil {
		t.Fatal(err)
	}

	primaryDst := filepath.Join(t.TempDir(), "app-skills")
	sybraHome := t.TempDir()

	newSyncer().Run(skillsync.Options{
		RepoDir:      repoDir,
		PrimaryDst:   primaryDst,
		SybraHomeDir: sybraHome,
	})

	if _, err := os.Stat(filepath.Join(primaryDst, "skill", "SKILL.md")); err != nil {
		t.Errorf("repo skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sybraHome, "CLAUDE.md")); err != nil {
		t.Errorf("orchestrator CLAUDE.md not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sybraHome, "AGENTS.md")); err != nil {
		t.Errorf("orchestrator AGENTS.md not copied: %v", err)
	}
}

func TestHasYAMLFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain", "---\nname: x\n---\n", true},
		{"crlf", "---\r\nname: x\r\n---\r\n", true},
		{"leading whitespace", "  \n---\nx\n---\n", true},
		{"with BOM", "\ufeff---\nname: x\n---\n", true},
		{"missing", "# heading", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skillsync.HasYAMLFrontmatter([]byte(tc.in)); got != tc.want {
				t.Errorf("HasYAMLFrontmatter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
