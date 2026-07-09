package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

// Best-of-N promotion fails closed with one of these distinct sentinel
// errors rather than force-pushing or overwriting an existing PR branch.
// Callers (engine_steps_bestofn.go) turn each into a human-required reason.
var (
	// ErrPromotionHasPR means the task already has a linked PR — promoting
	// would move the branch a reviewer/CI is already looking at.
	ErrPromotionHasPR = errors.New("best-of-n promotion refused: task already has a PR")
	// ErrPromotionDirty means the winning attempt's worktree has uncommitted
	// changes — promoting an uncommitted tree would silently drop that work.
	ErrPromotionDirty = errors.New("best-of-n promotion refused: winner worktree is dirty")
	// ErrPromotionDiverged means the canonical branch already points somewhere
	// that is not an ancestor of the winner's HEAD — force-moving it would
	// discard commits Sybra never authored as an attempt.
	ErrPromotionDiverged = errors.New("best-of-n promotion refused: canonical branch diverged from winner")
)

// attemptDirName derives the isolated directory name for one best-of-N
// attempt, distinct from the task's canonical worktree directory.
func attemptDirName(t task.Task, attemptID string) string {
	return t.DirName() + "-" + attemptID
}

// PathForAttempt returns the isolated worktree path for one best-of-N attempt.
func (m *Manager) PathForAttempt(t task.Task, attemptID string) string {
	return filepath.Join(m.dir, attemptDirName(t, attemptID))
}

// attemptBranchName derives the isolated branch name for one attempt from the
// task's (eventual) canonical branch, so promotion only ever has to rename
// the winner's ref onto the canonical name rather than reconstruct it.
func attemptBranchName(t task.Task, canonicalBranch, attemptID string) string {
	base := canonicalBranch
	if base == "" {
		base = branchNameForTask(t)
	}
	return base + "-" + attemptID
}

// PrepareAttempt creates (or resumes) an isolated worktree + branch for one
// best-of-N implementation attempt. It deliberately mirrors PrepareForTask's
// new-worktree path but differs in two ways required for attempt isolation:
// it never writes task.Branch (the canonical branch is only set by
// PromoteAttempt, once a winner is chosen) and it never pushes upstream (the
// attempt branch is local-only until promoted).
//
// Resume-safe: if the attempt worktree already exists, is a healthy git
// checkout, and is on the expected branch, it is reused as-is rather than
// recreated — a process restart mid-fan-out must not discard an attempt's
// in-progress commits.
func (m *Manager) PrepareAttempt(ctx context.Context, t task.Task, attemptID string) (dir, branch string, err error) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", "", fmt.Errorf("get project: %w", err)
	}
	if err := project.FetchOrigin(ctx, proj.ClonePath); err != nil {
		return "", "", fmt.Errorf("fetch origin: %w", err)
	}
	defaultBranch, err := project.DefaultBranch(ctx, proj.ClonePath)
	if err != nil {
		return "", "", fmt.Errorf("default branch: %w", err)
	}
	baseRef := worktreeBaseRef(proj.WorktreeBaseRef, defaultBranch)

	wtPath := m.PathForAttempt(t, attemptID)
	wtBranch := attemptBranchName(t, t.Branch, attemptID)

	if _, statErr := os.Stat(wtPath); statErr == nil {
		if project.WorktreeHealthy(ctx, wtPath) {
			if cur, cErr := project.CurrentBranch(ctx, wtPath); cErr == nil && cur == wtBranch {
				m.logger.Info("worktree.attempt.resumed", "task_id", t.ID, "attempt", attemptID, "path", wtPath)
				return wtPath, wtBranch, nil
			}
		}
		if err := project.RemoveWorktreeReconcile(ctx, proj.ClonePath, wtPath); err != nil {
			return "", "", fmt.Errorf("remove stale attempt worktree: %w", err)
		}
	}

	switch {
	case project.BranchExists(ctx, proj.ClonePath, wtBranch):
		if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, wtPath, wtBranch); err != nil {
			return "", "", fmt.Errorf("checkout existing attempt branch %s: %w", wtBranch, err)
		}
	default:
		if err := project.CreateWorktree(ctx, proj.ClonePath, wtPath, wtBranch, baseRef); err != nil {
			return "", "", fmt.Errorf("create attempt worktree: %w", err)
		}
	}
	m.logger.Info("worktree.attempt.created", "task_id", t.ID, "attempt", attemptID, "path", wtPath, "branch", wtBranch)

	if err := project.SanitizeWorktree(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.attempt.sanitize", "task_id", t.ID, "attempt", attemptID, "err", err)
	}
	if err := m.runSetup(ctx, t.ID, wtPath, m.resolveSetupCommands(wtPath, proj)); err != nil {
		return "", "", fmt.Errorf("setup on attempt worktree: %w", err)
	}
	m.installChecks(ctx, wtPath, proj)
	// Attempts must never push — PromoteAttempt moves the canonical branch
	// locally against the bare clone once a winner is chosen. Best-effort: this
	// only blocks a plain `git push origin` when a fork remote exists; the
	// authoritative guarantee is that PrepareAttempt/PromoteAttempt themselves
	// never call project.PushUpstream/PushSync for an attempt branch.
	if err := project.EnforceForkOnlyPush(ctx, wtPath); err != nil {
		m.logger.Warn("worktree.attempt.fork-only-push", "task_id", t.ID, "attempt", attemptID, "err", err)
	}
	m.seedWorktree(ctx, t, wtPath, wtBranch)
	return wtPath, wtBranch, nil
}

// PromoteAttempt fast-forwards the task's canonical branch onto the winning
// attempt's HEAD and materializes the canonical worktree there.
//
// Idempotent: if the canonical branch already points at the winner's HEAD,
// promotion is a no-op except for re-materializing the canonical worktree —
// safe to call again after a crash mid-promotion.
//
// Fails closed (returns a sentinel the caller must turn into human-required)
// rather than force-pushing or clobbering an existing PR:
//   - ErrPromotionHasPR: the task already has a linked PR.
//   - ErrPromotionDirty: the winner's worktree has uncommitted changes.
//   - ErrPromotionDiverged: the canonical branch exists and is not an
//     ancestor of the winner's HEAD (would discard non-attempt commits).
func (m *Manager) PromoteAttempt(ctx context.Context, t task.Task, winnerDir, winnerBranch string) (canonicalDir string, err error) {
	if t.PRNumber != 0 {
		return "", fmt.Errorf("%w: PR #%d", ErrPromotionHasPR, t.PRNumber)
	}
	dirty, dErr := project.IsWorktreeDirty(ctx, winnerDir)
	if dErr != nil {
		return "", fmt.Errorf("check winner worktree dirty: %w", dErr)
	}
	if dirty {
		return "", ErrPromotionDirty
	}
	winnerHead, err := project.CurrentCommit(ctx, winnerDir)
	if err != nil {
		return "", fmt.Errorf("resolve winner HEAD: %w", err)
	}

	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}
	canonicalBranch := t.Branch
	if canonicalBranch == "" {
		canonicalBranch = branchNameForTask(t)
	}
	canonicalPath := m.PathFor(t)

	if canonicalHead, exists := project.ResolveBareRef(ctx, proj.ClonePath, "refs/heads/"+canonicalBranch); exists {
		if canonicalHead != winnerHead && !project.IsAncestorInBare(ctx, proj.ClonePath, canonicalHead, winnerHead) {
			return "", fmt.Errorf("%w: canonical %s vs winner %s", ErrPromotionDiverged, shortSHA(canonicalHead), shortSHA(winnerHead))
		}
	}

	if err := project.SetBranchTo(ctx, proj.ClonePath, canonicalBranch, winnerHead); err != nil {
		return "", fmt.Errorf("fast-forward canonical branch: %w", err)
	}
	m.logger.Info("worktree.attempt.promoted", "task_id", t.ID, "branch", canonicalBranch, "winner_branch", winnerBranch, "head", shortSHA(winnerHead))

	dir, err := m.materializeCanonicalWorktree(ctx, t, proj, canonicalPath, canonicalBranch)
	if err != nil {
		return "", err
	}
	m.ensureBranch(t, canonicalBranch)
	return dir, nil
}

// materializeCanonicalWorktree ensures the task's canonical worktree exists
// and is checked out at canonicalBranch's current tip (already moved by
// SetBranchTo). Reuses a healthy existing checkout via a hard reset rather
// than recreating it whenever possible.
func (m *Manager) materializeCanonicalWorktree(ctx context.Context, t task.Task, proj project.Project, canonicalPath, canonicalBranch string) (string, error) {
	if _, statErr := os.Stat(canonicalPath); statErr == nil {
		if project.WorktreeHealthy(ctx, canonicalPath) {
			if cur, cErr := project.CurrentBranch(ctx, canonicalPath); cErr == nil && cur == canonicalBranch {
				if err := project.HardResetWorktree(ctx, canonicalPath, canonicalBranch); err != nil {
					return "", fmt.Errorf("reset canonical worktree to promoted branch: %w", err)
				}
				m.installChecks(ctx, canonicalPath, proj)
				return canonicalPath, nil
			}
		}
		if err := project.RemoveWorktreeReconcile(ctx, proj.ClonePath, canonicalPath); err != nil {
			return "", fmt.Errorf("remove stale canonical worktree: %w", err)
		}
	}
	if err := project.CreateWorktreeExisting(ctx, proj.ClonePath, canonicalPath, canonicalBranch); err != nil {
		return "", fmt.Errorf("materialize canonical worktree: %w", err)
	}
	if err := project.SanitizeWorktree(ctx, canonicalPath); err != nil {
		m.logger.Warn("worktree.attempt.promote.sanitize", "task_id", t.ID, "err", err)
	}
	m.installChecks(ctx, canonicalPath, proj)
	m.seedWorktree(ctx, t, canonicalPath, canonicalBranch)
	return canonicalPath, nil
}

// CleanupAttempts best-effort removes every attempt worktree directory (and
// its branch) for the given attempt IDs. Called after a winner is promoted
// (loser cleanup) and after all attempts fail (full cleanup). Errors are
// logged, not returned — a leftover attempt directory is disk-space waste,
// not a correctness problem, and must never block workflow advancement.
func (m *Manager) CleanupAttempts(ctx context.Context, t task.Task, attemptIDs []string) {
	proj, err := m.projects.Get(t.ProjectID)
	if err != nil {
		m.logger.Warn("worktree.attempt.cleanup.get-project", "task_id", t.ID, "err", err)
		return
	}
	for _, attemptID := range attemptIDs {
		wtPath := m.PathForAttempt(t, attemptID)
		if _, statErr := os.Stat(wtPath); statErr != nil {
			continue
		}
		if err := project.RemoveWorktreeReconcile(ctx, proj.ClonePath, wtPath); err != nil {
			m.logger.Warn("worktree.attempt.cleanup", "task_id", t.ID, "attempt", attemptID, "path", wtPath, "err", err)
			continue
		}
		m.logger.Info("worktree.attempt.cleaned-up", "task_id", t.ID, "attempt", attemptID, "path", wtPath)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
