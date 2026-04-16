package worktree

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
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
	base := filepath.Clean(m.dir)
	rel, err := filepath.Rel(base, clean)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path not within worktrees directory")
	}
	info, err := os.Stat(clean)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a valid directory")
	}
	return nil
}

// callPhase invokes fn with phase if fn is non-nil. Nil-safe.
func callPhase(fn func(string), phase string) {
	if fn != nil {
		fn(phase)
	}
}

// PrepareForTask creates (or reuses) a worktree for implementation work.
// Fetches origin, creates branch sybra/{dirName} off default branch,
// pushes upstream, and sets task.Branch.
// onPhase is an optional callback that receives human-readable phase labels
// as work progresses; pass nil when phase reporting is not needed.
func (m *Manager) PrepareForTask(t task.Task, onPhase func(string)) (string, error) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	callPhase(onPhase, "Fetching origin…")
	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}

	branch, err := project.DefaultBranch(proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	wtPath := m.PathFor(t)
	wtBranch := "sybra/" + t.DirName()
	baseRef := "refs/remotes/origin/" + branch

	if _, statErr := os.Stat(wtPath); statErr == nil {
		callPhase(onPhase, "Checking worktree…")
		usable, err := m.healOrRecreate(t.ID, proj.ClonePath, wtPath)
		if err != nil {
			return "", err
		}
		if usable {
			if err := project.SanitizeWorktree(wtPath); err != nil {
				m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
			}
			// Rebase is best-effort — conflicts with main shouldn't block agent
			// start on a branch that already has committed work.
			callPhase(onPhase, "Rebasing onto origin…")
			if err := project.RebaseOnto(wtPath, baseRef); err != nil {
				m.logger.Warn("worktree.rebase-skipped", "task_id", t.ID, "base", baseRef, "err", err)
			} else {
				m.logger.Info("worktree.rebased", "task_id", t.ID, "path", wtPath, "base", baseRef)
				// Sync remote after rebase — local SHAs changed, remote still has
				// old commits. create_pr push would fail with "diverged" otherwise.
				callPhase(onPhase, "Pushing upstream…")
				if err := project.PushForce(wtPath, wtBranch); err != nil {
					m.logger.Warn("worktree.push-force", "task_id", t.ID, "branch", wtBranch, "err", err)
				}
			}
			m.installChecks(wtPath, proj)
			m.ensureBranch(t, wtBranch)
			return wtPath, nil
		}
		// Worktree was wiped — fall through to create paths below.
	}

	// Branch may survive a prior worktree removal — check out existing branch
	// and rebase onto base instead of failing with "branch already exists".
	if project.BranchExists(proj.ClonePath, wtBranch) {
		callPhase(onPhase, "Creating worktree…")
		if err := project.CreateWorktreeExisting(proj.ClonePath, wtPath, wtBranch); err != nil {
			return "", fmt.Errorf("checkout existing branch %s: %w", wtBranch, err)
		}
		if err := project.SanitizeWorktree(wtPath); err != nil {
			m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
		}
		callPhase(onPhase, "Rebasing onto origin…")
		if err := project.RebaseOnto(wtPath, baseRef); err != nil {
			m.logger.Warn("worktree.rebase-skipped", "task_id", t.ID, "base", baseRef, "err", err)
		} else {
			// Sync remote after rebase.
			callPhase(onPhase, "Pushing upstream…")
			if err := project.PushForce(wtPath, wtBranch); err != nil {
				m.logger.Warn("worktree.push-force", "task_id", t.ID, "branch", wtBranch, "err", err)
			}
		}
		m.logger.Info("worktree.reused-branch", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		m.runSetup(wtPath, proj.SetupCommands)
		m.installChecks(wtPath, proj)
		m.ensureBranch(t, wtBranch)
		return wtPath, nil
	}

	callPhase(onPhase, "Creating worktree…")
	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	m.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)
	m.runSetup(wtPath, proj.SetupCommands)
	m.installChecks(wtPath, proj)

	callPhase(onPhase, "Pushing upstream…")
	if err := project.PushUpstream(wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.push-upstream", "task_id", t.ID, "branch", wtBranch, "err", err)
	}

	m.ensureBranch(t, wtBranch)
	return wtPath, nil
}

// PrepareForChat creates a worktree for an ephemeral chat session. Same as
// PrepareForTask but skips the upstream push — chat branches are local-only
// and deleted with the worktree when the chat ends.
// onPhase is an optional callback for phase labels; pass nil when not needed.
func (m *Manager) PrepareForChat(t task.Task, onPhase func(string)) (string, error) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	callPhase(onPhase, "Fetching origin…")
	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}

	branch, err := project.DefaultBranch(proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	wtPath := m.PathFor(t)
	wtBranch := "sybra/" + t.DirName()
	baseRef := "refs/remotes/origin/" + branch

	if _, statErr := os.Stat(wtPath); statErr == nil {
		usable, err := m.healOrRecreate(t.ID, proj.ClonePath, wtPath)
		if err != nil {
			return "", err
		}
		if usable {
			m.logger.Info("chat.worktree.reused", "task_id", t.ID, "path", wtPath)
			m.ensureBranch(t, wtBranch)
			return wtPath, nil
		}
		// Worktree was wiped — fall through.
	}

	callPhase(onPhase, "Creating worktree…")
	if project.BranchExists(proj.ClonePath, wtBranch) {
		if err := project.CreateWorktreeExisting(proj.ClonePath, wtPath, wtBranch); err != nil {
			return "", fmt.Errorf("checkout existing branch %s: %w", wtBranch, err)
		}
		m.logger.Info("chat.worktree.reused-branch", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		m.runSetup(wtPath, proj.SetupCommands)
		m.ensureBranch(t, wtBranch)
		return wtPath, nil
	}

	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create chat worktree: %w", err)
	}
	m.logger.Info("chat.worktree.created", "task_id", t.ID, "path", wtPath)
	m.runSetup(wtPath, proj.SetupCommands)
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
	m.runSetup(wtPath, proj.SetupCommands)
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
	m.runSetup(wtPath, proj.SetupCommands)
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

// runSetup executes a project's setup commands inside the worktree directory.
// Failures are logged but do not block worktree preparation.
func (m *Manager) runSetup(wtPath string, commands []string) {
	for _, raw := range commands {
		cmd := exec.Command("sh", "-c", raw)
		cmd.Dir = wtPath
		out, err := cmd.CombinedOutput()
		if err != nil {
			m.logger.Warn("worktree.setup-cmd", "cmd", raw, "path", wtPath, "err", err, "output", string(out))
			continue
		}
		m.logger.Info("worktree.setup-cmd", "cmd", raw, "path", wtPath)
	}
}

func (m *Manager) installChecks(wtPath string, proj project.Project) {
	repoCfg, err := project.LoadRepoConfig(wtPath)
	if err != nil {
		m.logger.Warn("worktree.repo-config", "path", wtPath, "err", err)
	}
	var repoChecks *project.ChecksConfig
	if repoCfg != nil {
		repoChecks = repoCfg.Checks
	}
	checks := project.MergeChecks(repoChecks, proj.Checks)
	if err := project.InstallHooks(wtPath, checks); err != nil {
		m.logger.Warn("worktree.hooks", "path", wtPath, "err", err)
	}
}

func (m *Manager) ensureBranch(t task.Task, branch string) {
	if t.Branch != "" {
		return
	}
	if _, err := m.tasks.Update(t.ID, task.Update{Branch: task.Ptr(branch)}); err != nil {
		m.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
	}
}
