package worktree

import (
	"fmt"
	"os"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

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
			// Rebase is best-effort — conflicts with main shouldn't block agent
			// start on a branch that already has committed work.
			callPhase(onPhase, "Rebasing onto origin…")
			if err := project.RebaseOnto(wtPath, baseRef); err != nil {
				m.logger.Warn("worktree.rebase-skipped", "task_id", t.ID, "base", baseRef, "err", err)
			} else {
				m.logger.Info("worktree.rebased", "task_id", t.ID, "path", wtPath, "base", baseRef)
				// Sync remote after rebase. PushSync picks the minimum mode —
				// no-op when local matches remote, regular push for
				// fast-forward, --force-with-lease only on divergence.
				callPhase(onPhase, "Syncing upstream…")
				m.logPushSync(t.ID, wtBranch, project.PushSync(wtPath, wtBranch))
			}
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
			m.logger.Warn("worktree.rebase-skipped", "task_id", t.ID, "base", baseRef, "err", err)
		} else {
			// Sync remote after rebase.
			callPhase(onPhase, "Syncing upstream…")
			m.logPushSync(t.ID, wtBranch, project.PushSync(wtPath, wtBranch))
		}
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
	if err := writeContextFile(t, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
	}
	return wtPath, nil
}

// finalizeWorktree runs post-checkout hooks shared by the "reuse existing
// worktree" and "checkout existing branch" fast-paths in PrepareForTask.
func (m *Manager) finalizeWorktree(t task.Task, wtPath, wtBranch string, proj project.Project) (string, error) {
	m.installChecks(wtPath, proj)
	m.ensureBranch(t, wtBranch)
	if err := writeContextFile(t, wtPath, wtBranch); err != nil {
		m.logger.Warn("worktree.context-file", "task_id", t.ID, "err", err)
	}
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
	if err := project.EnforceForkOnlyPush(wtPath); err != nil {
		m.logger.Warn("fix.worktree.fork-only-push", "task_id", t.ID, "err", err)
	}
	return wtPath, nil
}
