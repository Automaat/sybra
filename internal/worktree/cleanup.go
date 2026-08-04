package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

var (
	bareTaskIDRe   = regexp.MustCompile(`^[0-9a-f]{8}$`)
	suffixTaskIDRe = regexp.MustCompile(`-([0-9a-f]{8})$`)
)

const worktreeQuarantineDirName = ".quarantine"

// taskIDFromWorktreeDir extracts the owning task ID from a worktree directory
// name: task.Task.DirName() is either the bare 8-hex-char ID (no slug yet) or
// "<slug>-<8hex-id>". Anything else returns "" (no match).
func taskIDFromWorktreeDir(name string) string {
	if bareTaskIDRe.MatchString(name) {
		return name
	}
	if m := suffixTaskIDRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// Remove cleans up the worktree for a task via git worktree remove.
func (m *Manager) Remove(ctx context.Context, taskID string) {
	t, err := m.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return
	}
	// Never touch an externally-adopted worktree: the tool that created it
	// (e.g. Orca) owns its lifecycle. Removing it would delete the user's
	// checkout from under them.
	if t.WorktreeDir != "" {
		return
	}
	wtPath := filepath.Join(m.dir, t.DirName())
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	// Never reap a worktree whose completed work never reached origin — a task
	// bounced to a terminal status (done/cancelled) after a failed push would
	// otherwise lose its finished-but-unpushed diff right here, before the
	// periodic orphan sweep's identical guard ever runs (#2593). This is the
	// per-task chokepoint fired on task completion (completion.OnComplete),
	// manual terminal transitions (UpdateTask), and explicit deletes
	// (DeleteTask); consistent with doctor cleanup, even those never bypass it.
	if project.HasUnpushedCommits(ctx, wtPath) {
		m.logger.Warn("worktree.cleanup.unpushed-commits", "path", wtPath)
		return
	}
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return
	}
	if err := project.RemoveWorktree(ctx, proj.ClonePath, wtPath); err != nil {
		m.logger.Error("worktree.cleanup", "path", wtPath, "err", err)
	} else {
		m.logger.Info("worktree.cleaned", "path", wtPath)
	}
}

// CleanupOrphaned removes worktree directories for deleted or completed tasks
// that have no running agent.
func (m *Manager) CleanupOrphaned(ctx context.Context) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	tasks, err := m.tasks.List()
	if err != nil {
		return
	}

	active := make(map[string]*task.Task, len(tasks))
	for i := range tasks {
		active[tasks[i].DirName()] = &tasks[i]
	}
	observedProtected := make(map[string]bool)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == worktreeQuarantineDirName {
			continue
		}
		name := e.Name()
		wtPath := filepath.Join(m.dir, name)

		t, exists := active[name]
		switch {
		case !exists:
			// Task deleted — remove worktree directory.
		case t.Status != task.StatusDone:
			continue
		case m.hasAgent != nil && m.hasAgent(t.ID):
			continue
		}

		if project.HasUnpushedCommits(ctx, wtPath) {
			m.observeProtectedWorktree(ctx, wtPath, taskIDFromWorktreeDir(name), observedProtected)
			continue
		}

		removed := false
		if exists && t.ProjectID != "" {
			if proj, perr := m.projects.Get(t.ProjectID); perr == nil {
				if err := project.RemoveWorktree(ctx, proj.ClonePath, wtPath); err != nil {
					m.logger.Error("worktree.orphan-cleanup", "path", wtPath, "err", err)
				} else {
					removed = true
				}
			}
		}
		if !removed {
			// Task deleted or project lookup failed — force-remove and prune after.
			if err := os.RemoveAll(wtPath); err != nil {
				m.logger.Error("worktree.orphan-cleanup", "path", wtPath, "err", err)
				continue
			}
		}
		m.logger.Info("worktree.orphan-cleaned", "path", wtPath)
	}
	m.resolveProtectedWorktrees(observedProtected)

	// Prune dangling admin entries across all projects.
	if m.projects == nil {
		return
	}
	projects, err := m.projects.List()
	if err != nil {
		return
	}
	for i := range projects {
		if err := project.PruneWorktrees(ctx, projects[i].ClonePath); err != nil {
			m.logger.Warn("worktree.prune", "project", projects[i].ID, "err", err)
		}
	}
}

func (m *Manager) observeProtectedWorktree(ctx context.Context, path, taskID string, observed map[string]bool) {
	if m.protected == nil {
		m.logger.Warn("worktree.orphan-cleanup.protected", "event", "legacy", "task_id", taskID, "path", path)
		return
	}
	obs := cleanup.Observation{
		Kind:          cleanup.ResourceWorktree,
		TaskID:        taskID,
		Path:          path,
		Reason:        cleanup.ReasonUnpushedCommits,
		ObservedHead:  worktreeHead(ctx, path),
		ObservedState: worktreeObservedState(ctx, path),
		BytesRetained: worktreeDirSize(path),
	}
	finding, event, err := m.protected.Observe(obs)
	if err != nil {
		m.logger.Warn("worktree.orphan-cleanup.protected.observe", "path", path, "err", err)
		return
	}
	observed[finding.ID] = true
	if !event.ShouldLog() {
		return
	}
	m.logger.Warn("worktree.orphan-cleanup.protected",
		"event", event,
		"task_id", taskID,
		"path", path,
		"head", finding.ObservedHead,
		"state", finding.ObservedState,
		"bytes_retained", finding.BytesRetained)
}

func (m *Manager) resolveProtectedWorktrees(observed map[string]bool) {
	if m.protected == nil {
		return
	}
	if err := m.protected.ResolveMissing(cleanup.ResourceWorktree, observed); err != nil {
		m.logger.Warn("worktree.orphan-cleanup.protected.resolve", "err", err)
	}
}

func worktreeHead(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func worktreeObservedState(ctx context.Context, path string) string {
	dirty, err := project.IsWorktreeDirty(ctx, path)
	if err != nil {
		return "dirty=unknown"
	}
	return fmt.Sprintf("dirty=%t", dirty)
}

func worktreeDirSize(path string) int64 {
	size, err := dirSize(path)
	if err != nil {
		return 0
	}
	return size
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// HasUnpushedCommits reports whether taskID's own worktree holds commits not
// on origin. Used by sandbox cleanup (a separate per-task data dir from the
// git worktree) so it never reaps a sandbox tied to a task whose worktree
// still holds completed-but-unpushed work (#2593). Returns false — nothing to
// protect — when the worktree cannot be resolved, or when it is externally
// adopted (owned by the tool that created it, not Sybra).
func (m *Manager) HasUnpushedCommits(ctx context.Context, taskID string) bool {
	wtPath, ok := m.resolveWorktreePath(taskID)
	if !ok {
		return false
	}
	return project.HasUnpushedCommits(ctx, wtPath)
}

// resolveWorktreePath returns the Sybra-managed worktree path for taskID. For
// a live task it uses DirName(); for a deleted task (record gone) it falls
// back to scanning the worktrees dir for a directory whose embedded task ID
// matches — the deleted-task case is exactly what the unpushed-commits guard
// must protect, and a live record no longer exists to compute DirName() from
// (#2593). Externally-adopted worktrees (WorktreeDir set) are never resolved:
// their lifecycle is owned by the tool that created them, and they live
// outside m.dir so the directory scan never surfaces them either.
func (m *Manager) resolveWorktreePath(taskID string) (string, bool) {
	if t, err := m.tasks.Get(taskID); err == nil {
		if t.ProjectID == "" || t.WorktreeDir != "" {
			return "", false
		}
		wtPath := filepath.Join(m.dir, t.DirName())
		if _, err := os.Stat(wtPath); err != nil {
			return "", false
		}
		return wtPath, true
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if taskIDFromWorktreeDir(e.Name()) == taskID {
			return filepath.Join(m.dir, e.Name()), true
		}
	}
	return "", false
}

// List returns all git worktrees for the given project.
func (m *Manager) List(ctx context.Context, projectID string) ([]project.Worktree, error) {
	proj, err := m.projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	return project.ListWorktrees(ctx, proj.ClonePath)
}

// RepairAll runs `git worktree repair` against every project's bare clone.
// Designed for boot-time invocation: a container redeploy that moves the
// in-container mount point of the bare clone leaves every worktree with a
// stale absolute back-pointer. `git worktree repair` rewrites both sides of
// the pointer pair and is a no-op when paths are already correct.
func (m *Manager) RepairAll(ctx context.Context) {
	if m.projects == nil {
		return
	}
	projects, err := m.projects.List()
	if err != nil {
		m.logger.Warn("worktree.repair-all.list", "err", err)
		return
	}
	for i := range projects {
		if err := project.RepairWorktrees(ctx, projects[i].ClonePath); err != nil {
			m.logger.Warn("worktree.repair-all", "project", projects[i].ID, "err", err)
			continue
		}
		m.logger.Info("worktree.repair-all", "project", projects[i].ID)
	}
}

// refuseRecreateWithLocalWork stops healOrRecreate from wiping a worktree
// that still holds commits origin has never seen or uncommitted filesystem
// state.
//
// Remove and CleanupOrphaned already refuse in that case (#2593), but the
// branch-mismatch recreate path deleted unconditionally. A worktree can be
// healthy yet on an unexpected branch — precisely the state a merge or
// conflict resolution leaves — and wiping it discards work an agent already
// did and verified, with the only record being the original push failure.
//
// Failing loudly is the point: the task surfaces a real error a human can act
// on while the local state still exists.
func (m *Manager) refuseRecreateWithLocalWork(ctx context.Context, taskID, wtPath, reason string) error {
	if project.HasUnpushedCommits(ctx, wtPath) {
		m.logger.Error("worktree.recreate.unpushed-commits", "task_id", taskID, "path", wtPath, "reason", reason)
		return fmt.Errorf("refusing to recreate %s worktree %s: it holds commits that never reached a remote", reason, wtPath)
	}
	dirty, err := project.IsWorktreeDirty(ctx, wtPath)
	if err != nil {
		m.logger.Error("worktree.recreate.inspect-local-work", "task_id", taskID, "path", wtPath, "reason", reason, "err", err)
		return fmt.Errorf("refusing to recreate %s worktree %s: cannot verify that its local state is clean: %w", reason, wtPath, err)
	}
	if dirty {
		m.logger.Error("worktree.recreate.uncommitted-work", "task_id", taskID, "path", wtPath, "reason", reason)
		return fmt.Errorf("refusing to recreate %s worktree %s: it holds uncommitted work", reason, wtPath)
	}
	return nil
}

// healOrRecreate ensures the worktree at wtPath has resolvable git metadata
// and sits on wantBranch. Returns (true, nil) if the worktree is usable on
// return, (false, nil) if it was wiped and the caller should re-create it, or
// (_, err) on a hard error.
func (m *Manager) healOrRecreate(ctx context.Context, taskID, clonePath, wtPath, wantBranch string) (bool, error) {
	healthy := project.WorktreeHealthy(ctx, wtPath)
	onBranch := onExpectedBranch(ctx, wtPath, wantBranch)
	if healthy && onBranch {
		return true, nil
	}
	if healthy {
		if err := m.refuseRecreateWithLocalWork(ctx, taskID, wtPath, "branch-mismatch"); err != nil {
			return false, err
		}
		m.logger.Warn("worktree.branch-mismatch-recreate", "task_id", taskID, "path", wtPath, "branch", wantBranch)
		_ = project.RemoveWorktree(ctx, clonePath, wtPath)
		if err := os.RemoveAll(wtPath); err != nil {
			return false, fmt.Errorf("remove mismatched-branch worktree %s: %w", wtPath, err)
		}
		_ = project.PruneWorktrees(ctx, clonePath)
		return false, nil
	}
	m.logger.Warn("worktree.unhealthy", "task_id", taskID, "path", wtPath)
	if err := project.RepairWorktrees(ctx, clonePath); err != nil {
		m.logger.Warn("worktree.repair", "task_id", taskID, "err", err)
	}
	if project.WorktreeHealthy(ctx, wtPath) && onExpectedBranch(ctx, wtPath, wantBranch) {
		m.logger.Info("worktree.repaired", "task_id", taskID, "path", wtPath)
		return true, nil
	}
	// Broken linked-worktree metadata makes git status and reachability
	// unreadable. Preserve the entire checkout before recreating: unreadable is
	// not evidence that its filesystem contents are disposable.
	quarantined, err := m.quarantineWorktreeDir(wtPath)
	if err != nil {
		return false, fmt.Errorf("quarantine unhealthy worktree %s: %w", wtPath, err)
	}
	m.logger.Warn("worktree.unrepairable-recreate", "task_id", taskID, "path", wtPath, "quarantine", quarantined)
	_ = project.RemoveWorktree(ctx, clonePath, wtPath)
	_ = project.PruneWorktrees(ctx, clonePath)
	return false, nil
}

func (m *Manager) quarantineWorktreeDir(wtPath string) (string, error) {
	root := filepath.Join(m.dir, worktreeQuarantineDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	slot, err := os.MkdirTemp(root, filepath.Base(wtPath)+"-")
	if err != nil {
		return "", err
	}
	destination := filepath.Join(slot, "worktree")
	if err := os.Rename(wtPath, destination); err != nil {
		_ = os.Remove(slot)
		return "", err
	}
	return destination, nil
}

// onExpectedBranch reports whether wtPath is currently checked out on
// wantBranch. A reused worktree directory can be left on a leftover HEAD from
// a prior run or aborted rebase (e.g. a detached HEAD from an interrupted
// operation) while still passing WorktreeHealthy — reusing it as-is would let
// a stale HEAD get captured downstream as the tamper-detection baseline,
// producing a diff range that spans unrelated history instead of the current
// task's actual change.
func onExpectedBranch(ctx context.Context, wtPath, wantBranch string) bool {
	current, err := project.CurrentBranch(ctx, wtPath)
	if err != nil {
		return false
	}
	return current == wantBranch
}
