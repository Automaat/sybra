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

// prepCacheFresh reports whether wtPath's current HEAD matches the SHA recorded
// by the last known-good prep pass AND is known to be published on the push
// remote.
// The remote-tracking ref is the staleness signal, but a bare read of it is not
// enough: PrepareForTask's earlier FetchOrigin only refreshes origin's wildcard
// and only outside its debounce window, and never touches a fork remote at all.
// So this refreshes the push-remote's tracking ref from the live remote before
// comparing — otherwise an out-of-band push to the task branch (from a human,
// another Sybra clone/machine, CI, or the web UI) landing after the marker was
// written would be invisible, and the cache hit would skip the reconcile/rebase
// step that adopts those commits, starting the agent from stale code that later
// conflicts on push. Fails closed on a refresh error: redo the full prep rather
// than reuse against a possibly-stale ref. A HEAD that still matches the marker
// but trails its own (now-refreshed) tracking ref is stale. A missing tracking
// ref is stale too: it can mean the prior best-effort push failed after the
// marker was written, and a cache hit would skip the push that should
// publish/recover it.
//
// The marker only pins HEAD, so a prior run could have left staged/uncommitted
// edits after writing it. Reusing such a worktree would skip sanitize,
// reconcile, and push with that dirty state intact — so a non-clean worktree is
// treated as a stale marker too. Fails closed: any error reading dirtiness
// redoes the full prep rather than risk a dirty reuse.
func prepCacheFresh(ctx context.Context, wtPath, wtBranch string) bool {
	key, ok := readPrepCacheSHA(wtPath)
	if !ok {
		return false
	}
	head, err := project.CurrentCommit(ctx, wtPath)
	if err != nil || head != key {
		return false
	}
	if dirty, err := project.IsWorktreeDirty(ctx, wtPath); err != nil || dirty {
		return false
	}
	if wtBranch == "" {
		return true
	}
	remote := project.PushRemote(ctx, wtPath)
	if err := project.RefreshTrackingRef(ctx, wtPath, remote, wtBranch); err != nil {
		return false
	}
	tracking, ok := project.RemoteTrackingSHA(ctx, wtPath, remote, wtBranch)
	if !ok || tracking != head {
		return false
	}
	return true
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
