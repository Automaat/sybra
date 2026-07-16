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
// which a diff is eligible for the lightweight `/adversarial-review` skill.
// Anything larger gets the deeper `/staff-code-review`. These were 50 lines /
// 3 files and produced a staff verdict on 62 of 62 real reviews — the
// lightweight branch never executed once. A routine feature here lands ~300
// lines across ~10 files, so the caps sit above that and staff is reserved for
// diffs that are genuinely large.
const (
	triageReviewLineLimit = 400
	triageReviewFileLimit = 15
)

// triageReviewRiskyPathRe matches paths whose changes warrant staff-level
// review regardless of size: workflow/agent core, CI, container build. A
// 5-line tweak inside internal/workflow can break the engine for every task —
// exactly what staff review is for. Deliberately absent: `*_test.go`, because
// every real change ships a test and this alone forced staff on most diffs
// (the tampering concern it existed for is already covered mechanically by the
// detect_tampering step, which re-scans the full diff after review fixes
// land); `internal/sybra/`, the god package that nearly every change touches,
// so treating it as risky means treating everything as risky; and `cmd/` +
// `main.go`, thin entrypoint wiring whose logic is covered by the package
// rules above.
var triageReviewRiskyPathRe = regexp.MustCompile(
	`(^|/)(internal/(workflow|agent)/|Dockerfile|\.github/workflows/)`,
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

// triageReviewDocsExts lists file extensions that are pure documentation/prose
// with no executable meaning — always safe for the trivial fast-path unless
// carved out (see triageReviewCarveOutRe).
var triageReviewDocsExts = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".rst": true, ".adoc": true,
}

// triageReviewDepBumpBasenames lists manifest/lockfile basenames whose diffs
// are dependency-version bumps (e.g. Renovate) — mechanical, low-risk churn.
var triageReviewDepBumpBasenames = map[string]bool{
	"go.mod": true, "go.sum": true,
	"package.json": true, "package-lock.json": true,
	"yarn.lock": true, "pnpm-lock.yaml": true,
}

// triageReviewGeneratedRe matches generated/derived files: Wails bindings and
// protobuf/codegen output. Drift is already CI-enforced elsewhere.
var triageReviewGeneratedRe = regexp.MustCompile(
	`(^|/)frontend/bindings/|\.pb\.go$|_gen\.go$|\.gen\.go$`,
)

// triageReviewCarveOutRe matches paths that look docs-shaped but actually
// govern agent behavior (skills, CLAUDE.md, orchestrator prompts) — these
// must never get the trivial fast-path even though they end in .md.
var triageReviewCarveOutRe = regexp.MustCompile(
	`(^|/)\.claude/skills/|(^|/)CLAUDE\.md$|(^|/)SKILL\.md$|(^|/)orchestrator/`,
)

type trivialFileClass uint8

const (
	trivialFileNone trivialFileClass = iota
	trivialFileDocs
	trivialFileDep
	trivialFileGenerated
)

// classifyTrivialFile reports whether a single changed file belongs to a
// mechanically-safe class (docs, dependency bump, generated) and is not
// carved out as agent-behavior content that still warrants staff review.
//
// Docs-extension matching is deliberately case-sensitive (unlike the
// unsupported-extension downgrade further down, which lowercases): an
// unusual-cased extension (e.g. "FOO.MD") is unfamiliar enough that it
// should not silently bypass the line/file-count caps via this fast-path,
// even though it may still end up "simple" through the existing
// slower/cap-enforcing path.
func classifyTrivialFile(path string) trivialFileClass {
	if triageReviewCarveOutRe.MatchString(path) {
		return trivialFileNone
	}
	if triageReviewGeneratedRe.MatchString(path) {
		return trivialFileGenerated
	}
	base := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		base = path[i+1:]
	}
	if triageReviewDepBumpBasenames[base] {
		return trivialFileDep
	}
	if triageReviewDocsExts[filepathExt(path)] {
		return trivialFileDocs
	}
	return trivialFileNone
}

// triageVerdict applies the heuristic and returns "skip", "simple", or "staff"
// with a short rationale. Pure function — easy to unit-test without git.
//
// "skip"   — trivial diff (zero net lines changed): no review needed.
// "simple" — small, low-risk diff: lightweight /adversarial-review is sufficient.
// "staff"  — large or risky diff: full /staff-code-review required.
//
// Ordering: the pure-docs fast-path below runs BEFORE the risky-path gate
// and the size/file-count caps, and deliberately bypasses all of them —
// carve-out paths (skills/CLAUDE.md/orchestrator) are excluded from the
// docs class itself, so this cannot leak agent-behavior content. Dependency-
// manifest and generated-only diffs still have to pass the existing
// size/file-count/risky-path gates before they can route to the lightweight
// review.
func triageVerdict(files []string, insertions, deletions int) (verdict, reason string) {
	total := insertions + deletions
	if len(files) == 0 {
		return "staff", "no files reported"
	}
	// Zero net lines (e.g. rename-only, mode-change): no substantive diff to
	// review. Skip the review cycle entirely.
	if total == 0 {
		return "skip", fmt.Sprintf("0 lines changed across %d file(s)", len(files))
	}
	// Pure docs bypass BOTH the risky-path gate below and the size/file-count
	// caps further down: classifyTrivialFile already excludes carve-out paths
	// (.claude/skills/, CLAUDE.md, SKILL.md, orchestrator/), so a file only
	// reaches trivialFileDocs here if it is genuinely prose with no bearing on
	// agent/workflow behavior — even when it lives under a risky path prefix
	// like internal/workflow/. The downgrade target is still a real
	// lightweight review, not skip.
	allDocs := true
	for _, f := range files {
		if classifyTrivialFile(f) != trivialFileDocs {
			allDocs = false
			break
		}
	}
	if allDocs {
		return "simple", fmt.Sprintf("%d docs file(s)", len(files))
	}
	for _, f := range files {
		// Keep the carve-out/risky-path regex aligned between the trivial-file
		// classifier above and the hard staff gate here.
		if isRiskyReviewPath(f) || triageReviewCarveOutRe.MatchString(f) {
			return "staff", "risky path: " + f
		}
	}
	if total > triageReviewLineLimit {
		return "staff", fmt.Sprintf("%d lines > %d", total, triageReviewLineLimit)
	}
	if len(files) > triageReviewFileLimit {
		return "staff", fmt.Sprintf("%d files > %d", len(files), triageReviewFileLimit)
	}
	for _, f := range files {
		class := classifyTrivialFile(f)
		if class == trivialFileDep || class == trivialFileGenerated {
			continue
		}
		ext := strings.ToLower(filepathExt(f))
		if !triageReviewSimpleExts[ext] {
			// Unsupported extension on a small, non-risky diff: downgrade to
			// simple instead of forcing staff. The extension is unfamiliar but
			// the diff is tiny — a lightweight review is still better than
			// nothing, and staff is reserved for genuinely large/risky changes.
			label := ext
			if label == "" {
				label = "(no extension)"
			}
			return "simple", "small diff with unsupported extension: " + label + " (" + f + ")"
		}
	}
	return "simple", fmt.Sprintf("%d lines, %d files, low risk", total, len(files))
}

func isRiskyReviewPath(path string) bool {
	// Generated Wails bindings embed Go package paths in their output path, so
	// regex matching against ".../internal/sybra/..." would be a false positive.
	if strings.HasPrefix(path, "frontend/bindings/") {
		return false
	}
	return triageReviewRiskyPathRe.MatchString(path)
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

// execTriageReview decides between the lightweight `/adversarial-review` skill
// and the deeper `/staff-code-review` skill based on the diff's size and shape.
// Pure mechanical — no LLM call. Returns Output set to "simple" or "staff" so a
// downstream condition step can route via vars.step.<id>.output. Only pr-review
// acts on the "staff" verdict; simple-task-review consumes "skip" and otherwise
// always reviews adversarially.
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
