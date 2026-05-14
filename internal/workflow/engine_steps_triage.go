package workflow

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// triageReviewLineLimit and triageReviewFileLimit are the hard ceilings under
// which a diff is eligible for the lightweight `/pr-review` skill. Anything
// larger gets the deeper `/staff-code-review`. Tuned by gut; revisit after a
// week of real use.
const (
	triageReviewLineLimit = 50
	triageReviewFileLimit = 3
)

// triageReviewRiskyPathRe matches paths whose changes warrant staff-level
// review regardless of size: workflow/agent core, entrypoints, tests, CI,
// container build. A 5-line tweak inside internal/workflow can break the
// engine for every task — exactly what staff review is for.
var triageReviewRiskyPathRe = regexp.MustCompile(
	`(^|/)(internal/(workflow|agent|sybra)/|cmd/|main\.go$|.*_test\.go$|Dockerfile|\.github/workflows/)`,
)

// triageReviewSimpleExts lists the file extensions a "simple" review can
// safely cover. Anything outside this set (e.g. binary, .sql, .proto) is
// unfamiliar enough to warrant the deeper review.
var triageReviewSimpleExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".svelte": true,
	".md": true, ".yaml": true, ".yml": true, ".json": true,
	".css": true, ".html": true, ".txt": true,
}

// triageReviewShortStatRe parses the trailing summary of `git diff --shortstat`,
// e.g. ` 5 files changed, 23 insertions(+), 12 deletions(-)`.
var triageReviewShortStatRe = regexp.MustCompile(
	`(\d+)\s+files?\s+changed(?:,\s+(\d+)\s+insertions?\(\+\))?(?:,\s+(\d+)\s+deletions?\(-\))?`,
)

// triageVerdict applies the heuristic and returns "simple" or "staff" with a
// short rationale. Pure function — easy to unit-test without git.
func triageVerdict(files []string, insertions, deletions int) (verdict, reason string) {
	total := insertions + deletions
	if len(files) == 0 {
		return "staff", "no files reported"
	}
	if total > triageReviewLineLimit {
		return "staff", fmt.Sprintf("%d lines > %d", total, triageReviewLineLimit)
	}
	if len(files) > triageReviewFileLimit {
		return "staff", fmt.Sprintf("%d files > %d", len(files), triageReviewFileLimit)
	}
	for _, f := range files {
		if triageReviewRiskyPathRe.MatchString(f) {
			return "staff", "risky path: " + f
		}
		ext := strings.ToLower(filepathExt(f))
		if !triageReviewSimpleExts[ext] {
			label := ext
			if label == "" {
				label = "(no extension)"
			}
			return "staff", "unsupported extension: " + label + " (" + f + ")"
		}
	}
	return "simple", fmt.Sprintf("%d lines, %d files, low risk", total, len(files))
}

// filepathExt returns the file extension including the dot, or "" when none.
// Inlined to avoid pulling path/filepath into this file just for one call.
func filepathExt(name string) string {
	for i := len(name) - 1; i >= 0 && name[i] != '/'; i-- {
		if name[i] == '.' {
			return name[i:]
		}
	}
	return ""
}

// execTriageReview decides between the lightweight `/pr-review` skill and the
// deeper `/staff-code-review` skill based on the diff's size and shape. Pure
// mechanical — no LLM call. Returns Output set to "simple" or "staff" so a
// downstream condition step can route via vars.step.<id>.output.
//
// Two diff sources, in order:
//  1. Worktree (preferred): git diff <base>...HEAD against the resolved
//     origin base. Used by simple-task after verify_commits.
//  2. GitHub PR: gh pr diff --name-only + gh pr diff. Used by pr-review
//     when the task is tagged `review` on an existing PR.
//
// Fail-safe: any unrecoverable error (no worktree, no PR, git/gh failure)
// returns "staff" so we err toward the deeper review rather than silently
// shipping a thin one.
func (e *Engine) execTriageReview(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	files, ins, del, src, err := e.collectTriageDiff(taskID, t)
	if err != nil {
		e.logger.Warn("workflow.triage-review.diff-error", "task_id", taskID, "src", src, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "staff"}, nil
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
	if e.worktrees != nil {
		if wtPath, ok := e.worktrees.GetWorktreePath(taskID); ok {
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

	nameCmd := exec.CommandContext(ctx, "git", "diff", "--name-only", rangeSpec)
	nameCmd.Dir = wtPath
	nameOut, err := nameCmd.Output()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("git diff --name-only: %w", err)
	}

	statCmd := exec.CommandContext(ctx, "git", "diff", "--shortstat", rangeSpec)
	statCmd.Dir = wtPath
	statOut, err := statCmd.Output()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("git diff --shortstat: %w", err)
	}

	files = splitNonEmptyLines(string(nameOut))
	insertions, deletions = parseShortStat(string(statOut))
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
