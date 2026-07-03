package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"

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

// SyncTaskBranch proactively reconciles a task's existing worktree branch with
// the project's default branch, reusing the exact same reconcile→rebase→merge
// strategy as PrepareForTask's reused-worktree path (reconcileAndRebase) so
// there is only ever one merge policy. It is intentionally non-blocking: it
// never mutates task status, never escalates to human-required, and any
// error is returned alongside a SyncResult so the caller can log/record the
// outcome and continue the workflow regardless. This differs deliberately from
// PrepareForTask: there, the same ErrRebaseFailed blocks another authoring run
// because the next agent cannot safely proceed on a conflicted branch; here,
// sync_branch is an opportunistic pre-PR refresh, so conflict/failure leaves
// the task no worse off than skipping the step and is only recorded.
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

	// Adopt an externally-created worktree (e.g. one Orca already checked out)
	// instead of creating a Sybra-managed one. Runs every agent in that
	// directory as-is — no fetch/add/rebase/push-create/cleanup. This both
	// honours the user's existing checkout and sidesteps git's "branch already
	// checked out" refusal that a second worktree on the same branch would hit.
	if t.WorktreeDir != "" {
		return m.adoptWorktree(ctx, t, onPhase)
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

	if _, statErr := os.Stat(wtPath); statErr == nil {
		callPhase(onPhase, "Checking worktree…")
		usable, err := m.healOrRecreate(ctx, t.ID, proj.ClonePath, wtPath)
		if err != nil {
			return "", err
		}
		if usable {
			if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
				m.logger.Warn("worktree.sanitize", "task_id", t.ID, "err", err)
			}
			if err := m.reconcileAndRebase(ctx, wtPath, wtBranch, baseRef, onPhase); err != nil {
				return "", err
			}
			m.logger.Info("worktree.rebased", "task_id", t.ID, "path", wtPath, "base", baseRef)
			// Sync remote after rebase. PushSync picks the minimum mode —
			// no-op when local matches remote, regular push for
			// fast-forward, --force-with-lease only on divergence.
			callPhase(onPhase, "Syncing upstream…")
			m.logPushSync(t.ID, wtBranch, project.PushSync(ctx, wtPath, wtBranch))
			return m.finalizeWorktree(ctx, t, wtPath, wtBranch, proj)
		}
		// Worktree was wiped — fall through to create paths below.
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
		callPhase(onPhase, "Running setup…")
		if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
			return "", fmt.Errorf("setup on reused branch: %w", err)
		}
		return m.finalizeWorktree(ctx, t, wtPath, wtBranch, proj)
	}

	callPhase(onPhase, "Creating worktree…")
	if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}
	m.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)
	callPhase(onPhase, "Running setup…")
	if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("setup on new worktree: %w", err)
	}
	m.installChecks(ctx, wtPath, proj)

	callPhase(onPhase, "Pushing upstream…")
	if err := project.PushUpstream(ctx, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.push-upstream", "task_id", t.ID, "branch", wtBranch, "err", err)
	}

	m.ensureBranch(t, wtBranch)
	m.seedWorktree(ctx, t, wtPath, wtBranch)
	return wtPath, nil
}

// reconcileAndRebase prepares a reused worktree branch for another agent run.
// It first adopts any commits pushed to the remote branch since this worktree's
// local ref was last updated (e.g. a review fix pushed from another
// clone/machine), so the rebase carries them forward instead of a later
// force-push dropping them. Rebasing must then succeed before the agent runs —
// continuing from a stale branch makes downstream diff gates scan historical
// commits. If the rebase fails, it falls back to an additive merge (see
// MergeOnto) before giving up, since most staleness under concurrent agents
// is not a genuine content conflict. All three failures (reconcile, rebase,
// and the merge fallback) wrap ErrRebaseFailed so the caller can surface a
// repairable worktree.
func (m *Manager) reconcileAndRebase(ctx context.Context, wtPath, wtBranch, baseRef string, onPhase func(string)) error {
	callPhase(onPhase, "Reconciling with remote…")
	if err := project.ReconcileWithRemote(ctx, wtPath, wtBranch); err != nil {
		return fmt.Errorf("%w: reconcile %s with remote: %w", ErrRebaseFailed, wtBranch, err)
	}
	callPhase(onPhase, "Rebasing onto origin…")
	if rebaseErr := project.RebaseOnto(ctx, wtPath, baseRef); rebaseErr != nil {
		// A rebase failure here is very often not a genuine content conflict —
		// under concurrent agents, base moves quickly and the branch's own
		// history is otherwise clean. Try an additive merge before giving up:
		// unlike rebase, it never rewrites existing commits, so recovery never
		// needs a force-push (see MergeOnto). Only when the merge also fails
		// (a real conflict) does this still surface ErrRebaseFailed for the
		// caller to escalate — ordinarily to human-required, or to the PR-fix
		// conflict-recovery agent when a PR is already open.
		callPhase(onPhase, "Rebase failed, trying merge…")
		if mergeErr := project.MergeOnto(ctx, wtPath, baseRef); mergeErr != nil {
			return fmt.Errorf("%w: rebase %s onto %s: %w (merge fallback also failed: %w)",
				ErrRebaseFailed, wtBranch, baseRef, rebaseErr, mergeErr)
		}
		m.logger.Info("worktree.rebase-recovered-via-merge", "branch", wtBranch, "base", baseRef)
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

// finalizeWorktree runs post-checkout hooks shared by the "reuse existing
// worktree" and "checkout existing branch" fast-paths in PrepareForTask.
func (m *Manager) finalizeWorktree(ctx context.Context, t task.Task, wtPath, wtBranch string, proj project.Project) (string, error) {
	m.installChecks(ctx, wtPath, proj)
	m.ensureBranch(t, wtBranch)
	m.seedWorktree(ctx, t, wtPath, wtBranch)
	return wtPath, nil
}

// PrepareForChat creates a worktree for an ephemeral chat session. Same as
// PrepareForTask but skips the upstream push — chat branches are local-only
// and deleted with the worktree when the chat ends.
// onPhase is an optional callback for phase labels; pass nil when not needed.
func (m *Manager) PrepareForChat(ctx context.Context, t task.Task, onPhase func(string)) (string, error) {
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

	if _, statErr := os.Stat(wtPath); statErr == nil {
		usable, err := m.healOrRecreate(ctx, t.ID, proj.ClonePath, wtPath)
		if err != nil {
			return "", err
		}
		if usable {
			m.logger.Info("chat.worktree.reused", "task_id", t.ID, "path", wtPath)
			m.ensureBranch(t, wtBranch)
			if err := writeContextFile(ctx, t, wtPath, wtBranch); err != nil {
				m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
			}
			return wtPath, nil
		}
		// Worktree was wiped — fall through.
	}

	callPhase(onPhase, "Creating worktree…")
	if project.BranchExists(ctx, proj.ClonePath, wtBranch) {
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, wtBranch); err != nil {
			return "", fmt.Errorf("checkout existing branch %s: %w", wtBranch, err)
		}
		m.logger.Info("chat.worktree.reused-branch", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
			return "", fmt.Errorf("chat setup on reused branch: %w", err)
		}
		m.ensureBranch(t, wtBranch)
		if err := writeContextFile(ctx, t, wtPath, wtBranch); err != nil {
			m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
		}
		return wtPath, nil
	}

	if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
		return "", fmt.Errorf("create chat worktree: %w", err)
	}
	m.logger.Info("chat.worktree.created", "task_id", t.ID, "path", wtPath)
	if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("chat setup on new worktree: %w", err)
	}
	m.ensureBranch(t, wtBranch)
	if err := writeContextFile(ctx, t, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
	}
	return wtPath, nil
}

// PrepareForReview creates a detached-HEAD worktree for read-only PR review.
func (m *Manager) PrepareForReview(ctx context.Context, t task.Task) (string, error) {
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
	ref, err := project.FetchPRHead(ctx, proj.ClonePath, t.PRNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr head: %w", err)
	}
	if err := project.CreateWorktreeDetached(ctx, proj.ClonePath, wtPath, ref); err != nil {
		return "", fmt.Errorf("create review worktree: %w", err)
	}
	m.logger.Info("review.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
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
// callers.
func (m *Manager) PrepareForBranchFix(ctx context.Context, t task.Task) (string, error) {
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

	// Remove stale worktree, same rationale as PrepareForFix.
	switch _, statErr := os.Stat(wtPath); {
	case statErr == nil:
		if err := project.RemoveWorktreeReconcile(ctx, proj.ClonePath, wtPath); err != nil {
			return "", fmt.Errorf("remove stale branch-fix worktree: %w", err)
		}
	case !os.IsNotExist(statErr):
		return "", fmt.Errorf("stat branch-fix worktree %s: %w", wtPath, statErr)
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
	m.ensureBranch(t, branch)
	m.logger.Info("branch-fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
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

// PrepareForFix creates a worktree checking out the PR's head branch
// so the agent can rebase and push.
func (m *Manager) PrepareForFix(ctx context.Context, t task.Task, prNumber int) (string, error) {
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

	// Remove stale worktree — previous agent may have left dirty state, or a
	// prior `worktree prune` (or crash) dropped the admin entry while the
	// checkout dir survived (orphan). RemoveWorktreeReconcile handles both.
	switch _, statErr := os.Stat(wtPath); {
	case statErr == nil:
		if err := project.RemoveWorktreeReconcile(ctx, proj.ClonePath, wtPath); err != nil {
			return "", fmt.Errorf("remove stale fix worktree: %w", err)
		}
	case !os.IsNotExist(statErr):
		return "", fmt.Errorf("stat fix worktree %s: %w", wtPath, statErr)
	}

	originRef := "refs/remotes/origin/" + branch
	switch {
	case project.BranchExists(ctx, proj.ClonePath, branch):
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, branch); err != nil {
			return "", fmt.Errorf("checkout fix branch %s: %w", branch, err)
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
			return "", fmt.Errorf("fetch pr head: %w", err)
		}
		if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, branch, prHeadRef); err != nil {
			return "", fmt.Errorf("create fix worktree: %w", err)
		}
	}
	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("fix.worktree.sanitize", "task_id", t.ID, "err", err)
	}
	m.ensureBranch(t, branch)
	m.logger.Info("fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("fix setup: %w", err)
	}
	// pr-fix tasks must push to the existing PR's head branch on origin — not
	// via a fork remote. Skip fork-only-push so git push origin HEAD:<branch>
	// goes straight to the upstream and the fix lands on the tracked PR.
	if t.RunRole != "pr-fix" {
		if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
			m.logger.Warn("fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
		}
	}
	return wtPath, nil
}
