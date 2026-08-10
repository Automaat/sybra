package worktree

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/gitexec"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// PRBranchResolver fetches the head branch name for a PR.
// Injected to avoid importing internal/github.
type PRBranchResolver func(repo string, prNumber int) (string, error)

// AgentChecker reports whether a task has a running agent.
// Injected to avoid importing internal/agent.
type AgentChecker func(taskID string) bool

// CleanupGate proves that a fresh observed task/worktree snapshot contains no
// local-only work before a destructive removal. false fails closed.
type CleanupGate func(context.Context, task.Task, string) bool

// defaultSetupTimeout caps the entire SetupCommands batch per worktree.
// Accommodates cold `mise install` / `npm ci` runs on first use of a project
// but prevents stuck commands from blocking worktree creation forever.
const defaultSetupTimeout = 10 * time.Minute

type Config struct {
	WorktreesDir     string
	Projects         *project.Store
	Tasks            *task.Manager
	Logger           *slog.Logger
	LogsDir          string
	SetupTimeout     time.Duration
	PRBranchResolver PRBranchResolver
	AgentChecker     AgentChecker
	// LiveAgentChecker reports whether an already-registered agent process is
	// live for a task — unlike AgentChecker, it must NOT count an in-flight
	// dispatch claim as "running". PrepareForTask/SyncTaskBranch call it from
	// inside the very dispatch that holds that claim, so a claim-inclusive
	// check would always see its own claim and refuse to ever prepare.
	// Production must wire a claim-exclusive checker (for example
	// agent.Manager.HasLiveRegisteredAgentForTask). When unset, New falls back
	// to AgentChecker only for legacy/test harnesses that never hold an
	// in-flight dispatch claim while preparing the worktree.
	LiveAgentChecker AgentChecker
	// MisePath is the path to the mise binary. Defaults to "mise" (PATH lookup).
	// Tests inject a concrete path so parallel tests don't race on os.Setenv(PATH).
	MisePath string
	// ProtectedFindings persists protected-resource cleanup blockers so
	// repeated sweeps do not re-log the same unchanged finding forever.
	ProtectedFindings *cleanup.ProtectedStore
}

type Manager struct {
	dir              string
	projects         *project.Store
	tasks            *task.Manager
	logger           *slog.Logger
	logsDir          string
	setupTimeout     time.Duration
	prBranch         PRBranchResolver
	hasAgent         AgentChecker
	hasLiveAgentOnly AgentChecker
	misePath         string
	protected        *cleanup.ProtectedStore
	cleanupGate      CleanupGate
	// paths serializes mutating operations per worktree directory. Not a
	// Config field: it is process-internal state, and every Manager must have
	// its own (a shared one would let two Managers over different worktree
	// roots contend on identical relative paths).
	paths pathLocks
}

// SetCleanupGate late-binds the post-run reconciler after both the worktree
// manager and task/project stores exist.
func (m *Manager) SetCleanupGate(gate CleanupGate) { m.cleanupGate = gate }

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
	liveAgentChecker := cfg.LiveAgentChecker
	if liveAgentChecker == nil {
		liveAgentChecker = cfg.AgentChecker
	}
	return &Manager{
		dir:              cfg.WorktreesDir,
		projects:         cfg.Projects,
		tasks:            cfg.Tasks,
		logger:           cfg.Logger,
		logsDir:          cfg.LogsDir,
		setupTimeout:     timeout,
		prBranch:         cfg.PRBranchResolver,
		hasAgent:         cfg.AgentChecker,
		hasLiveAgentOnly: liveAgentChecker,
		misePath:         mp,
		protected:        cfg.ProtectedFindings,
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

// ErrWorktreeBusy reports that a worktree exists but an agent is still writing
// to it, so it must not be handed to a second one. Callers should retry rather
// than fall back to preparation, which would rebase the branch out from under
// the live run.
var ErrWorktreeBusy = errors.New("worktree has a live agent")

// ResolveExisting returns the task's worktree path if one is already checked
// out and usable as-is, without touching a single byte of it.
//
// Every Prepare* entry point rewrites what it returns: SanitizeWorktree
// auto-commits the dirty tree and then `reset --hard` / `clean -fd`s it,
// reconcileAndRebase moves it onto a fresher base, and PushSync publishes the
// result. That is right for an agent about to do work, and wrong for one sent
// to explain a failure — it hands the diagnosis a different tree on a
// different base, and a base-induced failure simply stops reproducing (#3073).
//
// "Usable" is narrower than "resolvable". A detached HEAD or an interrupted
// rebase passes `rev-parse --git-dir` but cannot be pushed from, and a
// recovery agent is under orders to fix, commit and push: its commit would
// land unreferenced and the next Prepare* would drop the verified fix. Those
// fall through to preparation. Merely being on an *unexpected* branch does
// not — that is a half-merged checkout, which is the state that failed.
func (m *Manager) ResolveExisting(ctx context.Context, t task.Task) (string, error) {
	if m == nil {
		return "", os.ErrNotExist
	}
	path := m.PathFor(t)
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	if !project.WorktreeHealthy(ctx, path) {
		return "", fmt.Errorf("resolve existing worktree %s: %w", path, os.ErrNotExist)
	}
	// Reuse skips PrepareForTask, and with it the only live-agent guard on this
	// path. Without this check a task parked mid-run hands its own worktree to
	// the recovery agent while the implementer is still committing into it.
	if m.hasLiveAgentOnly != nil && m.hasLiveAgentOnly(t.ID) {
		return "", fmt.Errorf("resolve existing worktree %s: %w", path, ErrWorktreeBusy)
	}
	if err := m.pushableCheckout(ctx, t, path); err != nil {
		return "", err
	}
	return path, nil
}

// inProgressMarkers are the per-worktree files git writes while an operation is
// half-applied. They live in the linked worktree's own git dir, not the common
// one, so `rev-parse --git-dir` is the right base.
var inProgressMarkers = []string{
	"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "sequencer",
}

// pushableCheckout rejects the states that pass WorktreeHealthy but leave an
// agent unable to publish its fix, or able to publish it somewhere catastrophic:
// a detached HEAD (PrepareForReview creates the same path detached), a
// half-applied rebase/merge/revert, and the project's default branch.
//
// The default-branch case is the dangerous one. The recovery mandate orders the
// agent to fix, commit and push, and origin's push URL is only neutered for fork
// remotes — so on an own-repo project this would put agent commits straight onto
// main with no PR. adoptWorktree already refuses it for the same reason, and
// like adoptWorktree this fails closed when the default branch cannot be read.
func (m *Manager) pushableCheckout(ctx context.Context, t task.Task, path string) error {
	branch, err := project.CurrentBranch(ctx, path)
	if err != nil {
		return fmt.Errorf("resolve existing worktree %s: %w", path, err)
	}
	if branch == "" {
		return fmt.Errorf("resolve existing worktree %s: detached HEAD: %w", path, os.ErrInvalid)
	}
	gitDir, err := gitexec.Output(ctx, gitexec.Options{Dir: path}, "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("resolve existing worktree %s: %w", path, err)
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	for _, marker := range inProgressMarkers {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			return fmt.Errorf("resolve existing worktree %s: %s in progress: %w", path, marker, os.ErrInvalid)
		}
	}
	return m.refuseDefaultBranch(ctx, t, path, branch)
}

func (m *Manager) refuseDefaultBranch(ctx context.Context, t task.Task, path, branch string) error {
	if m.projects == nil || strings.TrimSpace(t.ProjectID) == "" {
		return nil
	}
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return fmt.Errorf("resolve existing worktree %s: load project: %w", path, err)
	}
	def, err := project.DefaultBranch(ctx, proj.ClonePath)
	if err != nil {
		return fmt.Errorf("resolve existing worktree %s: cannot determine default branch to guard against: %w", path, err)
	}
	if branch == def {
		return fmt.Errorf("resolve existing worktree %s: checked out on default branch %q: %w", path, def, os.ErrInvalid)
	}
	return nil
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
	if t.Branch == branch {
		return
	}
	if _, err := m.tasks.UpdateBy(t.ID, "worktree.manager.record_branch", task.Update{Branch: task.Ptr(branch)}); err != nil {
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
