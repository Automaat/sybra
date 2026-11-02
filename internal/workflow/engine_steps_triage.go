package workflow

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const (
	triageVerdictSkip   = "skip"
	triageVerdictReview = "review"
)

// triageReviewShortStatRe parses the trailing summary of `git diff --shortstat`,
// e.g. ` 5 files changed, 23 insertions(+), 12 deletions(-)`.
var triageReviewShortStatRe = regexp.MustCompile(
	`(\d+)\s+files?\s+changed(?:,\s+(\d+)\s+insertions?\(\+\))?(?:,\s+(\d+)\s+deletions?\(-\))?`,
)

// triageVerdict returns triageVerdictSkip for a zero-net-line diff (rename-only,
// mode change), which has nothing to review, and triageVerdictReview otherwise.
// No files reported means the diff could not be read, so it reviews rather than
// silently skipping.
func triageVerdict(files []string, insertions, deletions int) (verdict, reason string) {
	if len(files) == 0 {
		return triageVerdictReview, "no files reported"
	}
	if total := insertions + deletions; total > 0 {
		return triageVerdictReview, fmt.Sprintf("%d lines, %d files", total, len(files))
	}
	return triageVerdictSkip, fmt.Sprintf("0 lines changed across %d file(s)", len(files))
}

// execTriageReview decides whether a diff needs a review at all. Pure mechanical
// — no LLM call. Output is triageVerdict's result, so a downstream condition step
// can route via vars.step.<id>.output.
//
// Two diff sources, in order:
//  1. Worktree (preferred): git diff <base>...HEAD against the resolved
//     origin base. Used by simple-task after verify_commits.
//  2. GitHub PR: gh pr diff --name-only + gh pr diff. Used by pr-review
//     when the task is tagged `review` on an existing PR.
//
// Fail-safe: any unrecoverable error (no worktree, no PR, git/gh failure)
// reviews rather than silently skipping.
func (e *Engine) execTriageReview(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	files, ins, del, src, err := e.collectTriageDiff(taskID, t)
	if err != nil {
		e.logger.Warn("workflow.triage-review.diff-error", "task_id", taskID, "src", src, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: triageVerdictReview}, nil
	}
	verdict, reason := triageVerdict(files, ins, del)
	e.logger.Info("workflow.triage-review",
		"task_id", taskID,
		"verdict", verdict,
		"src", src,
		"files", len(files),
		"insertions", ins,
		"deletions", del,
		"reason", reason,
	)
	return StepOutput{StepID: step.ID, Status: "completed", Output: verdict}, nil
}

// collectTriageDiff gathers (files, insertions, deletions, source) for the
// triage heuristic. Tries the worktree first, falls back to gh pr diff when
// the task has a PR but no worktree (e.g. the standalone pr-review workflow).
func (e *Engine) collectTriageDiff(taskID string, t TaskInfo) (files []string, insertions, deletions int, source string, err error) {
	if e.execution.Worktrees != nil {
		if wtPath, ok := e.execution.Worktrees.GetWorktreePath(taskID); ok {
			files, insertions, deletions, err = e.gitTriageDiff(wtPath)
			if err == nil {
				return files, insertions, deletions, "worktree", nil
			}
			// Worktree present but git failed — surface the error. Don't fall
			// through to gh: a broken worktree shouldn't silently flip to PR
			// mode and mask the real problem.
			return nil, 0, 0, "worktree", err
		}
	}
	if t.PRNumber > 0 && t.ProjectID != "" {
		files, insertions, deletions, err = e.ghTriageDiff(t.ProjectID, t.PRNumber)
		return files, insertions, deletions, "pr", err
	}
	return nil, 0, 0, "none", fmt.Errorf("no worktree and no pr to inspect")
}

// gitTriageDiff runs `git diff <base>...HEAD` against the worktree to extract
// changed files and the insertion/deletion counts.
func (e *Engine) gitTriageDiff(wtPath string) (files []string, insertions, deletions int, err error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	base := resolveOriginBase(ctx, wtPath)
	rangeSpec := base + "...HEAD"

	nameOut, err := gitStdout(ctx, wtPath, "diff", "--name-only", rangeSpec)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("git diff --name-only: %w", err)
	}

	statOut, err := gitStdout(ctx, wtPath, "diff", "--shortstat", rangeSpec)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("git diff --shortstat: %w", err)
	}

	files = splitNonEmptyLines(nameOut)
	insertions, deletions = parseShortStat(statOut)
	return files, insertions, deletions, nil
}

// ghTriageDiff queries GitHub for the PR diff when there's no worktree.
// Uses `gh pr diff --name-only` for the file list and `gh pr diff --patch` to
// count insertions/deletions from the unified diff body.
func (e *Engine) ghTriageDiff(repo string, pr int) (files []string, insertions, deletions int, err error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	nameCmd := exec.CommandContext(ctx, "bash", "-c",
		"gh pr diff \"$_PR\" --repo \"$_REPO\" --name-only")
	nameCmd.Env = append(nameCmd.Environ(),
		"_REPO="+repo,
		fmt.Sprintf("_PR=%d", pr),
	)
	nameOut, err := nameCmd.Output()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("gh pr diff --name-only: %w", err)
	}

	patchCmd := exec.CommandContext(ctx, "bash", "-c",
		"gh pr diff \"$_PR\" --repo \"$_REPO\" --patch")
	patchCmd.Env = append(patchCmd.Environ(),
		"_REPO="+repo,
		fmt.Sprintf("_PR=%d", pr),
	)
	patchOut, err := patchCmd.Output()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("gh pr diff --patch: %w", err)
	}

	files = splitNonEmptyLines(string(nameOut))
	insertions, deletions = countPatchLines(string(patchOut))
	return files, insertions, deletions, nil
}

func splitNonEmptyLines(s string) []string {
	out := make([]string, 0)
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// parseShortStat extracts insertion and deletion counts from
// `git diff --shortstat` summary lines.
func parseShortStat(s string) (insertions, deletions int) {
	m := triageReviewShortStatRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0
	}
	insertions, _ = strconv.Atoi(m[2])
	deletions, _ = strconv.Atoi(m[3])
	return insertions, deletions
}

// countPatchLines walks a unified diff body and counts insertion / deletion
// lines, ignoring the `+++`/`---` file headers.
func countPatchLines(patch string) (insertions, deletions int) {
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			continue
		case strings.HasPrefix(line, "+"):
			insertions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return insertions, deletions
}
