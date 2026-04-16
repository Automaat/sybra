package worktree

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// PRBranchResolver fetches the head branch name for a PR.
// Injected to avoid importing internal/github.
type PRBranchResolver func(repo string, prNumber int) (string, error)

// AgentChecker reports whether a task has a running agent.
// Injected to avoid importing internal/agent.
type AgentChecker func(taskID string) bool

// defaultSetupTimeout caps the entire SetupCommands batch per worktree.
// Accommodates cold `mise install` / `npm ci` runs on first use of a project
// but prevents stuck commands from blocking worktree creation forever.
const defaultSetupTimeout = 5 * time.Minute

type Config struct {
	WorktreesDir     string
	Projects         *project.Store
	Tasks            *task.Manager
	Logger           *slog.Logger
	LogsDir          string
	SetupTimeout     time.Duration
	PRBranchResolver PRBranchResolver
	AgentChecker     AgentChecker
}

type Manager struct {
	dir          string
	projects     *project.Store
	tasks        *task.Manager
	logger       *slog.Logger
	logsDir      string
	setupTimeout time.Duration
	prBranch     PRBranchResolver
	hasAgent     AgentChecker
}

func New(cfg Config) *Manager {
	timeout := cfg.SetupTimeout
	if timeout <= 0 {
		timeout = defaultSetupTimeout
	}
	return &Manager{
		dir:          cfg.WorktreesDir,
		projects:     cfg.Projects,
		tasks:        cfg.Tasks,
		logger:       cfg.Logger,
		logsDir:      cfg.LogsDir,
		setupTimeout: timeout,
		prBranch:     cfg.PRBranchResolver,
		hasAgent:     cfg.AgentChecker,
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
		callPhase(onPhase, "Running setup…")
		if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
			return "", fmt.Errorf("setup on reused branch: %w", err)
		}
		m.installChecks(wtPath, proj)
		m.ensureBranch(t, wtBranch)
		return wtPath, nil
	}

	callPhase(onPhase, "Creating worktree…")
	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	m.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)
	callPhase(onPhase, "Running setup…")
	if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("setup on new worktree: %w", err)
	}
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
		if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
			return "", fmt.Errorf("chat setup on reused branch: %w", err)
		}
		m.ensureBranch(t, wtBranch)
		return wtPath, nil
	}

	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create chat worktree: %w", err)
	}
	m.logger.Info("chat.worktree.created", "task_id", t.ID, "path", wtPath)
	if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("chat setup on new worktree: %w", err)
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
	if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("review setup: %w", err)
	}
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
	if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("fix setup: %w", err)
	}
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
// Every command runs via `sh -c` in wtPath with a shared batch timeout. All
// stdout/stderr is streamed to a per-task log file so agents (and operators)
// can inspect bootstrap failures without digging through the global log.
//
// Returns an error on the first non-zero exit or on timeout. Callers must
// treat setup failure as blocking: an agent started on a worktree with a
// broken toolchain will burn tokens hitting missing-tool errors.
func (m *Manager) runSetup(taskID, wtPath string, commands []string) error {
	if len(commands) == 0 {
		return nil
	}

	logPath := m.setupLogPath(taskID)
	var logFile *os.File
	if logPath != "" {
		f, logErr := m.openSetupLog(logPath)
		if logErr != nil {
			// Missing log dir is not fatal — fall back to slog only. Agents can
			// still run; operators debug via sybra.log.
			m.logger.Warn("worktree.setup-log-open", "task_id", taskID, "path", logPath, "err", logErr)
		} else {
			logFile = f
		}
	}
	defer func() {
		if logFile != nil {
			_ = logFile.Close()
		}
	}()

	writeLog := func(s string) {
		if logFile == nil {
			return
		}
		_, _ = logFile.WriteString(s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.setupTimeout)
	defer cancel()

	writeLog(fmt.Sprintf(
		"=== worktree setup: task=%s path=%s started_at=%s timeout=%s commands=%d ===\n",
		taskID, wtPath, time.Now().UTC().Format(time.RFC3339), m.setupTimeout, len(commands),
	))

	for i, raw := range commands {
		if err := ctx.Err(); err != nil {
			writeLog(fmt.Sprintf("\n!!! timeout before command %d (%s): %v\n", i+1, raw, err))
			m.logger.Error("worktree.setup-timeout",
				"task_id", taskID, "path", wtPath, "cmd", raw, "err", err,
				"log", logPath)
			return fmt.Errorf("setup timeout before command %q: %w", raw, err)
		}

		started := time.Now()
		writeLog(fmt.Sprintf("\n--- [%d/%d] %s\n$ %s\n", i+1, len(commands), started.UTC().Format(time.RFC3339), raw))

		cmd := exec.CommandContext(ctx, "sh", "-c", raw)
		cmd.Dir = wtPath
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}

		m.logger.Info("worktree.setup-start",
			"task_id", taskID, "path", wtPath, "cmd", raw, "index", i+1, "total", len(commands))

		err := cmd.Run()
		dur := time.Since(started)

		if err != nil {
			writeLog(fmt.Sprintf("\n!!! exit err=%v duration=%s\n", err, dur))
			m.logger.Error("worktree.setup-fail",
				"task_id", taskID, "path", wtPath, "cmd", raw,
				"index", i+1, "total", len(commands),
				"duration", dur, "err", err, "log", logPath)
			return fmt.Errorf("setup command %q failed after %s: %w (see %s)", raw, dur, err, logPath)
		}

		writeLog(fmt.Sprintf("\n<<< ok duration=%s\n", dur))
		m.logger.Info("worktree.setup-ok",
			"task_id", taskID, "path", wtPath, "cmd", raw,
			"index", i+1, "total", len(commands), "duration", dur)
	}

	writeLog(fmt.Sprintf("\n=== worktree setup: task=%s completed_at=%s ===\n",
		taskID, time.Now().UTC().Format(time.RFC3339)))
	m.logger.Info("worktree.setup-complete",
		"task_id", taskID, "path", wtPath, "commands", len(commands), "log", logPath)
	return nil
}

// setupLogPath returns the per-task setup log file path. Empty logsDir
// disables file logging (returns ""), keeping in-memory/test setups working
// without needing to configure a log dir.
func (m *Manager) setupLogPath(taskID string) string {
	if m.logsDir == "" {
		return ""
	}
	return filepath.Join(m.logsDir, "worktrees", taskID+"-setup.log")
}

func (m *Manager) openSetupLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	// Truncate: each worktree prep starts fresh, old log contents are stale.
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

// resolveSetupCommands loads the worktree's .sybra.yaml (if present) and
// merges its `setup:` block with the project's app-level SetupCommands.
// Repo commands run first (canonical toolchain bootstrap), app commands
// second (per-machine additions). A missing or unparseable .sybra.yaml
// falls back to app commands only — logged but non-fatal so an agent can
// still start on a checkout without the file.
func (m *Manager) resolveSetupCommands(wtPath string, proj project.Project) []string {
	repoCfg, err := project.LoadRepoConfig(wtPath)
	if err != nil {
		m.logger.Warn("worktree.repo-config-setup",
			"path", wtPath, "project", proj.ID, "err", err)
		return proj.SetupCommands
	}
	var repoSetup []string
	if repoCfg != nil {
		repoSetup = repoCfg.Setup
	}
	merged := project.MergeSetup(repoSetup, proj.SetupCommands)
	if len(merged) > 0 {
		m.logger.Info("worktree.setup-resolved",
			"path", wtPath, "project", proj.ID,
			"repo_cmds", len(repoSetup), "app_cmds", len(proj.SetupCommands),
			"total", len(merged))
	}
	return merged
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
