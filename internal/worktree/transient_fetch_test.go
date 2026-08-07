package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// writeFakeGitFailingPRHeadFetch installs a `git` wrapper ahead of the real
// one on PATH that fails only the `fetch origin +refs/pull/<N>/head:...`
// invocation FetchPRHead makes, with a message matching one of
// project.transientNetworkMarkers (DNS resolution failure). Every other git
// invocation is passed through to the real binary unchanged.
func writeFakeGitFailingPRHeadFetch(t *testing.T) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("LookPath(git): %v", err)
		panic("unreachable")
	}
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
has_fetch=0
has_pull_ref=0
for arg in "$@"; do
  case "$arg" in
    fetch) has_fetch=1 ;;
    *refs/pull/*) has_pull_ref=1 ;;
  esac
done
if [ "$has_fetch" = "1" ] && [ "$has_pull_ref" = "1" ]; then
  echo "fatal: unable to access 'https://github.com/...': Could not resolve host: github.com" >&2
  exit 128
fi
exec %q "$@"
`, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
		panic("unreachable")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestPrepareForReview_TransientPRHeadFetchClassified is the regression for
// the incident where a review dispatch's PR-head fetch failed with a DNS
// blip and the raw git error (not wrapped as ErrTransientFetch) fed the
// workflow circuit breaker, tripping the task to human-required after a few
// retries even though nothing needed a human decision.
func TestPrepareForReview_TransientPRHeadFetchClassified(t *testing.T) {
	h := prepareHarness(t, nil, 0)
	writeFakeGitFailingPRHeadFetch(t)

	const prNumber = 91
	h.m.prBranch = func(_ string, _ int) (string, error) { return "some-branch", nil }

	tk, err := h.tasks.Create("review pr", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": h.proj.ID,
		"pr_number":  prNumber,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
		panic("unreachable")
	}

	_, err = h.m.PrepareForReview(context.Background(), tk)
	if err == nil {
		t.Fatal("PrepareForReview: want error from failing PR-head fetch, got nil")
		panic("unreachable")
	}
	if !errors.Is(err, ErrTransientFetch) {
		t.Fatalf("PrepareForReview error = %v, want wrapped ErrTransientFetch", err)
	}
}

// TestPrepareForFix_TransientPRHeadFetchClassified mirrors
// TestPrepareForReview_TransientPRHeadFetchClassified for the fix-worktree
// fork-PR fallback path, which hits the same FetchPRHead call.
func TestPrepareForFix_TransientPRHeadFetchClassified(t *testing.T) {
	h := prepareHarness(t, nil, 0)
	writeFakeGitFailingPRHeadFetch(t)

	const prNumber = 92
	h.m.prBranch = func(_ string, _ int) (string, error) { return "fork/pr-branch", nil }

	tk, err := h.tasks.Create("fix fork pr", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("create task: %v", err)
		panic("unreachable")
	}
	tk, err = h.tasks.UpdateMap(tk.ID, map[string]any{"project_id": h.proj.ID})
	if err != nil {
		t.Fatalf("update task: %v", err)
		panic("unreachable")
	}

	_, err = h.m.PrepareForFix(context.Background(), tk, prNumber)
	if err == nil {
		t.Fatal("PrepareForFix: want error from failing PR-head fetch, got nil")
		panic("unreachable")
	}
	if !errors.Is(err, ErrTransientFetch) {
		t.Fatalf("PrepareForFix error = %v, want wrapped ErrTransientFetch", err)
	}
}
