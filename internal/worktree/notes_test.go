package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/notes"
)

func TestEnsureNotesFile_SeedsAndExcludes(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	if err := ensureNotesFile(context.Background(), wt); err != nil {
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
		if err := ensureNotesFile(context.Background(), wt); err != nil {
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

// TestAppendSetupFailureNote_SeedsAndAppends confirms a fresh worktree (no
// NOTES.md yet, as fix-role prepares don't call seedWorktree) gets the
// scratchpad created, excluded, and the failure recorded so SeedWorkingMemory
// surfaces it to the fix agent.
func TestAppendSetupFailureNote_SeedsAndAppends(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	setupErr := errors.New(`setup command "npm run build:desktop" failed: exit status 1`)
	if err := appendSetupFailureNote(context.Background(), wt, setupErr); err != nil {
		t.Fatalf("appendSetupFailureNote: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(wt, notes.FileName))
	if err != nil {
		t.Fatalf("read scratchpad: %v", err)
	}
	got := string(content)
	if !strings.HasPrefix(got, notes.SeedTemplate) {
		t.Errorf("expected seed template preserved as prefix, got:\n%s", got)
	}
	if !strings.Contains(got, "Setup failure") || !strings.Contains(got, setupErr.Error()) {
		t.Errorf("scratchpad missing setup failure note: %q", got)
	}

	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.Contains(string(out), notes.FileName) {
		t.Errorf("expected %s to be git-excluded, got status:\n%s", notes.FileName, out)
	}
}

// TestAppendSetupFailureNote_PreservesExisting confirms the note is appended
// after any accrued agent memory, not clobbering it.
func TestAppendSetupFailureNote_PreservesExisting(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	accrued := "# Working Notes\n\n## Decisions\n- picked the BK-tree approach\n"
	if err := os.WriteFile(filepath.Join(wt, notes.FileName), []byte(accrued), 0o644); err != nil {
		t.Fatal(err)
	}

	setupErr := errors.New("exit status 1")
	if err := appendSetupFailureNote(context.Background(), wt, setupErr); err != nil {
		t.Fatalf("appendSetupFailureNote: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(wt, notes.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), accrued) {
		t.Errorf("appendSetupFailureNote clobbered accrued memory: %q", got)
	}
	if !strings.Contains(string(got), setupErr.Error()) {
		t.Errorf("scratchpad missing setup failure note: %q", got)
	}
}

func TestSetupFailureMarker_WriteReadClear(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	setupErr := errors.New("exit status 1")
	if err := writeSetupFailureMarker(context.Background(), wt, setupErr); err != nil {
		t.Fatalf("writeSetupFailureMarker: %v", err)
	}

	got, ok, err := ReadSetupFailureMarker(wt)
	if err != nil {
		t.Fatalf("ReadSetupFailureMarker: %v", err)
	}
	if !ok {
		t.Fatal("ReadSetupFailureMarker reported no marker")
	}
	if got != setupErr.Error() {
		t.Fatalf("ReadSetupFailureMarker = %q, want %q", got, setupErr.Error())
	}

	if err := clearSetupFailureMarker(wt); err != nil {
		t.Fatalf("clearSetupFailureMarker: %v", err)
	}
	got, ok, err = ReadSetupFailureMarker(wt)
	if err != nil {
		t.Fatalf("ReadSetupFailureMarker after clear: %v", err)
	}
	if ok || got != "" {
		t.Fatalf("ReadSetupFailureMarker after clear = (%q, %v), want empty,false", got, ok)
	}
}
