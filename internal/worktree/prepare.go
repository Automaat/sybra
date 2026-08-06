package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/prepstate"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

// ErrRebaseFailed indicates a reused task worktree could not be rebased onto
// the project base ref and must be repaired before another agent run. Alias
// of worktreeerr.ErrRebaseFailed so internal/workflow can classify it without
// importing internal/worktree (which would create an import cycle via
// internal/task -> internal/workflow).
var ErrRebaseFailed = worktreeerr.ErrRebaseFailed

// ErrTransientFetch indicates reconcileAndRebase's remote fetch/ls-remote
// step failed for a transient network reason rather than a genuine content
// conflict. Alias of worktreeerr.ErrTransientFetch for the same import-cycle
// reason as ErrRebaseFailed above.
var ErrTransientFetch = worktreeerr.ErrTransientFetch

// ErrAgentRunning indicates PrepareForTask refused to reuse (and rebase) a
// worktree that a tracked agent is still live in. Alias of
// worktreeerr.ErrAgentRunning for the same import-cycle reason as
// ErrRebaseFailed above.
var ErrAgentRunning = worktreeerr.ErrAgentRunning

// ErrTaskBranchMissing indicates a task's own branch does not exist locally
// or on origin, so PrepareForBranchFix cannot check it out for no-PR conflict
// recovery. Unlike PrepareForFix (which falls back to the PR head ref via
// refs/pull/<N>/head), there is no PR yet to fall back to — a missing branch
// here is a hard failure the caller must escalate to human-required rather
// than guess at.
var ErrTaskBranchMissing = errors.New("task branch does not exist locally or on origin")

// SyncResult classifies the outcome of a proactive branch sync
// (Manager.SyncTaskBranch). String values are stable — they are recorded as
// workflow step output and generic artifacts, so renaming a constant changes
// what's observable in task history.
type SyncResult string

const (
	// SyncSynced means the worktree branch moved (reconciled/rebased and/or
	// pushed) as a result of the sync.
	SyncSynced SyncResult = "synced"
	// SyncNoop means the branch was already up to date with base — nothing to do.
	SyncNoop SyncResult = "noop"
	// SyncConflict means reconciling or rebasing onto base hit a genuine content
	// conflict (ErrRebaseFailed) that requires human or PR-fix-agent recovery.
	SyncConflict SyncResult = "conflict"
	// SyncFailed means the sync could not complete for a reason other than a
	// content conflict (network, auth, push rejection, etc.) — transient, and
	// safe to retry on the next checkpoint.
	SyncFailed SyncResult = "failed"
	// SyncSkipped means there was nothing to sync: no worktree for the task, or
	// the worktree is externally adopted (not Sybra-managed).
	SyncSkipped SyncResult = "skipped"
)

func (r SyncResult) String() string { return string(r) }

// SyncTaskBranch reconciles a task's existing worktree branch with its own
// remote tracking branch, reusing the same no-proactive-base-merge policy as
// PrepareForTask's reused-worktree path (reconcileAndRebase). It is
// intentionally non-blocking: it never mutates task status, never escalates to
// human-required, and any error is returned alongside a SyncResult so the caller
// can log/record the outcome and continue the workflow regardless. It does not
// merge the project's default branch just to refresh a stale PR; base-branch
// merges are reserved for explicit conflict-resolution paths.
//
// Skips (SyncSkipped, nil) when there is no existing worktree for the task, or
// the worktree is externally adopted (t.WorktreeDir set) — adopted worktrees
// are not Sybra-managed and PrepareForTask never reconciles them either.
func (m *Manager) SyncTaskBranch(ctx context.Context, t task.Task) (SyncResult, error) {
	if t.WorktreeDir != "" {
		return SyncSkipped, nil
	}
	if !m.Exists(t) {
		return SyncSkipped, nil
	}
	// Another mutating operation owns this directory. This sync is
	// opportunistic (see the doc above), so skip rather than fail — the next
	// checkpoint retries once the directory is free.
	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		m.logger.Info("worktree.sync-branch.busy", "task_id", t.ID, "err", err)
		return SyncSkipped, nil
	}
	defer release()
	// A tracked agent is still live in this worktree: rebasing here would
	// corrupt its in-flight edits. This sync is opportunistic (never blocks
	// workflow advancement per the doc above), so skip rather than fail —
	// the next tick retries once the agent is idle.
	if m.hasLiveAgentOnly != nil && m.hasLiveAgentOnly(t.ID) {
		return SyncSkipped, nil
	}
	wtPath := m.PathFor(t)

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return SyncFailed, fmt.Errorf("sync branch: get project: %w", err)
	}
	if err := project.FetchOrigin(ctx, proj.ClonePath); err != nil {
		return SyncFailed, fmt.Errorf("sync branch: fetch origin: %w", err)
	}
	branch, err := project.DefaultBranch(ctx, proj.ClonePath)
	if err != nil {
		return SyncFailed, fmt.Errorf("sync branch: default branch: %w", err)
	}

	wtBranch := branchNameForTask(t)
	baseRef := worktreeBaseRef(proj.WorktreeBaseRef, branch)

	preHEAD, err := project.CurrentCommit(ctx, wtPath)
	if err != nil {
		return SyncFailed, fmt.Errorf("sync branch: resolve pre-sync HEAD: %w", err)
	}

	if err := m.reconcileAndRebase(ctx, wtPath, wtBranch, baseRef, nil); err != nil {
		if errors.Is(err, ErrRebaseFailed) {
			m.logger.Warn("worktree.sync-branch.conflict", "task_id", t.ID, "branch", wtBranch, "err", err)
			return SyncConflict, err
		}
		return SyncFailed, fmt.Errorf("sync branch: reconcile: %w", err)
	}

	pushErr := project.PushSync(ctx, wtPath, wtBranch)
	m.logPushSync(t.ID, wtBranch, pushErr)
	if pushErr != nil && !errors.Is(pushErr, project.ErrBranchMissing) {
		return SyncFailed, fmt.Errorf("sync branch: push: %w", pushErr)
	}

	postHEAD, err := project.CurrentCommit(ctx, wtPath)
	if err != nil {
		return SyncFailed, fmt.Errorf("sync branch: resolve post-sync HEAD: %w", err)
	}

	if preHEAD != postHEAD {
		m.logger.Info("worktree.sync-branch.synced", "task_id", t.ID, "branch", wtBranch, "base", baseRef)
		return SyncSynced, nil
	}
	m.logger.Info("worktree.sync-branch.noop", "task_id", t.ID, "branch", wtBranch, "base", baseRef)
	return SyncNoop, nil
}

// PrepareForTask creates (or reuses) a worktree for implementation work.
// Fetches origin, creates a conventional-prefixed branch off default branch,
// pushes upstream, and sets task.Branch.
// onPhase is an optional callback that receives human-readable phase labels
// as work progresses; pass nil when phase reporting is not needed.
func (m *Manager) PrepareForTask(ctx context.Context, t task.Task, onPhase func(string)) (string, error) {
	if t.Slug != "" {
		if err := task.ValidateSlug(t.Slug); err != nil {
			return "", fmt.Errorf("task slug: %w", err)
		}
	}

	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		return "", err
	}
	defer release()

	// Adopt an externally-created worktree (e.g. one Orca already checked out)
	// instead of creating a Sybra-managed one. Runs every agent in that
	// directory as-is — no fetch/add/rebase/push-create/cleanup. This both
	// honours the user's existing checkout and sidesteps git's "branch already
	// checked out" refusal that a second worktree on the same branch would hit.
	if t.WorktreeDir != "" {
		return m.adoptWorktree(ctx, t, onPhase)
	}

	// A tracked agent is still live for this task: reusing its worktree
	// below would rebase (or recreate) the branch out from under its
	// in-flight edits. This is the last line of defense against a
	// stale/incorrect "no agent running" read upstream (e.g. ResumeStalled)
	// triggering a rebase against a live run — the caller must retry once
	// idle, same as workflow.ErrDispatchInFlight.
	if m.hasLiveAgentOnly != nil && m.hasLiveAgentOnly(t.ID) {
		return "", fmt.Errorf("prepare worktree for reuse: %w", ErrAgentRunning)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	callPhase(onPhase, "Fetching origin…")
	if err := project.FetchOrigin(ctx, proj.ClonePath); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}

	branch, err := project.DefaultBranch(ctx, proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	if proj.WorktreeBaseRef == project.WorktreeBaseRefHead {
		if err := project.SyncLocalBranch(ctx, proj.ClonePath, branch); err != nil {
			m.logger.Warn("worktree.sync-local-branch", "task_id", t.ID, "branch", branch, "err", err)
		}
	}

	wtPath := m.PathFor(t)
	wtBranch := branchNameForTask(t)
	baseRef := worktreeBaseRef(proj.WorktreeBaseRef, branch)

	wtBranch = m.resolveTaskBranch(ctx, t, proj.ClonePath, wtPath, wtBranch)
	if path, reused, err := m.prepareExistingWorktree(ctx, t, proj, wtPath, wtBranch, baseRef, onPhase); err != nil {
		return "", err
	} else if reused {
		return path, nil
	}

	// Branch may survive a prior worktree removal — check out existing branch
	// and rebase onto base instead of failing with "branch already exists".
	if project.BranchExists(ctx, proj.ClonePath, wtBranch) {
		callPhase(onPhase, "Creating worktree…")
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, wtBranch); err != nil {
			return "", fmt.Errorf("checkout existing branch %s: %w", wtBranch, err)
		}
		if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
			m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
		}
		if err := m.reconcileAndRebase(ctx, wtPath, wtBranch, baseRef, onPhase); err != nil {
			return "", err
		}
		// Sync remote after rebase.
		callPhase(onPhase, "Syncing upstream…")
		m.logPushSync(t.ID, wtBranch, project.PushSync(ctx, wtPath, wtBranch))
		m.logger.Info("worktree.reused-branch", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		if err := m.runPrepareSetup(ctx, t.ID, wtPath, proj, "reused branch", onPhase); err != nil {
			return "", err
		}
		path, err := m.finalizeWorktree(ctx, t, wtPath, wtBranch, proj)
		m.recordPreparedState(ctx, t.ID, wtPath, wtBranch)
		return path, err
	}

	callPhase(onPhase, "Creating worktree…")
	if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	m.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)
	if err := m.runPrepareSetup(ctx, t.ID, wtPath, proj, "new worktree", onPhase); err != nil {
		return "", err
	}
	m.installChecks(ctx, wtPath, proj)

	callPhase(onPhase, "Pushing upstream…")
	if err := project.PushUpstream(ctx, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.push-upstream", "task_id", t.ID, "branch", wtBranch, "err", err)
	}

	path, err := m.finalizeWorktree(ctx, t, wtPath, wtBranch, proj)
	m.recordPreparedState(ctx, t.ID, wtPath, wtBranch)
	return path, err
}

func (m *Manager) prepareExistingWorktree(ctx context.Context, t task.Task, proj project.Project, wtPath, wtBranch, baseRef string, onPhase func(string)) (path string, reused bool, err error) {
	if _, err := os.Stat(wtPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat worktree %s: %w", wtPath, err)
	}
	callPhase(onPhase, "Checking worktree…")
	usable, err := m.healOrRecreate(ctx, t.ID, proj.ClonePath, wtPath, wtBranch)
	if err != nil {
		return "", false, err
	}
	if !usable {
		return "", false, nil
	}
	if reusable, err := prepstate.Reusable(ctx, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.prep-state-read", "task_id", t.ID, "path", wtPath, "branch", wtBranch, "err", err)
	} else if reusable {
		m.logger.Info("worktree.prep-state-reused", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		if err := m.runPrepareSetup(ctx, t.ID, wtPath, proj, "prepared worktree", onPhase); err != nil {
			return "", false, err
		}
		path, err := m.finalizeWorktree(ctx, t, wtPath, wtBranch, proj)
		return path, true, err
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
	}
	if err := m.reconcileAndRebase(ctx, wtPath, wtBranch, baseRef, onPhase); err != nil {
		return "", false, err
	}
	m.logger.Info("worktree.rebased", "task_id", t.ID, "path", wtPath, "base", baseRef)
	callPhase(onPhase, "Syncing upstream…")
	m.logPushSync(t.ID, wtBranch, project.PushSync(ctx, wtPath, wtBranch))
	if err := m.runPrepareSetup(ctx, t.ID, wtPath, proj, "reused worktree", onPhase); err != nil {
		return "", false, err
	}
	path, err = m.finalizeWorktree(ctx, t, wtPath, wtBranch, proj)
	m.recordPreparedState(ctx, t.ID, wtPath, wtBranch)
	return path, true, err
}

func (m *Manager) resolveTaskBranch(ctx context.Context, t task.Task, clonePath, wtPath, wtBranch string) string {
	other, ok := m.branchCollidesWithOtherWorktree(ctx, clonePath, wtBranch, wtPath)
	if !ok {
		return wtBranch
	}
	unique := branchPrefixForTask(t) + "/" + t.DirName()
	if unique == wtBranch {
		return wtBranch
	}
	m.logger.Warn("worktree.branch-collision.rederive",
		"task_id", t.ID, "stored", wtBranch, "other", other, "rederived", unique)
	if _, uErr := m.tasks.Update(t.ID, task.Update{Branch: task.Ptr(unique)}); uErr != nil {
		m.logger.Warn("worktree.branch-collision.persist", "task_id", t.ID, "err", uErr)
	}
	return unique
}

func (m *Manager) branchCollidesWithOtherWorktree(ctx context.Context, clonePath, branch, wtPath string) (string, bool) {
	if branch == "" {
		return "", false
	}
	wts, err := project.ListWorktrees(ctx, clonePath)
	if err != nil {
		m.logger.Warn("worktree.branch-collision.list", "clone", clonePath, "err", err)
		return "", false
	}
	want := filepath.Clean(wtPath)
	for _, w := range wts {
		if w.Branch == branch && filepath.Clean(w.Path) != want {
			return w.Path, true
		}
	}
	return "", false
}

func (m *Manager) runPrepareSetup(ctx context.Context, taskID, wtPath string, proj project.Project, label string, onPhase func(string)) error {
	callPhase(onPhase, "Running setup…")
	if err := m.runSetup(ctx, taskID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return fmt.Errorf("setup on %s: %w", label, err)
	}
	return nil
}

// reconcileAndRebase prepares a reused worktree branch for another agent run.
// It first adopts any commits pushed to the remote branch since this worktree's
// local ref was last updated (e.g. a review fix pushed from another
// clone/machine), so the rebase carries them forward instead of a later
// force-push dropping them. Unpushed local branches are then rebased onto the
// configured base so they stay linear before first publication. Already-pushed
// branches are deliberately not rebased or merged with base: Sybra must not add
// merge commits just to refresh a branch, and it must not rewrite published
// history. GitHub PR state and the dedicated conflict-recovery paths own base
// conflict resolution. The reconcile step performs a network fetch/ls-remote,
// and a transient connectivity blip there (SSH/DNS/timeout) looks identical to
// a genuine failure unless distinguished — so that case wraps ErrTransientFetch
// instead, which callers must never escalate to human-required.
func (m *Manager) reconcileAndRebase(ctx context.Context, wtPath, wtBranch, baseRef string, onPhase func(string)) error {
	callPhase(onPhase, "Reconciling with remote…")
	if err := project.ReconcileWithRemote(ctx, wtPath, wtBranch); err != nil {
		if project.IsTransientNetworkError(err) {
			return fmt.Errorf("%w: reconcile %s with remote: %w", ErrTransientFetch, wtBranch, err)
		}
		return fmt.Errorf("%w: reconcile %s with remote: %w", ErrRebaseFailed, wtBranch, err)
	}
	if project.BranchPushed(ctx, wtPath, wtBranch) {
		m.logger.Info("worktree.pushed-branch-skip-base-sync", "branch", wtBranch, "base", baseRef)
		return nil
	}
	callPhase(onPhase, fmt.Sprintf("Rebasing onto %s…", baseRef))
	if rebaseErr := project.RebaseOnto(ctx, wtPath, baseRef); rebaseErr != nil {
		return fmt.Errorf("%w: rebase %s onto %s: %w", ErrRebaseFailed, wtBranch, baseRef, rebaseErr)
	}
	return nil
}

// seedWorktree drops the per-worktree agent context into an implementation
// worktree: the identity beacon (writeContextFile) and the working-memory
// scratchpad (ensureNotesFile). Both are git-excluded and best-effort —
// failures are logged, not fatal, since neither blocks the agent from running.
func (m *Manager) seedWorktree(ctx context.Context, t task.Task, wtPath, branch string) {
	if err := writeContextFile(ctx, t, wtPath, branch); err != nil {
		m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
	}
	if err := ensureNotesFile(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.notes-file", "task_id", t.ID, "err", err)
	}
	if err := excludeWorkflowScratchFiles(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.workflow-scratch-exclude", "task_id", t.ID, "err", err)
	}
}

// adoptWorktree wires Sybra to run a task inside a pre-existing, externally
// managed git worktree (t.WorktreeDir). It validates the directory, records the
// checked-out branch on the task, installs the project's commit/push hooks, and
// drops the identity beacon. It deliberately performs no history-changing git
// operations (no fetch, add, rebase, or push) and never schedules cleanup — the
// only worktree mutations are the hook/push-gate install (via installChecks)
// and the beacon file. The owning tool (e.g. Orca) keeps responsibility for the
// directory's lifecycle. Project setup commands are intentionally skipped: the
// adopted worktree is assumed already provisioned by the owning tool.
//
// Adoption is refused when the worktree is in detached HEAD or sits on the
// repo's default branch — the implementation agent commits to and pushes the
// checked-out branch, so either case would push agent work to a shared branch
// (or origin's default) with no PR/review. The caller must hand off from a
// dedicated feature branch.
func (m *Manager) adoptWorktree(ctx context.Context, t task.Task, onPhase func(string)) (string, error) {
	wtPath := t.WorktreeDir
	callPhase(onPhase, "Adopting worktree…")
	info, err := os.Stat(wtPath)
	if err != nil {
		return "", fmt.Errorf("adopt worktree %q: %w", wtPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("adopt worktree %q: not a directory", wtPath)
	}
	if !project.WorktreeHealthy(ctx, wtPath) {
		return "", fmt.Errorf("adopt worktree %q: not a healthy git worktree", wtPath)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("adopt worktree: get project %q: %w", t.ProjectID, err)
	}

	branch, err := project.CurrentBranch(ctx, wtPath)
	if err != nil {
		return "", fmt.Errorf("adopt worktree %q: resolve branch: %w", wtPath, err)
	}
	if branch == "" {
		return "", fmt.Errorf("adopt worktree %q: detached HEAD — check out a feature branch before handoff", wtPath)
	}
	// Refuse the default branch: pushing the agent's commits there bypasses PR
	// review entirely. Fail closed — if the default branch cannot be
	// determined, abort rather than risk adopting a worktree that turns out to
	// be on it.
	def, derr := project.DefaultBranch(ctx, proj.ClonePath)
	if derr != nil {
		return "", fmt.Errorf("adopt worktree %q: cannot determine default branch to guard against: %w", wtPath, derr)
	}
	if branch == def {
		return "", fmt.Errorf("adopt worktree %q: checked out on default branch %q — create a feature branch before handoff", wtPath, def)
	}

	m.ensureBranch(t, branch)
	m.installChecks(ctx, wtPath, proj)
	m.seedWorktree(ctx, t, wtPath, branch)
	m.logger.Info("worktree.adopted", "task_id", t.ID, "path", wtPath, "branch", branch)
	return wtPath, nil
}

// reuseFixWorktree is the fix/branch-fix analogue of PrepareForTask's
// healOrRecreate + reconcile fast path: PrepareForFix and PrepareForBranchFix
// used to unconditionally RemoveWorktreeReconcile any existing worktree and
// recreate it from scratch (full setup: mise install, npm ci, npm run
// build:desktop) on every single dispatch, even though a healthy worktree
// already checked out on the right branch just needs its remote fast-forwarded.
// Returns (true, nil) when wtPath is now healthy, on wantBranch, and synced to
// the latest remote head — the caller can skip setup entirely, mirroring
// PrepareForTask's genuine-reuse path. Also true when the branch had diverged
// from its own remote head and MergeDivergedRemote deterministically
// reconciled it with a real merge. Returns (false, nil) when there was
// nothing to reuse (no worktree, unhealthy, wrong branch, or a non-diverged
// remote sync failure) — in every such case wtPath has already been removed,
// so the caller's normal create-fresh path runs unmodified. Returns a non-nil
// error (wtPath removed) when the branch diverged from its own remote head
// AND reconciling that with a real merge hit a genuine content conflict — a
// recreate cannot paper over that, so the caller must surface it rather than
// silently continue (see #2347).
func (m *Manager) reuseFixWorktree(ctx context.Context, taskID, clonePath, wtPath, wantBranch string) (bool, error) {
	if _, statErr := os.Stat(wtPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, fmt.Errorf("stat worktree %s: %w", wtPath, statErr)
	}
	usable, err := m.healOrRecreate(ctx, taskID, clonePath, wtPath, wantBranch)
	if err != nil {
		return false, err
	}
	if !usable {
		return false, nil
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("fix.worktree.sanitize", "task_id", taskID, "err", err)
	}
	// Fast-forward the branch to the live remote head (e.g. a fix pushed from
	// another clone/machine since this worktree was last used). Unlike
	// PrepareForTask's reused path there is no rebase here — fix worktrees
	// check out the PR/task branch directly. Any failure other than a genuine
	// divergence (dirty, transient network) falls back to a clean recreate
	// rather than risk handing the fixer a stale or half-synced checkout; the
	// reused worktree was always freshly recreated before this change, so
	// recreation on failure is strictly no worse than the prior behavior.
	if err := project.ReconcileWithRemote(ctx, wtPath, wantBranch); err != nil {
		if errors.Is(err, project.ErrBranchDiverged) {
			// A plain recreate here would just re-check-out the same diverged
			// local branch and reproduce the bug this guards against (#2347)
			// — the recreated worktree still starts ahead of, and behind, its
			// own remote head. Deterministically reconcile it with a real
			// merge first instead.
			merged, mergeErr := project.MergeDivergedRemote(ctx, wtPath, wantBranch)
			switch {
			case mergeErr != nil:
				m.logger.Warn("fix.worktree.reconcile-diverged-merge-failed", "task_id", taskID, "branch", wantBranch, "err", mergeErr)
			case merged:
				m.logger.Info("fix.worktree.reconcile-diverged-merged", "task_id", taskID, "branch", wantBranch)
				return true, nil
			default:
				// Genuine content conflict merging the branch's own remote
				// head into local — a real semantic blocker between two
				// copies of the same branch, not something a recreate can
				// paper over. Stop here rather than silently dispatching the
				// fix agent against an unreconciled worktree.
				m.logger.Warn("fix.worktree.reconcile-diverged-conflict", "task_id", taskID, "branch", wantBranch)
				if rerr := project.RemoveWorktreeReconcile(ctx, clonePath, wtPath); rerr != nil {
					return false, fmt.Errorf("remove worktree after diverged-branch conflict: %w", rerr)
				}
				return false, fmt.Errorf("%w: branch %s has a genuine content conflict against its own remote head", project.ErrBranchDiverged, wantBranch)
			}
		} else {
			m.logger.Warn("fix.worktree.reconcile-failed-recreate", "task_id", taskID, "branch", wantBranch, "err", err)
		}
		if rerr := project.RemoveWorktreeReconcile(ctx, clonePath, wtPath); rerr != nil {
			return false, fmt.Errorf("remove worktree after reconcile failure: %w", rerr)
		}
		return false, nil
	}
	return true, nil
}

func (m *Manager) reuseBranchConflictWorktree(ctx context.Context, taskID, clonePath, wtPath, wantBranch string) (bool, error) {
	if _, statErr := os.Stat(wtPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, fmt.Errorf("stat worktree %s: %w", wtPath, statErr)
	}
	usable, err := m.healOrRecreate(ctx, taskID, clonePath, wtPath, wantBranch)
	if err != nil {
		return false, err
	}
	if !usable {
		return false, nil
	}
	if _, err := project.CheckpointCommit(ctx, wtPath,
		"wip: checkpoint before branch conflict recovery\n\nSybra preserved local work before preparing conflict recovery."); err != nil {
		return false, fmt.Errorf("checkpoint branch-conflict worktree: %w", err)
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		return false, fmt.Errorf("sanitize branch-conflict worktree: %w", err)
	}
	return true, nil
}

type freshFixReconcileTarget struct {
	remote string
	ref    string
}

// reconcileFreshFixWorktree ensures a newly created fix/branch-fix worktree is
// normalized to a single branch head before dispatch. A recreated worktree can
// still start from a stale local branch ref in the bare clone; if that branch
// had unpushed local commits and the remote also advanced, simply re-checking
// it out reproduces the same ahead/behind divergence that forced recovery.
func (m *Manager) reconcileFreshFixWorktree(ctx context.Context, taskID, clonePath, wtPath, branch string, target freshFixReconcileTarget) error {
	if target.remote == "" && target.ref == "" {
		return nil
	}

	reconcile := func() error {
		if target.ref != "" {
			return project.ReconcileWithRef(ctx, wtPath, target.ref)
		}
		return project.ReconcileWithNamedRemote(ctx, wtPath, target.remote, branch)
	}
	merge := func() (bool, error) {
		if target.ref != "" {
			return project.MergeDivergedRef(ctx, wtPath, target.ref)
		}
		return project.MergeDivergedNamedRemote(ctx, wtPath, target.remote, branch)
	}

	if err := reconcile(); err != nil {
		if errors.Is(err, project.ErrBranchDiverged) {
			merged, mergeErr := merge()
			switch {
			case mergeErr != nil:
				if rerr := project.RemoveWorktreeReconcile(ctx, clonePath, wtPath); rerr != nil {
					m.logger.Warn("fix.worktree.cleanup-after-diverged-merge-failed", "task_id", taskID, "branch", branch, "err", rerr)
				}
				return fmt.Errorf("reconcile fresh %s with remote: %w", branch, mergeErr)
			case merged:
				m.logger.Info("fix.worktree.reconcile-fresh-diverged-merged", "task_id", taskID, "branch", branch)
				return nil
			default:
				m.logger.Warn("fix.worktree.reconcile-fresh-diverged-conflict", "task_id", taskID, "branch", branch)
				if rerr := project.RemoveWorktreeReconcile(ctx, clonePath, wtPath); rerr != nil {
					m.logger.Warn("fix.worktree.cleanup-after-diverged-conflict", "task_id", taskID, "branch", branch, "err", rerr)
				}
				return fmt.Errorf("%w: branch %s has a genuine content conflict against its own remote head", project.ErrBranchDiverged, branch)
			}
		}
		if rerr := project.RemoveWorktreeReconcile(ctx, clonePath, wtPath); rerr != nil {
			m.logger.Warn("fix.worktree.cleanup-after-fresh-reconcile-failed", "task_id", taskID, "branch", branch, "err", rerr)
		}
		if project.IsTransientNetworkError(err) {
			return fmt.Errorf("%w: reconcile %s with remote: %w", ErrTransientFetch, branch, err)
		}
		return fmt.Errorf("reconcile fresh %s with remote: %w", branch, err)
	}
	return nil
}

// finalizeWorktree runs post-checkout hooks shared by the "reuse existing
// worktree" and "checkout existing branch" fast-paths in PrepareForTask.
func (m *Manager) finalizeWorktree(ctx context.Context, t task.Task, wtPath, wtBranch string, proj project.Project) (string, error) {
	m.installChecks(ctx, wtPath, proj)
	m.ensureBranch(t, wtBranch)
	m.seedWorktree(ctx, t, wtPath, wtBranch)
	return wtPath, nil
}

func (m *Manager) recordPreparedState(ctx context.Context, taskID, wtPath, wtBranch string) {
	wrote, err := prepstate.WriteVerified(ctx, wtPath, wtBranch)
	if err != nil {
		m.logger.Warn("worktree.prep-state-write", "task_id", taskID, "path", wtPath, "branch", wtBranch, "err", err)
		return
	}
	if wrote {
		m.logger.Info("worktree.prep-state-written", "task_id", taskID, "path", wtPath, "branch", wtBranch)
	}
}

// PrepareForReview creates a detached-HEAD worktree for read-only PR review.
func (m *Manager) PrepareForReview(ctx context.Context, t task.Task) (string, error) {
	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		return "", err
	}
	defer release()

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(ctx, proj.ClonePath); err != nil {
		m.logger.Warn("review.worktree.fetch", "project", proj.ID, "err", err)
	}

	wtPath := m.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr == nil {
		return wtPath, nil
	}

	// The branch name is only a log annotation now (the checkout uses the PR
	// head ref below), so a transient gh failure must not block worktree
	// creation.
	branch, err := m.prBranch(t.ProjectID, t.PRNumber)
	if err != nil {
		m.logger.Warn("review.worktree.pr-branch", "project", proj.ID, "pr", t.PRNumber, "err", err)
		branch = ""
	}

	// Check out the PR head via refs/pull/<N>/head rather than
	// refs/remotes/origin/<branch>: a fork PR's head branch never lands under
	// origin, so the latter fails with "invalid reference".
	//
	// Like reconcileAndRebase's remote fetch, a transient connectivity blip
	// here (SSH/DNS/timeout) looks identical to a genuine failure unless
	// distinguished — wrap it as ErrTransientFetch so ClassifyAgentStartFailure
	// treats it as retryable instead of feeding the circuit breaker (the raw
	// error used to fall into ClassifyAgentStartFailure's default case, which
	// counts toward the breaker and can trip a review dispatch to
	// human-required after a few DNS blips even though nothing needs a human).
	ref, err := project.FetchPRHead(ctx, proj.ClonePath, t.PRNumber)
	if err != nil {
		if project.IsTransientNetworkError(err) {
			return "", fmt.Errorf("%w: fetch pr head: %w", ErrTransientFetch, err)
		}
		return "", fmt.Errorf("fetch pr head: %w", err)
	}
	if err := project.CreateWorktreeDetached(ctx, proj.ClonePath, wtPath, ref); err != nil {
		return "", fmt.Errorf("create review worktree: %w", err)
	}
	if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
		m.logger.Warn("review.worktree.signoff-hook", "task_id", t.ID, "err", err)
	}
	m.logger.Info("review.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	// Review is read-only and never builds anything — skip the desktop build
	// step other roles' setup pulls in (see filterNonAuthoringSetup).
	setupCmds := filterNonAuthoringSetup(m.resolveTrustedSetupCommands(ctx, proj))
	if err := m.runSetup(ctx, t.ID, wtPath, setupCmds); err != nil {
		return "", fmt.Errorf("review setup: %w", err)
	}
	return wtPath, nil
}

// PrepareForBranchFix creates a worktree checking out the task's OWN branch
// (resolved by name via branchNameForTask, not via a PR lookup) so a no-PR
// conflict-recovery agent can merge base in and push. Sibling of
// PrepareForFix for tasks with no PR yet (still in
// implementation/review/testing, or at create_pr): the same non-rebasing
// checkout dance, minus PrepareForFix's PR-head fallback — a task's own
// branch either exists (locally or on origin) or this is a hard failure
// (ErrTaskBranchMissing) that must escalate rather than guess at a ref to
// check out. Does not change PrepareForFix's behavior for its own PR-keyed
// callers. Setup failures are non-gating, same rationale as PrepareForFix.
func (m *Manager) PrepareForBranchFix(ctx context.Context, t task.Task) (string, error) {
	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		return "", err
	}
	defer release()

	// Adopt an externally-created worktree as-is, mirroring PrepareForFix.
	if t.WorktreeDir != "" {
		return m.adoptWorktree(ctx, t, nil)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	var fetchErr error
	if fetchErr = project.FetchOrigin(ctx, proj.ClonePath); fetchErr != nil {
		m.logger.Warn("branch-fix.worktree.fetch", "project", proj.ID, "err", fetchErr)
	}

	branch := branchNameForTask(t)
	wtPath := m.PathFor(t)

	// Reuse a healthy worktree already on the task's branch, same rationale
	// and fallback-to-recreate behavior as PrepareForFix.
	reused, err := m.reuseFixWorktree(ctx, t.ID, proj.ClonePath, wtPath, branch)
	if err != nil {
		return "", fmt.Errorf("reuse branch-fix worktree: %w", err)
	}
	if reused {
		m.ensureBranch(t, branch)
		if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
			m.logger.Warn("branch-fix.worktree.signoff-hook", "task_id", t.ID, "err", err)
		}
		m.logger.Info("branch-fix.worktree.reused", "task_id", t.ID, "path", wtPath, "branch", branch)
		if t.RunRole != "pr-fix" {
			if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
				m.logger.Warn("branch-fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
			}
		}
		return wtPath, nil
	}

	originRef := "refs/remotes/origin/" + branch
	switch {
	case project.BranchExists(ctx, proj.ClonePath, branch):
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, branch); err != nil {
			return "", fmt.Errorf("checkout task branch %s: %w", branch, err)
		}
	case project.RefExists(ctx, proj.ClonePath, originRef):
		if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, branch, originRef); err != nil {
			return "", fmt.Errorf("create branch-fix worktree: %w", err)
		}
	default:
		if fetchErr != nil {
			return "", fmt.Errorf("fetch origin for task branch %s: %w", branch, fetchErr)
		}
		// No PR to fall back to (unlike PrepareForFix) — the branch is
		// genuinely missing. Escalate rather than guess.
		return "", fmt.Errorf("%w: %s", ErrTaskBranchMissing, branch)
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("branch-fix.worktree.sanitize", "task_id", t.ID, "err", err)
	}
	if project.RemoteConfigured(ctx, wtPath, "origin") {
		target := freshFixReconcileTarget{remote: "origin"}
		if err := m.reconcileFreshFixWorktree(ctx, t.ID, proj.ClonePath, wtPath, branch, target); err != nil {
			return "", fmt.Errorf("reconcile fresh branch-fix worktree: %w", err)
		}
	}
	m.ensureBranch(t, branch)
	if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
		m.logger.Warn("branch-fix.worktree.signoff-hook", "task_id", t.ID, "err", err)
	}
	m.logger.Info("branch-fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	// Read setup from the trusted default branch, never the checked-out worktree:
	// this branch already carries commits an implementation agent pushed in an
	// earlier run, so its own .sybra.yaml is not Sybra-authored and a compromised
	// agent could plant a malicious setup: block that runs via unsandboxed
	// `sh -c` (same class as issue #1519 — see resolveTrustedSetupCommands).
	if err := m.runSetupNonGating(ctx, t.ID, wtPath, m.resolveTrustedSetupCommands(ctx, proj)); err != nil {
		return "", fmt.Errorf("branch-fix setup: %w", err)
	}
	// pr-fix-role recovery runs must push straight to origin (the task's own
	// branch lives there, no fork), mirroring PrepareForFix's RunRole guard.
	if t.RunRole != "pr-fix" {
		if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
			m.logger.Warn("branch-fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
		}
	}
	return wtPath, nil
}

// PrepareForBranchConflict prepares a worktree for the dedicated
// branch-conflict recovery workflow. Unlike PrepareForBranchFix, it
// intentionally does NOT auto-reconcile the branch with origin: this path is
// entered precisely because that reconciliation already hit a real content
// conflict, so repeating it here would just fail before the fixer agent gets a
// chance to resolve the branch manually.
func (m *Manager) PrepareForBranchConflict(ctx context.Context, t task.Task) (string, error) {
	return m.PrepareForBranchConflictFromRemote(ctx, t, "origin")
}

// PrepareForBranchConflictFromRemote prepares a branch-conflict recovery
// worktree whose remote side lives on remote, e.g. fork-backed PR heads.
func (m *Manager) PrepareForBranchConflictFromRemote(ctx context.Context, t task.Task, remote string) (string, error) {
	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		return "", err
	}
	defer release()

	if t.WorktreeDir != "" {
		return m.adoptWorktree(ctx, t, nil)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if remote == "" {
		remote = "origin"
	}
	var fetchErr error
	if fetchErr = project.FetchOrigin(ctx, proj.ClonePath); fetchErr != nil {
		m.logger.Warn("branch-conflict.worktree.fetch", "project", proj.ID, "err", fetchErr)
	}

	branch := branchNameForTask(t)
	targetFetchErr := fetchErr
	if remote != "origin" {
		targetFetchErr = nil
		if !project.RemoteConfigured(ctx, proj.ClonePath, remote) {
			targetFetchErr = fmt.Errorf("remote %s is not configured", remote)
		} else if err := project.FetchRemoteBranch(ctx, proj.ClonePath, remote, branch); err != nil {
			targetFetchErr = err
		}
		if targetFetchErr != nil {
			m.logger.Warn("branch-conflict.worktree.fetch-remote", "project", proj.ID, "remote", remote, "branch", branch, "err", targetFetchErr)
		}
	}
	wtPath := m.PathFor(t)

	reused, err := m.reuseBranchConflictWorktree(ctx, t.ID, proj.ClonePath, wtPath, branch)
	if err != nil {
		return "", fmt.Errorf("reuse branch-conflict worktree: %w", err)
	}
	if reused {
		m.ensureBranch(t, branch)
		if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
			m.logger.Warn("branch-conflict.worktree.signoff-hook", "task_id", t.ID, "err", err)
		}
		m.logger.Info("branch-conflict.worktree.reused", "task_id", t.ID, "path", wtPath, "branch", branch)
		if t.RunRole != "pr-fix" {
			if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
				m.logger.Warn("branch-conflict.worktree.fork-only-push", "task_id", t.ID, "err", err)
			}
		}
		return wtPath, nil
	}

	remoteRef := "refs/remotes/" + remote + "/" + branch
	switch {
	case project.BranchExists(ctx, proj.ClonePath, branch):
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, branch); err != nil {
			return "", fmt.Errorf("checkout task branch %s: %w", branch, err)
		}
	case project.RefExists(ctx, proj.ClonePath, remoteRef):
		if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, branch, remoteRef); err != nil {
			return "", fmt.Errorf("create branch-conflict worktree: %w", err)
		}
	default:
		if targetFetchErr != nil {
			return "", fmt.Errorf("fetch %s for task branch %s: %w", remote, branch, targetFetchErr)
		}
		return "", fmt.Errorf("%w: %s", ErrTaskBranchMissing, branch)
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("branch-conflict.worktree.sanitize", "task_id", t.ID, "err", err)
	}
	m.ensureBranch(t, branch)
	if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
		m.logger.Warn("branch-conflict.worktree.signoff-hook", "task_id", t.ID, "err", err)
	}
	m.logger.Info("branch-conflict.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	if err := m.runSetupNonGating(ctx, t.ID, wtPath, m.resolveTrustedSetupCommands(ctx, proj)); err != nil {
		return "", fmt.Errorf("branch-conflict setup: %w", err)
	}
	if t.RunRole != "pr-fix" {
		if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
			m.logger.Warn("branch-conflict.worktree.fork-only-push", "task_id", t.ID, "err", err)
		}
	}
	return wtPath, nil
}

// ResetForRetry discards a killed or hung agent's partial work before a clean
// retry, under the same per-path exclusion every Prepare* takes. It aborts an
// in-progress rebase and hard-resets the tree, so running it against a
// directory a live preparation is inside produces exactly the half-rebased
// tree the exclusion exists to prevent.
//
// dir overrides the task's own worktree path for callers that already resolved
// one. It returns the path it actually acted on — callers log that rather than
// their own argument, which is empty whenever they left the resolution here —
// and whether a reset ran; a directory that does not exist is not an error.
func (m *Manager) ResetForRetry(ctx context.Context, t task.Task, dir, ref string) (target string, reset bool, err error) {
	target = dir
	if target == "" {
		target = m.PathFor(t)
	}
	if target == "" {
		return "", false, nil
	}

	release, err := m.lockPath(target)
	if err != nil {
		return target, false, err
	}
	defer release()

	if _, statErr := os.Stat(target); statErr != nil {
		if os.IsNotExist(statErr) {
			return target, false, nil
		}
		return target, false, fmt.Errorf("stat clean retry worktree: %w", statErr)
	}
	if err := project.ResetWorktreeForRetry(ctx, target, ref); err != nil {
		return target, false, fmt.Errorf("reset worktree for retry: %w", err)
	}
	return target, true, nil
}

// PruneMissingWorktree drops the bare repo's admin entry for a worktree path
// whose directory is gone from disk. Locked on that path: the directory can be
// missing only because another preparation has not created it yet, and pruning
// the registration mid `git worktree add` leaves a checkout git no longer
// tracks.
func (m *Manager) PruneMissingWorktree(ctx context.Context, clonePath, dir string) error {
	release, err := m.lockPath(dir)
	if err != nil {
		return err
	}
	defer release()

	return project.RemoveWorktreeReconcile(ctx, clonePath, dir)
}

// RecreateFromBase discards a task's diverged branch and its worktree so the
// next PrepareForTask rebuilds it fresh off the project's base ref. The branch
// tip is first backed up to refs/sybra-backup/<branch> (best-effort) so the
// discarded commits stay recoverable. Used as the last-resort recovery when
// merge-based branch-conflict recovery is exhausted on a no-PR task: the branch
// genuinely cannot be reconciled, so re-implementing from a clean base is the
// only autonomous path left.
func (m *Manager) RecreateFromBase(ctx context.Context, t task.Task) error {
	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		return err
	}
	defer release()

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return fmt.Errorf("get project: %w", err)
	}
	branch := branchNameForTask(t)
	wtPath := m.PathFor(t)
	if project.BranchExists(ctx, proj.ClonePath, branch) {
		if berr := project.BackupBranchRef(ctx, proj.ClonePath, branch); berr != nil {
			m.logger.Warn("worktree.recreate.backup", "task_id", t.ID, "branch", branch, "err", berr)
		}
	}
	if rerr := project.RemoveWorktreeReconcile(ctx, proj.ClonePath, wtPath); rerr != nil {
		return fmt.Errorf("remove worktree: %w", rerr)
	}
	if project.BranchExists(ctx, proj.ClonePath, branch) {
		if derr := project.DeleteBranch(ctx, proj.ClonePath, branch); derr != nil {
			return fmt.Errorf("delete branch: %w", derr)
		}
	}
	if derr := project.DeleteUpstreamBranch(ctx, proj.ClonePath, branch); derr != nil {
		return fmt.Errorf("delete upstream branch: %w", derr)
	}
	m.logger.Info("worktree.recreated-from-base", "task_id", t.ID, "branch", branch)
	return nil
}

// PrepareForFix creates a worktree checking out the PR's head branch
// so the agent can rebase and push. Setup command failures do not abort
// worktree creation (see runSetupNonGating) — the fixer this worktree is for
// may exist precisely because that PR broke a gating setup command (e.g. a
// build step), so refusing to create the worktree would deadlock the task.
func (m *Manager) PrepareForFix(ctx context.Context, t task.Task, prNumber int) (string, error) {
	release, err := m.lockPath(m.PathFor(t))
	if err != nil {
		return "", err
	}
	defer release()

	// Adopt an externally-created worktree (e.g. an Orca handoff) as-is instead
	// of re-creating it. Without this guard PrepareForFix runs
	// `git worktree add` at the adopted path, which fails ("already exists" /
	// "branch already checked out") and — after wtFailureLimit retries — strands
	// the task in human-required. The fix agent rebases/pushes from this
	// worktree itself, so no Sybra-managed checkout is needed. Mirrors
	// PrepareForTask.
	if t.WorktreeDir != "" {
		return m.adoptWorktree(ctx, t, nil)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(ctx, proj.ClonePath); err != nil {
		m.logger.Warn("fix.worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := m.prBranch(t.ProjectID, prNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr branch: %w", err)
	}

	wtPath := m.PathFor(t)

	// Reuse a healthy worktree already on the fix branch — fast-forwarded to
	// the latest remote head, no setup re-run. Only when that isn't possible
	// (no worktree, unhealthy, wrong branch, or a remote conflict) does the
	// stale worktree get removed and recreated from scratch below.
	reused, err := m.reuseFixWorktree(ctx, t.ID, proj.ClonePath, wtPath, branch)
	if err != nil {
		return "", fmt.Errorf("reuse fix worktree: %w", err)
	}
	if reused {
		m.ensureBranch(t, branch)
		if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
			m.logger.Warn("fix.worktree.signoff-hook", "task_id", t.ID, "err", err)
		}
		m.logger.Info("fix.worktree.reused", "task_id", t.ID, "path", wtPath, "branch", branch)
		if t.RunRole != "pr-fix" {
			if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
				m.logger.Warn("fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
			}
		}
		return wtPath, nil
	}

	originRef := "refs/remotes/origin/" + branch
	reconcileTarget := freshFixReconcileTarget{remote: "origin"}
	switch {
	case project.BranchExists(ctx, proj.ClonePath, branch):
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, branch); err != nil {
			return "", fmt.Errorf("checkout fix branch %s: %w", branch, err)
		}
		if !project.RefExists(ctx, proj.ClonePath, originRef) {
			prHeadRef, err := project.FetchPRHead(ctx, proj.ClonePath, prNumber)
			if err != nil {
				if project.IsTransientNetworkError(err) {
					return "", fmt.Errorf("%w: fetch pr head: %w", ErrTransientFetch, err)
				}
				return "", fmt.Errorf("fetch pr head: %w", err)
			}
			reconcileTarget = freshFixReconcileTarget{ref: prHeadRef}
		}
	case project.RefExists(ctx, proj.ClonePath, originRef):
		if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, branch, originRef); err != nil {
			return "", fmt.Errorf("create fix worktree: %w", err)
		}
	default:
		// Branch head is not under refs/remotes/origin/* — e.g. a fork PR, or a
		// branch FetchOrigin could not pull. Fall back to the PR head ref, which
		// GitHub exposes at refs/pull/<N>/head for every PR, and branch off that
		// so the fix agent still gets a real local branch to push.
		prHeadRef, err := project.FetchPRHead(ctx, proj.ClonePath, prNumber)
		if err != nil {
			if project.IsTransientNetworkError(err) {
				return "", fmt.Errorf("%w: fetch pr head: %w", ErrTransientFetch, err)
			}
			return "", fmt.Errorf("fetch pr head: %w", err)
		}
		if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, branch, prHeadRef); err != nil {
			return "", fmt.Errorf("create fix worktree: %w", err)
		}
		reconcileTarget = freshFixReconcileTarget{ref: prHeadRef}
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("fix.worktree.sanitize", "task_id", t.ID, "err", err)
	}
	if err := m.reconcileFreshFixWorktree(ctx, t.ID, proj.ClonePath, wtPath, branch, reconcileTarget); err != nil {
		return "", fmt.Errorf("reconcile fresh fix worktree: %w", err)
	}
	m.ensureBranch(t, branch)
	if err := project.InstallSignoffHook(ctx, wtPath); err != nil {
		m.logger.Warn("fix.worktree.signoff-hook", "task_id", t.ID, "err", err)
	}
	m.logger.Info("fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	if err := m.runSetupNonGating(ctx, t.ID, wtPath, m.resolveTrustedSetupCommands(ctx, proj)); err != nil {
		return "", fmt.Errorf("fix setup: %w", err)
	}
	// pr-fix tasks may need to push either to origin or to a fork-hosted PR
	// head branch. Skip fork-only-push so the prompt/runtime can choose the
	// correct remote for the existing PR instead of blocking one side.
	if t.RunRole != "pr-fix" {
		if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
			m.logger.Warn("fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
		}
	}
	return wtPath, nil
}
