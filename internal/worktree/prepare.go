package worktree

import (
	"errors"
	"fmt"
	"os"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

var ErrRebaseFailed = errors.New("worktree rebase failed")

// PrepareForTask creates (or reuses) a worktree for implementation work.
// Fetches origin, creates a conventional-prefixed branch off default branch,
// pushes upstream, and sets task.Branch.
// onPhase is an optional callback that receives human-readable phase labels
// as work progresses; pass nil when phase reporting is not needed.
func (m *Manager) PrepareForTask(t task.Task, onPhase func(string)) (string, error) {
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
		return m.adoptWorktree(t, onPhase)
	}

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

	if proj.WorktreeBaseRef == project.WorktreeBaseRefHead {
		if err := project.SyncLocalBranch(proj.ClonePath, branch); err != nil {
			m.logger.Warn("worktree.sync-local-branch", "task_id", t.ID, "branch", branch, "err", err)
		}
	}

	wtPath := m.PathFor(t)
	wtBranch := branchNameForTask(t)
	baseRef := worktreeBaseRef(proj.WorktreeBaseRef, branch)

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
			// Rebase must succeed before another agent run. Continuing from a
			// stale branch makes downstream diff gates scan historical commits.
			callPhase(onPhase, "Rebasing onto origin…")
			if err := project.RebaseOnto(wtPath, baseRef); err != nil {
				return "", fmt.Errorf("%w: rebase %s onto %s: %w", ErrRebaseFailed, wtBranch, baseRef, err)
			}
			m.logger.Info("worktree.rebased", "task_id", t.ID, "path", wtPath, "base", baseRef)
			// Sync remote after rebase. PushSync picks the minimum mode —
			// no-op when local matches remote, regular push for
			// fast-forward, --force-with-lease only on divergence.
			callPhase(onPhase, "Syncing upstream…")
			m.logPushSync(t.ID, wtBranch, project.PushSync(wtPath, wtBranch))
			return m.finalizeWorktree(t, wtPath, wtBranch, proj)
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
			return "", fmt.Errorf("%w: rebase %s onto %s: %w", ErrRebaseFailed, wtBranch, baseRef, err)
		}
		// Sync remote after rebase.
		callPhase(onPhase, "Syncing upstream…")
		m.logPushSync(t.ID, wtBranch, project.PushSync(wtPath, wtBranch))
		m.logger.Info("worktree.reused-branch", "task_id", t.ID, "path", wtPath, "branch", wtBranch)
		callPhase(onPhase, "Running setup…")
		if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
			return "", fmt.Errorf("setup on reused branch: %w", err)
		}
		return m.finalizeWorktree(t, wtPath, wtBranch, proj)
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
	m.seedWorktree(t, wtPath, wtBranch)
	return wtPath, nil
}

// seedWorktree drops the per-worktree agent context into an implementation
// worktree: the identity beacon (writeContextFile) and the working-memory
// scratchpad (ensureNotesFile). Both are git-excluded and best-effort —
// failures are logged, not fatal, since neither blocks the agent from running.
func (m *Manager) seedWorktree(t task.Task, wtPath, branch string) {
	if err := writeContextFile(t, wtPath, branch); err != nil {
		m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
	}
	if err := ensureNotesFile(wtPath); err != nil {
		m.logger.Warn("worktree.notes-file", "task_id", t.ID, "err", err)
	}
	if err := excludeWorkflowScratchFiles(wtPath); err != nil {
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
func (m *Manager) adoptWorktree(t task.Task, onPhase func(string)) (string, error) {
	wtPath := t.WorktreeDir
	callPhase(onPhase, "Adopting worktree…")
	info, err := os.Stat(wtPath)
	if err != nil {
		return "", fmt.Errorf("adopt worktree %q: %w", wtPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("adopt worktree %q: not a directory", wtPath)
	}
	if !project.WorktreeHealthy(wtPath) {
		return "", fmt.Errorf("adopt worktree %q: not a healthy git worktree", wtPath)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("adopt worktree: get project %q: %w", t.ProjectID, err)
	}

	branch, err := project.CurrentBranch(wtPath)
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
	def, derr := project.DefaultBranch(proj.ClonePath)
	if derr != nil {
		return "", fmt.Errorf("adopt worktree %q: cannot determine default branch to guard against: %w", wtPath, derr)
	}
	if branch == def {
		return "", fmt.Errorf("adopt worktree %q: checked out on default branch %q — create a feature branch before handoff", wtPath, def)
	}

	m.ensureBranch(t, branch)
	m.installChecks(wtPath, proj)
	m.seedWorktree(t, wtPath, branch)
	m.logger.Info("worktree.adopted", "task_id", t.ID, "path", wtPath, "branch", branch)
	return wtPath, nil
}

// finalizeWorktree runs post-checkout hooks shared by the "reuse existing
// worktree" and "checkout existing branch" fast-paths in PrepareForTask.
func (m *Manager) finalizeWorktree(t task.Task, wtPath, wtBranch string, proj project.Project) (string, error) {
	m.installChecks(wtPath, proj)
	m.ensureBranch(t, wtBranch)
	m.seedWorktree(t, wtPath, wtBranch)
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

	if proj.WorktreeBaseRef == project.WorktreeBaseRefHead {
		if err := project.SyncLocalBranch(proj.ClonePath, branch); err != nil {
			m.logger.Warn("worktree.sync-local-branch", "task_id", t.ID, "branch", branch, "err", err)
		}
	}

	wtPath := m.PathFor(t)
	wtBranch := branchNameForTask(t)
	baseRef := worktreeBaseRef(proj.WorktreeBaseRef, branch)

	if _, statErr := os.Stat(wtPath); statErr == nil {
		usable, err := m.healOrRecreate(t.ID, proj.ClonePath, wtPath)
		if err != nil {
			return "", err
		}
		if usable {
			m.logger.Info("chat.worktree.reused", "task_id", t.ID, "path", wtPath)
			m.ensureBranch(t, wtBranch)
			if err := writeContextFile(t, wtPath, wtBranch); err != nil {
				m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
			}
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
		if err := writeContextFile(t, wtPath, wtBranch); err != nil {
			m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
		}
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
	if err := writeContextFile(t, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
	}
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
	ref, err := project.FetchPRHead(proj.ClonePath, t.PRNumber)
	if err != nil {
		return "", fmt.Errorf("fetch pr head: %w", err)
	}
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
	// Adopt an externally-created worktree (e.g. an Orca handoff) as-is instead
	// of re-creating it. Without this guard PrepareForFix runs
	// `git worktree add` at the adopted path, which fails ("already exists" /
	// "branch already checked out") and — after wtFailureLimit retries — strands
	// the task in human-required. The fix agent rebases/pushes from this
	// worktree itself, so no Sybra-managed checkout is needed. Mirrors
	// PrepareForTask.
	if t.WorktreeDir != "" {
		return m.adoptWorktree(t, nil)
	}

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
	if project.BranchExists(proj.ClonePath, branch) {
		if err := project.CreateWorktreeExisting(proj.ClonePath, wtPath, branch); err != nil {
			return "", fmt.Errorf("checkout fix branch %s: %w", branch, err)
		}
	} else if err := project.CreateWorktree(proj.ClonePath, wtPath, branch, ref); err != nil {
		return "", fmt.Errorf("create fix worktree: %w", err)
	}
	if err := project.SanitizeWorktree(wtPath); err != nil {
		m.logger.Warn("fix.worktree.sanitize", "task_id", t.ID, "err", err)
	}
	m.ensureBranch(t, branch)
	m.logger.Info("fix.worktree.created", "task_id", t.ID, "path", wtPath, "branch", branch)
	if err := m.runSetup(t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", fmt.Errorf("fix setup: %w", err)
	}
	// pr-fix tasks must push to the existing PR's head branch on origin — not
	// via a fork remote. Skip fork-only-push so git push origin HEAD:<branch>
	// goes straight to the upstream and the fix lands on the tracked PR.
	if t.RunRole != "pr-fix" {
		if err := project.EnforceForkOnlyPush(wtPath); err != nil {
			m.logger.Warn("fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
		}
	}
	return wtPath, nil
}
