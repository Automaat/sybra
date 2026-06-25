package worktree

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Remove cleans up the worktree for a task via git worktree remove.
func (m *Manager) Remove(taskID string) {
	t, err := m.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return
	}
	// Never touch an externally-adopted worktree: the tool that created it
	// (e.g. Orca) owns its lifecycle. Removing it would delete the user's
	// checkout out from under them.
	if t.WorktreeDir != "" {
		return
	}
	wtPath := filepath.Join(m.dir, t.DirName())
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return
	}
	if err := project.RemoveWorktree(proj.ClonePath, wtPath); err != nil {
		m.logger.Error("worktree.cleanup", "path", wtPath, "err", err)
	} else {
		m.logger.Info("worktree.cleaned", "path", wtPath)
	}
}

// CleanupOrphaned removes worktree directories for deleted or completed tasks
// that have no running agent.
func (m *Manager) CleanupOrphaned() {
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

		removed := false
		if exists && t.ProjectID != "" {
			if proj, perr := m.projects.Get(t.ProjectID); perr == nil {
				if err := project.RemoveWorktree(proj.ClonePath, wtPath); err != nil {
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
		if err := project.PruneWorktrees(projects[i].ClonePath); err != nil {
			m.logger.Warn("worktree.prune", "project", projects[i].ID, "err", err)
		}
	}
}

// List returns all git worktrees for the given project.
func (m *Manager) List(projectID string) ([]project.Worktree, error) {
	proj, err := m.projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	return project.ListWorktrees(proj.ClonePath)
}

// RepairAll runs `git worktree repair` against every project's bare clone.
// Designed for boot-time invocation: a container redeploy that moves the
// in-container mount point of the bare clone leaves every worktree with a
// stale absolute back-pointer. `git worktree repair` rewrites both sides of
// the pointer pair and is a no-op when paths are already correct.
func (m *Manager) RepairAll() {
	if m.projects == nil {
		return
	}
	projects, err := m.projects.List()
	if err != nil {
		m.logger.Warn("worktree.repair-all.list", "err", err)
		return
	}
	for i := range projects {
		if err := project.RepairWorktrees(projects[i].ClonePath); err != nil {
			m.logger.Warn("worktree.repair-all", "project", projects[i].ID, "err", err)
			continue
		}
		m.logger.Info("worktree.repair-all", "project", projects[i].ID)
	}
}

// healOrRecreate ensures the worktree at wtPath has resolvable git metadata.
// Returns (true, nil) if the worktree is usable on return, (false, nil) if it
// was wiped and the caller should re-create it, or (_, err) on a hard error.
func (m *Manager) healOrRecreate(taskID, clonePath, wtPath string) (bool, error) {
	if project.WorktreeHealthy(wtPath) {
		return true, nil
	}
	m.logger.Warn("worktree.unhealthy", "task_id", taskID, "path", wtPath)
	if err := project.RepairWorktrees(clonePath); err != nil {
		m.logger.Warn("worktree.repair", "task_id", taskID, "err", err)
	}
	if project.WorktreeHealthy(wtPath) {
		m.logger.Info("worktree.repaired", "task_id", taskID, "path", wtPath)
		return true, nil
	}
	m.logger.Warn("worktree.unrepairable-recreate", "task_id", taskID, "path", wtPath)
	_ = project.RemoveWorktree(clonePath, wtPath)
	if err := os.RemoveAll(wtPath); err != nil {
		return false, fmt.Errorf("remove unhealthy worktree %s: %w", wtPath, err)
	}
	_ = project.PruneWorktrees(clonePath)
	return false, nil
}
