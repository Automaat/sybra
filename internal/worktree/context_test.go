package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestWriteContextFile_WritesBeacon(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	tk := task.Task{
		ID:        "fa6919fc",
		Slug:      "bk-tree-on-per-base-buckets",
		Title:     "perf(history): implement BK-tree",
		ProjectID: "Automaat/sybra",
	}
	branch := "sybra/bk-tree-on-per-base-buckets-fa6919fc"

	if err := writeContextFile(tk, wt, branch); err != nil {
		t.Fatalf("writeContextFile: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(wt, contextFileName))
	if err != nil {
		t.Fatalf("read beacon: %v", err)
	}
	body := string(content)
	for _, want := range []string{tk.ID, branch, tk.Title, tk.ProjectID, wt} {
		if !strings.Contains(body, want) {
			t.Errorf("beacon missing %q. body:\n%s", want, body)
		}
	}
}

func TestWriteContextFile_AddsToInfoExclude(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	tk := task.Task{ID: "fa6919fc", Slug: "x", ProjectID: "p/r"}
	if err := writeContextFile(tk, wt, "sybra/x-fa6919fc"); err != nil {
		t.Fatalf("writeContextFile: %v", err)
	}

	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.Contains(string(out), contextFileName) {
		t.Errorf("expected %s to be ignored by git, got status:\n%s", contextFileName, out)
	}
}

func TestWriteContextFile_IdempotentExclude(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	tk := task.Task{ID: "fa6919fc", Slug: "x", ProjectID: "p/r"}
	for range 3 {
		if err := writeContextFile(tk, wt, "sybra/x-fa6919fc"); err != nil {
			t.Fatalf("writeContextFile: %v", err)
		}
	}

	excludePathBytes, err := exec.Command("git", "-C", wt, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	excludePath := strings.TrimSpace(string(excludePathBytes))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(wt, excludePath)
	}
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	count := strings.Count(string(data), "/"+contextFileName)
	if count != 1 {
		t.Errorf("expected exactly 1 exclude entry, got %d. exclude:\n%s", count, data)
	}
}

func TestExcludeWorkflowScratchFiles(t *testing.T) {
	wt := t.TempDir()
	mustRunInDir(t, wt, "git", "init", "-b", "main")

	if err := excludeWorkflowScratchFiles(wt); err != nil {
		t.Fatalf("excludeWorkflowScratchFiles: %v", err)
	}
	for _, file := range []string{
		".sybra-review-task.md",
		".sybra-diff-task.patch",
		".sybra-plan-task.md",
		".sybra-plan-contract-task.json",
		".sybra-critique-task.md",
	} {
		if err := os.WriteFile(filepath.Join(wt, file), []byte("scratch"), 0o644); err != nil {
			t.Fatalf("write scratch file: %v", err)
		}
	}

	out, err := exec.Command("git", "-C", wt, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Fatalf("workflow scratch files should be ignored, got status:\n%s", got)
	}
}
