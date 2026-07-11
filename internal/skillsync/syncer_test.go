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
	keep := []byte("---\nname: sybra-keep\ndescription: test\n---\n\n# sybra-keep")
	if err := os.WriteFile(filepath.Join(srcDir, "sybra-keep.md"), keep, 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	sybraOrphan := filepath.Join(dstDir, "sybra-orphan")
	if err := os.MkdirAll(sybraOrphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sybraOrphan, "SKILL.md"), []byte("---\nname: sybra-orphan\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	userSkill := filepath.Join(dstDir, "ship-issue")
	if err := os.MkdirAll(userSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkill, "SKILL.md"), []byte("---\nname: ship-issue\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	newSyncer().SyncDir(srcDir, dstDir)

	if _, err := os.Stat(sybraOrphan); !os.IsNotExist(err) {
		t.Errorf("sybra- orphan skill dir should be removed: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(userSkill, "SKILL.md")); err != nil {
		t.Errorf("user skill without sybra- prefix must be preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "sybra-keep", "SKILL.md")); err != nil {
		t.Errorf("sybra-keep/SKILL.md missing: %v", err)
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

// TestSyncDirSkipsUserOwnedSymlink locks that the syncer never overwrites a
// destination skill the user owns as a symlink (e.g. one symlinked from their
// dotfiles), even when a bundle skill shares its name.
func TestSyncDirSkipsUserOwnedSymlink(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "shared.md"),
		[]byte("---\nname: shared\ndescription: from sybra\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	userSrc := t.TempDir()
	if err := os.WriteFile(filepath.Join(userSrc, "SKILL.md"), []byte("USER OWNED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(userSrc, filepath.Join(dst, "shared")); err != nil {
		t.Fatal(err)
	}

	newSyncer().SyncDir(src, dst)

	info, err := os.Lstat(filepath.Join(dst, "shared"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("user-owned symlink was replaced (err=%v)", err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "shared", "SKILL.md"))
	if string(got) != "USER OWNED" {
		t.Errorf("user content overwritten: %q", got)
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

// TestRunInstallsToAgentDirs locks that Run mirrors skills into all three
// per-agent destinations — ~/.claude/skills, ~/.codex/skills, and the
// cross-agent ~/.agents/skills (read by Orca-launched agents) — when
// UserHomeDir is set.
func TestRunInstallsToAgentDirs(t *testing.T) {
	embeddedSrc := filepath.Join(t.TempDir(), "embedded")
	dataDir := filepath.Join(embeddedSrc, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := []byte("---\nname: embedded-skill\ndescription: from embed\n---\n\n# embed")
	if err := os.WriteFile(filepath.Join(dataDir, "embedded-skill.md"), skill, 0o644); err != nil {
		t.Fatal(err)
	}

	userHome := t.TempDir()
	newSyncer().Run(skillsync.Options{
		// A path without go.mod forces embedded mode (no cwd fallback to the
		// real repo this test runs in).
		RepoDir:     filepath.Join(t.TempDir(), "not-a-repo"),
		SkillsFS:    os.DirFS(embeddedSrc),
		PrimaryDst:  filepath.Join(t.TempDir(), "app-skills"),
		UserHomeDir: userHome,
	})

	for _, dir := range []string{".claude", ".codex", ".agents"} {
		p := filepath.Join(userHome, dir, "skills", "embedded-skill", "SKILL.md")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("skill missing in %s/skills: %v", dir, err)
		}
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

	for _, name := range []string{"plan-critic", "plan-fork", "sybra-plan", "sybra-test", "sybra-triage"} {
		if _, err := os.Stat(filepath.Join(primaryDst, name, "SKILL.md")); err != nil {
			t.Errorf("%s skill missing: %v", name, err)
		}
	}
}

// TestRunEmbeddedSkillsLandInAllDirsWhenGoModPresent verifies that skills
// present only in the embedded bundle (e.g. sybra-test) land in every
// destination — primaryDst and all three per-agent skill dirs — even when
// go.mod is present (merge mode, not embedded-only mode). Regression guard for
// the case where Run was thought to skip embedded skills when a repo was found.
func TestRunEmbeddedSkillsLandInAllDirsWhenGoModPresent(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Repo .claude/skills has a disk skill but NOT sybra-test — that one comes
	// only from the embedded bundle.
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	repoSkill := []byte("---\nname: repo-only\ndescription: t\n---\n")
	if err := os.WriteFile(filepath.Join(skillsSrc, "repo-only.md"), repoSkill, 0o644); err != nil {
		t.Fatal(err)
	}

	primaryDst := filepath.Join(t.TempDir(), "app-skills")
	userHome := t.TempDir()

	newSyncer().Run(skillsync.Options{
		RepoDir:     repoDir,
		SkillsFS:    skills.FS,
		PrimaryDst:  primaryDst,
		UserHomeDir: userHome,
	})

	allDsts := []string{
		primaryDst,
		filepath.Join(userHome, ".claude", "skills"),
		filepath.Join(userHome, ".codex", "skills"),
		filepath.Join(userHome, ".agents", "skills"),
	}
	for _, dir := range allDsts {
		// sybra-test lives only in the embedded bundle.
		if _, err := os.Stat(filepath.Join(dir, "sybra-test", "SKILL.md")); err != nil {
			t.Errorf("sybra-test/SKILL.md missing from %s: %v", dir, err)
		}
		// repo-only skill must also land in all destinations.
		if _, err := os.Stat(filepath.Join(dir, "repo-only", "SKILL.md")); err != nil {
			t.Errorf("repo-only/SKILL.md missing from %s: %v", dir, err)
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

func TestRunDowngradesCommitFlags(t *testing.T) {
	repoDir := t.TempDir()
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	dirSkillSrc := filepath.Join(skillsSrc, "dirskill")
	if err := os.MkdirAll(dirSkillSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	flatSkill := []byte("---\nname: flat\ndescription: test\n---\n\nCommit with `git commit -s -S -m msg`.")
	if err := os.WriteFile(filepath.Join(skillsSrc, "flat.md"), flatSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	dirSkill := []byte("---\nname: dirskill\ndescription: test\n---\n\nRun `git commit -sS -m msg`.")
	if err := os.WriteFile(filepath.Join(dirSkillSrc, "SKILL.md"), dirSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	orchDir := filepath.Join(repoDir, "orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := []byte("# orchestrator\n\n```bash\ngit commit -s -S -m \"type(scope): desc\"\n```")
	if err := os.WriteFile(filepath.Join(orchDir, "CLAUDE.md"), claude, 0o644); err != nil {
		t.Fatal(err)
	}

	primaryDst := filepath.Join(t.TempDir(), "app-skills")
	sybraHome := t.TempDir()
	newSyncer().Run(skillsync.Options{
		RepoDir:              repoDir,
		PrimaryDst:           primaryDst,
		SybraHomeDir:         sybraHome,
		DowngradeCommitFlags: true,
	})

	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	for _, p := range []string{
		filepath.Join(sybraHome, "CLAUDE.md"),
		filepath.Join(primaryDst, "flat", "SKILL.md"),
		filepath.Join(primaryDst, "dirskill", "SKILL.md"),
	} {
		got := read(p)
		if strings.Contains(got, "-S") {
			t.Errorf("%s still contains -S after downgrade:\n%s", p, got)
		}
		if !strings.Contains(got, "git commit -s") {
			t.Errorf("%s lost the -s sign-off flag:\n%s", p, got)
		}
	}
}

func TestRunKeepsCommitFlagsWhenSigningAvailable(t *testing.T) {
	repoDir := t.TempDir()
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	flatSkill := []byte("---\nname: flat\ndescription: test\n---\n\nCommit with `git commit -s -S -m msg`.")
	if err := os.WriteFile(filepath.Join(skillsSrc, "flat.md"), flatSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	primaryDst := filepath.Join(t.TempDir(), "app-skills")
	newSyncer().Run(skillsync.Options{
		RepoDir:              repoDir,
		PrimaryDst:           primaryDst,
		DowngradeCommitFlags: false,
	})
	b, err := os.ReadFile(filepath.Join(primaryDst, "flat", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "git commit -s -S") {
		t.Errorf("expected -s -S preserved, got:\n%s", b)
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
