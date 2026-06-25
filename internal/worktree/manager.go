package worktree

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	// MisePath is the path to the mise binary. Defaults to "mise" (PATH lookup).
	// Tests inject a concrete path so parallel tests don't race on os.Setenv(PATH).
	MisePath string
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
	misePath     string
}

func New(cfg Config) *Manager {
	timeout := cfg.SetupTimeout
	if timeout <= 0 {
		timeout = defaultSetupTimeout
	}
	mp := cfg.MisePath
	// Accept bare name "mise" or absolute paths only; reject relative paths.
	if mp == "" || (mp != "mise" && !filepath.IsAbs(mp)) {
		mp = "mise"
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
		misePath:     mp,
	}
}

// Dir returns the base worktrees directory.
func (m *Manager) Dir() string { return m.dir }

// PathFor returns the worktree path for a task. A task with an explicit
// WorktreeDir (an externally-adopted worktree, e.g. created by Orca) resolves
// to that path; otherwise the path is derived under the worktrees directory.
func (m *Manager) PathFor(t task.Task) string {
	if t.WorktreeDir != "" {
		return t.WorktreeDir
	}
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

func (m *Manager) ensureBranch(t task.Task, branch string) {
	if t.Branch != "" {
		return
	}
	if _, err := m.tasks.Update(t.ID, task.Update{Branch: task.Ptr(branch)}); err != nil {
		m.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
	}
}

func (m *Manager) logPushSync(taskID, branch string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, project.ErrBranchMissing) {
		m.logger.Info("worktree.push-sync-skipped", "task_id", taskID, "branch", branch, "reason", "local branch ref missing")
	} else {
		m.logger.Warn("worktree.push-sync", "task_id", taskID, "branch", branch, "err", err)
	}
}

// worktreeBaseRef returns the git ref used as the starting point for new
// worktree branches. "head" uses the local branch tip (picks up unpushed
// commits); anything else (including "fresh" and the zero value) uses the
// remote tracking ref so the worktree always starts from pushed state.
func worktreeBaseRef(setting, branch string) string {
	if setting == project.WorktreeBaseRefHead {
		return "refs/heads/" + branch
	}
	return "refs/remotes/origin/" + branch
}
