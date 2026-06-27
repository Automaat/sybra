package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/notes"
)

func TestEnsureNotesFile_SeedsAndExcludes(t *testing.T) {
	if !hasGit() {
		t.Skip("git not available")
	}
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	if err := ensureNotesFile(wt); err != nil {
		t.Fatalf("ensureNotesFile: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(wt, notes.FileName))
	if err != nil {
		t.Fatalf("read scratchpad: %v", err)
	}
	if string(content) != notes.SeedTemplate {
		t.Errorf("scratchpad = %q, want seed template", content)
	}

	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.Contains(string(out), notes.FileName) {
		t.Errorf("expected %s to be git-excluded, got status:\n%s", notes.FileName, out)
	}
}

func TestEnsureNotesFile_PreservesExistingContent(t *testing.T) {
	if !hasGit() {
		t.Skip("git not available")
	}
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	// Simulate an agent that has already accrued working memory across a run.
	accrued := "# Working Notes\n\n## Decisions\n- picked the BK-tree approach\n"
	path := filepath.Join(wt, notes.FileName)
	if err := os.WriteFile(path, []byte(accrued), 0o644); err != nil {
		t.Fatal(err)
	}

	// A worktree re-prepare (resume/reuse) must not clobber it.
	for range 3 {
		if err := ensureNotesFile(wt); err != nil {
			t.Fatalf("ensureNotesFile: %v", err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != accrued {
		t.Errorf("ensureNotesFile clobbered agent memory: got %q, want %q", got, accrued)
	}

	// Exclude must still be present and not duplicated across the re-runs.
	excludeBytes, err := exec.Command("git", "-C", wt, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	excludePath := strings.TrimSpace(string(excludeBytes))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wt, excludePath)
	}
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if n := strings.Count(string(data), "/"+notes.FileName); n != 1 {
		t.Errorf("expected exactly 1 exclude entry, got %d. exclude:\n%s", n, data)
	}
}
