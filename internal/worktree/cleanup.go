package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

var (
	bareTaskIDRe   = regexp.MustCompile(`^[0-9a-f]{8}$`)
	suffixTaskIDRe = regexp.MustCompile(`-([0-9a-f]{8})$`)
)

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

	for _, e := range entries {
		if !e.IsDir() {
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
			m.logger.Warn("worktree.orphan-cleanup.unpushed-commits", "path", wtPath)
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
		m.logger.Warn("worktree.branch-mismatch-recreate", "task_id", taskID, "path", wtPath, "branch", wantBranch)
		m.logger.Warn("worktree.unrepairable-recreate", "task_id", taskID, "path", wtPath)
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
	m.logger.Warn("worktree.unrepairable-recreate", "task_id", taskID, "path", wtPath)
	_ = project.RemoveWorktree(ctx, clonePath, wtPath)
	if err := os.RemoveAll(wtPath); err != nil {
		return false, fmt.Errorf("remove unhealthy worktree %s: %w", wtPath, err)
	}
	_ = project.PruneWorktrees(ctx, clonePath)
	return false, nil
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
