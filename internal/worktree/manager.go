package worktree

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
)

// PRBranchResolver fetches the head branch name for a PR.
// Injected to avoid importing internal/github.
type PRBranchResolver func(repo string, prNumber int) (string, error)

// AgentChecker reports whether a task has a running agent.
// Injected to avoid importing internal/agent.
type AgentChecker func(taskID string) bool

type Config struct {
	WorktreesDir     string
	Projects         *project.Store
	Tasks            *task.Manager
	Logger           *slog.Logger
	PRBranchResolver PRBranchResolver
	AgentChecker     AgentChecker
}

type Manager struct {
	dir      string
	projects *project.Store
	tasks    *task.Manager
	logger   *slog.Logger
	prBranch PRBranchResolver
	hasAgent AgentChecker
}

func New(cfg Config) *Manager {
	return &Manager{
		dir:      cfg.WorktreesDir,
		projects: cfg.Projects,
		tasks:    cfg.Tasks,
		logger:   cfg.Logger,
		prBranch: cfg.PRBranchResolver,
		hasAgent: cfg.AgentChecker,
	}
}

// Dir returns the base worktrees directory.
func (m *Manager) Dir() string { return m.dir }

// PathFor returns the worktree path for a task.
func (m *Manager) PathFor(t task.Task) string {
	return filepath.Join(m.dir, t.DirName())
}

// Exists reports whether the worktree directory exists for a task.
func (m *Manager) Exists(t task.Task) bool {
	_, err := os.Stat(m.PathFor(t))
	return err == nil
}

// ValidatePath checks that path is within the worktrees directory and is a directory.
func (m *Manager) ValidatePath(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(m.dir)) {
		return fmt.Errorf("path not within worktrees directory")
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a valid directory")
	}
	return nil
}

// PrepareForTask creates (or reuses) a worktree for implementation work.
// Fetches origin, creates branch synapse/{dirName} off default branch,
// pushes upstream, and sets task.Branch.
func (m *Manager) PrepareForTask(t task.Task) (string, error) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}

	branch, err := project.DefaultBranch(proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	wtPath := m.PathFor(t)
	wtBranch := "synapse/" + t.DirName()
	baseRef := "refs/remotes/origin/" + branch

	if _, statErr := os.Stat(wtPath); statErr == nil {
		if err := project.SanitizeWorktree(wtPath); err != nil {
			m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
		}
		// Rebase is best-effort — conflicts with main shouldn't block agent
		// start on a branch that already has committed work.
		if err := project.RebaseOnto(wtPath, baseRef); err != nil {
			m.logger.Warn("worktree.rebase-skipped", "task_id", t.ID, "base", baseRef, "err", err)
		} else {
			m.logger.Info("worktree.rebased", "task_id", t.ID, "path", wtPath, "base", baseRef)
		}
		m.ensureBranch(t, wtBranch)
		return wtPath, nil
	}

	// Branch may survive a prior worktree removal — check out existing branch
	// and rebase onto base instead of failing with "branch already exists".
	if project.BranchExists(proj.ClonePath, wtBranch) {
		if err := project.CreateWorktreeExisting(proj.ClonePath, wtPath, wtBranch); err != nil {
			return "", fmt.Errorf("checkout existing branch %s: %w", wtBranch, err)
		}
		if err := project.SanitizeWorktree(wtPath); err != nil {
			m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
		}
		if err := project.RebaseOnto(wtPath, baseRef); err != nil {
			m.logger.Warn("worktree.rebase-skipped", "task_id", t.ID, "base", baseRef, "err", err)
		}
		m.logger.Info("worktree.reused-branch", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		m.ensureBranch(t, wtBranch)
		return wtPath, nil
	}

	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	m.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)

	if err := project.PushUpstream(wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.push-upstream", "task_id", t.ID, "branch", wtBranch, "err", err)
	}

	m.ensureBranch(t, wtBranch)
	return wtPath, nil
}

// PrepareForReview creates a detached-HEAD worktree for read-only PR review.
func (m *Manager) PrepareForReview(t task.Task) (string, error) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		m.logger.Warn("review.worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := m.prBranch(t.ProjectID, t.PRNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr branch: %w", err)
	}

	wtPath := m.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr == nil {
		return wtPath, nil
	}

	ref := "refs/remotes/origin/" + branch
	if err := project.CreateWorktreeDetached(proj.ClonePath, wtPath, ref); err != nil {
		return "", fmt.Errorf("create review worktree: %w", err)
	}
	m.logger.Info("review.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	return wtPath, nil
}

// PrepareForFix creates a worktree checking out the PR's head branch
// so the agent can rebase and push.
func (m *Manager) PrepareForFix(t task.Task, prNumber int) (string, error) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		m.logger.Warn("fix.worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := m.prBranch(t.ProjectID, prNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr branch: %w", err)
	}

	wtPath := m.PathFor(t)

	// Remove stale worktree — previous agent may have left dirty state.
	if _, statErr := os.Stat(wtPath); statErr == nil {
		_ = project.RemoveWorktree(proj.ClonePath, wtPath)
	}

	ref := "refs/remotes/origin/" + branch
	if err := project.CreateWorktreeExisting(proj.ClonePath, wtPath, ref); err != nil {
		return "", fmt.Errorf("create fix worktree: %w", err)
	}
	if err := project.SanitizeWorktree(wtPath); err != nil {
		m.logger.Warn("fix.worktree.sanitize", "task_id", t.ID, "err", err)
	}
	m.logger.Info("fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	return wtPath, nil
}

// Remove cleans up the worktree for a task via git worktree remove.
func (m *Manager) Remove(taskID string) {
	t, err := m.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
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

func (m *Manager) ensureBranch(t task.Task, branch string) {
	if t.Branch != "" {
		return
	}
	if _, err := m.tasks.Update(t.ID, map[string]any{"branch": branch}); err != nil {
		m.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
	}
}
