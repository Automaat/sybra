package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

// prepCacheMarkerName is git-excluded (never committed) — it records the
// worktree HEAD SHA as of the last time this exact worktree's prep pipeline
// (fetch/heal/sanitize/reconcile+rebase/push, see PrepareForTask's reused-
// worktree fast path) is known to have completed successfully. A HEAD match
// on a later PrepareForTask call means "this exact commit was already fully
// prepped and pushed" — the whole pipeline can be skipped in favor of reuse.
//
// Besides PrepareForTask writing its own marker, MarkPrepFresh is the
// cross-package handoff a sibling recovery flow uses: branch-conflict-fix's
// verify_commits step calls it once it has independently confirmed the
// worktree's HEAD matches its remote-tracking ref (see
// workflow.execVerifyCommits), so the dispatch resumed right after recovery
// doesn't redo fetch/heal/sanitize/rebase/push/setup from scratch even
// though the recovery workflow (and the pr-fix agent it ran) already left
// the tree in a fully reconciled, pushed state (issue #2765).
const prepCacheMarkerName = ".sybra-prep-cache"

func readPrepCacheSHA(wtPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(wtPath, prepCacheMarkerName))
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(data))
	return sha, sha != ""
}

// prepCacheFresh reports whether wtPath's current HEAD matches the SHA
// recorded by the last known-good prep pass.
func prepCacheFresh(ctx context.Context, wtPath string) bool {
	key, ok := readPrepCacheSHA(wtPath)
	if !ok {
		return false
	}
	head, err := project.CurrentCommit(ctx, wtPath)
	if err != nil {
		return false
	}
	return head == key
}

// writePrepCacheSHA records wtPath's current HEAD as freshly prepped.
// Best-effort, like writeSetupCacheKey: a failed write only costs a future
// cache hit, never worktree creation itself.
func (m *Manager) writePrepCacheSHA(ctx context.Context, taskID, wtPath string) {
	head, err := project.CurrentCommit(ctx, wtPath)
	if err != nil || head == "" {
		m.logger.Warn("worktree.prep-cache-head", "task_id", taskID, "path", wtPath, "err", err)
		return
	}
	if err := addToInfoExclude(ctx, wtPath, prepCacheMarkerName); err != nil {
		m.logger.Warn("worktree.prep-cache-exclude", "task_id", taskID, "path", wtPath, "err", err)
	}
	path := filepath.Join(wtPath, prepCacheMarkerName)
	if err := os.WriteFile(path, []byte(head+"\n"), 0o600); err != nil {
		m.logger.Warn("worktree.prep-cache-write", "task_id", taskID, "path", wtPath, "err", err)
	}
}

// MarkPrepFresh is the cross-package handoff point implementing
// workflow.WorktreeGetter: a caller that has already established wtPath's
// HEAD is fully reconciled and pushed (e.g. branch-conflict-fix's
// verify_commits step, guarded on a remote-tracking-ref match before
// calling this) marks it so the next PrepareForTask for the same task
// reuses it without redoing the prep pipeline. No-op when wtPath is empty.
func (m *Manager) MarkPrepFresh(taskID, wtPath string) {
	if wtPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.writePrepCacheSHA(ctx, taskID, wtPath)
}
