package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// TamperBlessedTag short-circuits the detector: a human who has reviewed a
// flagged diff and accepted it adds this tag, then moves the task back into the
// flow. Without it, re-dispatching a flagged task would re-scan the same
// committed diff and re-flag forever (livelock).
const TamperBlessedTag = "tamper-blessed"

// TamperFlaggedReasonPrefix is the status_reason prefix used when the detector
// flips a task to human-required for manual review.
const TamperFlaggedReasonPrefix = "possible test tampering — needs human bless before review:"

// IsTamperFlaggedReason reports whether a human-required status reason was
// produced by the tamper detector.
func IsTamperFlaggedReason(reason string) bool {
	return strings.HasPrefix(reason, TamperFlaggedReasonPrefix)
}

// tamperCategory classifies a changed file by the role it plays in the
// project's verification surface. Only test/snapshot/fixture/ci files are
// scanned for tampering; everything else is treated as implementation.
type tamperCategory string

const (
	tamperCatTest     tamperCategory = "test"
	tamperCatSnapshot tamperCategory = "snapshot"
	tamperCatFixture  tamperCategory = "fixture"
	tamperCatCI       tamperCategory = "ci"
	tamperCatOther    tamperCategory = "other"
)

// tamperSeverity grades a finding. A high finding blocks the task (flips it to
// human-required); medium is recorded for audit / reviewer context only.
const (
	tamperHigh   = "high"
	tamperMedium = "medium"
)

// tamperMaxScannedFiles bounds the number of per-file `git diff` subprocesses
// run on a pathological diff. Verification files past this cap still appear in
// the report (and produce a coarse medium finding) but are not scanned in
// detail.
const tamperMaxScannedFiles = 60

// tamperMaxReasonFindings caps how many high findings are spelled out in the
// human-required status reason before it is elided.
const tamperMaxReasonFindings = 5

// tamperFinding is one suspicious edit detected in a verification file.
type tamperFinding struct {
	File     string `json:"file"`
	Category string `json:"category"`
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Detail   string `json:"detail,omitempty"`
}

// tamperReport is the structured result of inspecting a task's diff. Stored as
// a generic artifact for audit; never surfaced to a public destination without
// scrubbing first.
type tamperReport struct {
	TaskID   string          `json:"taskId"`
	Base     string          `json:"base"`
	Range    string          `json:"range"`
	Files    []string        `json:"files"`
	Findings []tamperFinding `json:"findings"`
}

func (r tamperReport) highCount() int {
	n := 0
	for i := range r.Findings {
		if r.Findings[i].Severity == tamperHigh {
			n++
		}
	}
	return n
}

// tamperChange is one entry of `git diff --name-status`, optionally enriched
// with the file's unified-diff patch (empty for deletions and ignored files).
type tamperChange struct {
	Status string // "A", "M", "D", "R100", …
	Path   string
	Patch  string
	// FullContent is the post-change file content, when available. Used to
	// tell an added t.Skip (etc.) that follows an existing, repo-established
	// idiom apart from a genuinely novel skip — see isEstablishedSkipIdiom.
	FullContent string
}

type tamperPatchResult struct {
	Findings  []tamperFinding
	AddAssert int
	DelAssert int
}

var (
	// tamperTestPathRe matches test source files by filename convention across
	// Go, Python, and JS/TS.
	tamperTestPathRe = regexp.MustCompile(
		`(^|/)([^/]+_test\.go|test_[^/]+\.py|[^/]+_test\.py|[^/]+\.(test|spec)\.[jt]sx?)$`)
	// tamperTestDirRe matches files that live under a conventional test dir.
	tamperTestDirRe = regexp.MustCompile(`(^|/)(tests?|__tests__|specs?)/`)
	// tamperSnapshotRe matches snapshot / golden fixtures.
	tamperSnapshotRe = regexp.MustCompile(`(^|/)__snapshots__/|\.snap$|\.golden$|(^|/)testdata/`)
	// tamperFixtureRe matches fixture directories.
	tamperFixtureRe = regexp.MustCompile(`(^|/)fixtures?/`)
	// tamperCIPathRe matches CI / quality-gate config whose edit can neuter the
	// checks themselves.
	tamperCIPathRe = regexp.MustCompile(
		`(^|/)\.github/workflows/|(^|/)\.gitlab-ci\.yml$|(^|/)\.sybra\.yaml$|` +
			`(^|/)\.golangci\.ya?ml$|(^|/)\.pre-commit-config\.yaml$|(^|/)Makefile$|(^|/)mise\.toml$`)
)

// classifyTamperPath maps a repo-relative path to its verification category.
func classifyTamperPath(path string) tamperCategory {
	switch {
	case tamperTestPathRe.MatchString(path), tamperTestDirRe.MatchString(path):
		return tamperCatTest
	case tamperSnapshotRe.MatchString(path):
		return tamperCatSnapshot
	case tamperFixtureRe.MatchString(path):
		return tamperCatFixture
	case tamperCIPathRe.MatchString(path):
		return tamperCatCI
	default:
		return tamperCatOther
	}
}

var (
	// tamperAddedSkipRe matches added lines that disable a test outright.
	tamperAddedSkipRe = regexp.MustCompile(
		`\bt\.Skip(Now|f)?\s*\(|\bt\.SkipNow\b|` + // Go
			`@pytest\.mark\.(skip|skipif|xfail)|\bpytest\.skip\s*\(|` + // pytest
			`@(unittest\.)?skip(Unless|If)?\s*\(|\braise\s+SkipTest\b|` + // unittest
			`\b(it|describe|test|context)\.skip\s*\(|\bx(it|describe|test)\s*\(|` + // jest/mocha/vitest
			`\b(test|it)\.todo\s*\(|@(Ignore|Disabled)\b`) // todo / JUnit
	// tamperAddedExitRe matches an added forced success exit.
	tamperAddedExitRe = regexp.MustCompile(
		`\bos\.Exit\s*\(\s*0\s*\)|\bsys\.exit\s*\(\s*0\s*\)|\bprocess\.exit\s*\(\s*0\s*\)|(^|[^.\w])exit\s*\(\s*0\s*\)`)
	// tamperBuildIgnoreRe matches an added Go build-ignore tag (excludes the
	// file from the build, so its tests stop running).
	tamperBuildIgnoreRe = regexp.MustCompile(`^//go:build\s+ignore\b|^// \+build\s+ignore\b`)
	// tamperAssertionRe matches an assertion line. Counted on both sides of the
	// diff so a net removal (deletions > additions) flags weakened coverage
	// without false-positiving on pure refactors/renames. Bare `require` is
	// deliberately excluded: it matches JS `require("dep")` / Ruby
	// `require "spec_helper"` imports, whose removal is not a coverage loss —
	// only `require.` (testify-style assertions) counts.
	tamperAssertionRe = regexp.MustCompile(
		`\bassert\b|\brequire\.|\bexpect\s*\(|` +
			`\b(?:t|tb)\.(Error|Errorf|Fatal|Fatalf)\b|\bEXPECT_|\bASSERT_|\.should\b|\bshould\.`)
	// tamperTestDeclRe matches a test-case declaration. Net removal flags
	// deleted test cases.
	tamperTestDeclRe = regexp.MustCompile(
		`\bfunc\s+(Test|Benchmark|Fuzz|Example)[A-Z_0-9]|\bdef\s+test_|\b(it|test|describe)\s*\(`)
	// tamperCINeuterRe matches an added CI/quality-gate line that defeats the
	// gate without failing it. Scoped to tamperCatCI files.
	tamperCINeuterRe = regexp.MustCompile(
		`continue-on-error:\s*true|allow_failure:\s*true|\|\|\s*true\b|\bif:\s*false\b|\bexit\s+0\b`)
	// tamperCIRunRe matches a CI line that runs the project's checks. A net
	// removal of such lines in a CI file flags a deleted gate (e.g. dropping
	// the `run: go test` step) even when no neuter token is added.
	tamperCIRunRe = regexp.MustCompile(
		`(?i)\b(go test|go vet|npm (?:run )?(?:test|lint|check|typecheck)|` +
			`pnpm (?:run )?(?:test|lint)|yarn (?:test|lint)|pytest|jest|vitest|` +
			`golangci-lint|nilaway|make (?:test|lint|check)|cargo (?:test|clippy)|` +
			`ctest|tox|svelte-check|tsc)\b`)
	// tamperTautoExpectRe matches a jest/vitest expect(...).toBe(...) pair; the
	// two operands are compared in detectTautology.
	tamperTautoExpectRe = regexp.MustCompile(
		`expect\s*\(([^()]+)\)\s*\.\s*(?:toBe|toEqual|toStrictEqual)\s*\(([^()]+)\)`)
	// tamperTautoCallRe matches an equality-assertion call with a flat (no
	// nested parens) argument list.
	tamperTautoCallRe = regexp.MustCompile(
		`\b(?:assertEqual|assertEquals|assertSame|Equal|Equalf)\s*\(([^()]*)\)`)
	// tamperTautoCmpRe matches a bare `assert`/`if` equality comparison.
	tamperTautoCmpRe = regexp.MustCompile(
		`\b(?:assert|if)\s+(.+?)\s*={2,3}\s*(.+?)\s*[:{]?\s*$`)
)

// isEstablishedSkipIdiom reports whether the added skip line is already a
// repeated, pre-existing idiom in the file rather than a novel tampering
// attempt: an identical (trimmed) line appears at least twice in the
// post-change file content — once for the newly added occurrence and at
// least once more already established elsewhere (e.g. the repo-wide
// `if !hasGit() { t.Skip("git not available") }` guard). fileContent is the
// full post-change file; empty when unavailable, in which case the added
// line is always treated as novel (fail toward flagging, not toward silence).
func isEstablishedSkipIdiom(addedLine, fileContent string) bool {
	target := strings.TrimSpace(addedLine)
	if target == "" || fileContent == "" {
		return false
	}
	count := 0
	for line := range strings.SplitSeq(fileContent, "\n") {
		if strings.TrimSpace(line) == target {
			count++
			if count >= 2 {
				return true
			}
		}
	}
	return false
}

// looksLikeComment reports whether a (diff-stripped) line is a source comment.
// Commenting code out adds nothing of substance, so commented additions are
// ignored — a commented-out assertion then counts as a removal, not an offset.
func looksLikeComment(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "* ") ||
		strings.HasPrefix(t, "<!--")
}

// detectTautology reports whether an added assertion compares a value to
// itself (e.g. require.Equal(t, got, got), expect(x).toBe(x), assert a == a) —
// a near-unambiguous way to make a test pass without testing anything. Pure
// literal hardcoding (expected 5 → 4) is NOT detectable here; that is the
// baseline-suite's job (see issue #1058 follow-up).
func detectTautology(content string) bool {
	if m := tamperTautoExpectRe.FindStringSubmatch(content); len(m) == 3 && eqOperand(m[1], m[2]) {
		return true
	}
	if m := tamperTautoCallRe.FindStringSubmatch(content); len(m) == 2 {
		args := splitTopArgs(m[1])
		if n := len(args); n >= 2 && eqOperand(args[n-1], args[n-2]) {
			return true
		}
	}
	if m := tamperTautoCmpRe.FindStringSubmatch(content); len(m) == 3 && eqOperand(m[1], m[2]) {
		return true
	}
	return false
}

func eqOperand(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && a == b
}

func splitTopArgs(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// scanTamperPatch inspects one file's unified diff for high-severity tampering
// signals. Pure — no git, no IO — so it is exhaustively unit-tested.
//
// Hunk state is tracked explicitly: `---`/`+++` are file headers only before
// the first `@@`, so a removed/added line whose content itself begins with
// `--`/`++` is still classified by its leading diff marker rather than mistaken
// for a header. Comment-only additions are ignored, so commenting out a test or
// assertion registers as a removal (the deletion side) with no offsetting add.
func scanTamperPatch(path string, cat tamperCategory, patch, fileContent string) []tamperFinding {
	return scanTamperPatchResult(path, cat, patch, fileContent).Findings
}

func scanTamperPatchResult(path string, cat tamperCategory, patch, fileContent string) tamperPatchResult {
	s := &tamperScan{path: path, cat: cat, isCI: cat == tamperCatCI, seen: map[string]bool{}, fileContent: fileContent}
	inHunk := false
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case isDiffHeaderLine(line):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk && (strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++")):
			// file header before any hunk; ignore
		case inHunk && strings.HasPrefix(line, "+"):
			s.feedAdded(line[1:])
		case inHunk && strings.HasPrefix(line, "-"):
			s.feedRemoved(line[1:])
		}
	}
	return s.finalize()
}

// isDiffHeaderLine reports whether a line is a git diff metadata header that
// resets hunk state (so a subsequent `---`/`+++` is a file header, not content).
func isDiffHeaderLine(line string) bool {
	for _, p := range []string{"diff --git ", "index ", "old mode ", "new mode ",
		"new file", "deleted file", "similarity ", "rename ", "copy ", "Binary "} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// tamperScan accumulates findings and net token counts while walking a patch.
type tamperScan struct {
	path        string
	cat         tamperCategory
	isCI        bool
	fileContent string
	seen        map[string]bool
	findings    []tamperFinding
	addAssert   int
	delAssert   int
	addDecl     int
	delDecl     int
	addRun      int
	delRun      int
}

func (s *tamperScan) add(rule, detail string) {
	if s.seen[rule] {
		return
	}
	s.seen[rule] = true
	s.findings = append(s.findings, tamperFinding{
		File: s.path, Category: string(s.cat), Severity: tamperHigh, Rule: rule, Detail: detail,
	})
}

func (s *tamperScan) feedAdded(content string) {
	// A build-ignore tag is syntactically a comment but a meaningful directive
	// that excludes the file from the build — check it before the comment skip.
	if tamperBuildIgnoreRe.MatchString(strings.TrimSpace(content)) {
		s.add("added-build-ignore", trimDiffLine(content))
		return
	}
	if looksLikeComment(content) {
		return
	}
	if tamperAddedSkipRe.MatchString(content) && !isEstablishedSkipIdiom(content, s.fileContent) {
		s.add("added-skip", trimDiffLine(content))
	}
	if tamperAddedExitRe.MatchString(content) {
		s.add("added-early-exit", trimDiffLine(content))
	}
	if s.isCI {
		if tamperCINeuterRe.MatchString(content) {
			s.add("ci-neutered", trimDiffLine(content))
		}
		if tamperCIRunRe.MatchString(content) {
			s.addRun++
		}
		return
	}
	if tamperAssertionRe.MatchString(content) {
		s.addAssert++
	}
	if tamperTestDeclRe.MatchString(content) {
		s.addDecl++
	}
	if detectTautology(content) {
		s.add("tautological-assertion", trimDiffLine(content))
	}
}

func (s *tamperScan) feedRemoved(content string) {
	if looksLikeComment(content) {
		return
	}
	if s.isCI {
		if tamperCIRunRe.MatchString(content) {
			s.delRun++
		}
		return
	}
	if tamperAssertionRe.MatchString(content) {
		s.delAssert++
	}
	if tamperTestDeclRe.MatchString(content) {
		s.delDecl++
	}
}

// finalize appends the net-removal findings (a deletion not offset by an
// addition signals weakened coverage / a removed gate) and returns the result.
func (s *tamperScan) finalize() tamperPatchResult {
	netFinding := func(rule, noun string, del, add int) {
		if net := del - add; net > 0 {
			s.findings = append(s.findings, tamperFinding{
				File: s.path, Category: string(s.cat), Severity: tamperHigh,
				Rule: rule, Detail: fmt.Sprintf("net %d %s removed", net, noun),
			})
		}
	}
	if s.isCI {
		netFinding("removed-ci-step", "check step(s)", s.delRun, s.addRun)
		return tamperPatchResult{Findings: s.findings}
	}
	netFinding("removed-assertions", "assertion line(s)", s.delAssert, s.addAssert)
	netFinding("removed-test-cases", "test declaration(s)", s.delDecl, s.addDecl)
	return tamperPatchResult{Findings: s.findings, AddAssert: s.addAssert, DelAssert: s.delAssert}
}

// buildTamperReport assembles the report from the parsed diff. Pure function:
// classification + per-file scan + medium fallback. No git access.
func buildTamperReport(taskID, base string, changes []tamperChange) tamperReport {
	report := tamperReport{TaskID: taskID, Base: base}
	scanned := 0
	totalAddedAssertions := 0
	for i := range changes {
		c := changes[i]
		cat := classifyTamperPath(c.Path)
		if cat == tamperCatOther {
			continue
		}
		report.Files = append(report.Files, c.Path)

		// Whole-file deletion of a verification file is itself high-severity.
		if strings.HasPrefix(c.Status, "D") {
			report.Findings = append(report.Findings, tamperFinding{
				File: c.Path, Category: string(cat), Severity: tamperHigh,
				Rule: "deleted-verification-file", Detail: string(cat) + " file deleted",
			})
			continue
		}
		if scanned >= tamperMaxScannedFiles {
			continue
		}
		scanned++
		res := scanTamperPatchResult(c.Path, cat, c.Patch, c.FullContent)
		totalAddedAssertions += res.AddAssert
		report.Findings = append(report.Findings, res.Findings...)
	}
	if totalAddedAssertions > 0 {
		report.Findings = downgradeRemovedAssertionOnlyFindings(report.Findings)
	}

	// Verification files changed but nothing high fired: record one medium
	// finding per file so the stored report and the reviewer still have the
	// context (benign test edits are normal for feature work and do not block).
	if report.highCount() == 0 && len(report.Findings) == 0 {
		for _, f := range report.Files {
			report.Findings = append(report.Findings, tamperFinding{
				File: f, Category: string(classifyTamperPath(f)),
				Severity: tamperMedium, Rule: "verification-file-changed",
			})
		}
	}
	return report
}

func downgradeRemovedAssertionOnlyFindings(findings []tamperFinding) []tamperFinding {
	out := findings[:0]
	for _, f := range findings {
		if f.Rule == "removed-assertions" {
			f.Severity = tamperMedium
		}
		out = append(out, f)
	}
	return out
}

// execDetectTampering inspects the worktree diff (base...HEAD) for reward-
// hacking / test-tampering signals: added skip/xfail markers, forced exit(0),
// Go build-ignore tags, commented-out or net-removed assertions and test
// cases, tautological self-comparisons, deleted verification files, and
// neutered CI gates. A high-severity finding flips the task to human-required
// so it cannot reach done without an explicit human bless; benign
// verification-file changes are recorded but do not block.
//
// Pure literal-value hardcoding (an expected `5` swapped to `4`) is NOT
// detectable from the diff alone — that is the baseline-suite's job, deferred
// to the issue #1058 follow-up.
//
// Short-circuits (no-op):
//   - task carries the tamper-blessed tag (human already accepted the diff →
//     returns "blessed" so re-dispatch does not re-flag the same commits)
//   - No WorktreeGetter configured / no worktree / no verification files
//     changed (returns "clean")
//
// Fail-open on git/context errors: verify_commits already gates broken
// worktrees to human-required, so a diff failure here is logged and passed
// through rather than double-flipping or stranding the task.
func (e *Engine) execDetectTampering(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if slices.Contains(t.Tags, TamperBlessedTag) {
		e.logger.Info("workflow.detect-tampering.blessed", "task_id", taskID)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "blessed"}, nil
	}
	if e.worktrees == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree getter configured"}, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree for task"}, nil
	}

	report, err := e.collectTamperReport(taskID, wtPath, t)
	if err != nil {
		// collectTamperReport wraps the per-step timeout/cancel into the
		// returned error, so check the chain (not e.ctx, which stays live when
		// only the inner shellTimeout fires).
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: context canceled"}, nil
		}
		e.logger.Warn("workflow.detect-tampering.diff-error", "task_id", taskID, "err", err)
		return StepOutput{StepID: step.ID, Status: "completed", Output: "clean"}, nil
	}

	if len(report.Files) == 0 {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "clean"}, nil
	}

	if e.recorder != nil {
		if data, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
			if recErr := e.recorder.PutGeneric(taskID, "tamper-report.json", step.ID, string(data)); recErr != nil {
				e.logger.Warn("workflow.detect-tampering.artifact", "task_id", taskID, "err", recErr)
			}
		}
	}

	if high := report.highCount(); high > 0 {
		reason := tamperReason(report)
		if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
			e.logger.Error("workflow.detect-tampering.status", "task_id", taskID, "err", statusErr)
		}
		e.logger.Warn("workflow.detect-tampering.flagged",
			"task_id", taskID, "high", high, "files", len(report.Files))
		return StepOutput{StepID: step.ID, Status: "completed", Output: "flagged"}, nil
	}

	e.logger.Info("workflow.detect-tampering.clean", "task_id", taskID, "files", len(report.Files))
	return StepOutput{StepID: step.ID, Status: "completed", Output: "clean"}, nil
}

// collectTamperReport runs git to gather the changed files and per-file patches
// for verification files, then delegates to the pure buildTamperReport.
func (e *Engine) collectTamperReport(taskID, wtPath string, t TaskInfo) (tamperReport, error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	base, rangeSpec := resolveTamperRange(ctx, wtPath, t)

	// core.quotePath=false keeps non-ASCII paths unquoted so classification and
	// the per-file diff pathspec below see the real filename.
	nsCmd := exec.CommandContext(ctx, "git", "-c", "core.quotePath=false", "diff", "--name-status", rangeSpec)
	nsCmd.Dir = wtPath
	nsOut, err := nsCmd.Output()
	if err != nil {
		// Surface the per-step timeout/cancel through the error chain so the
		// caller can distinguish it from a genuine git failure.
		if ctx.Err() != nil {
			return tamperReport{}, fmt.Errorf("git diff --name-status: %w", ctx.Err())
		}
		return tamperReport{}, fmt.Errorf("git diff --name-status: %w", err)
	}

	changes := parseNameStatus(string(nsOut))
	fetched := 0
	for i := range changes {
		c := &changes[i]
		if classifyTamperPath(c.Path) == tamperCatOther || strings.HasPrefix(c.Status, "D") {
			continue
		}
		// Honour the same cap buildTamperReport scans under, so a pathological
		// diff cannot spawn an unbounded number of per-file `git diff` calls.
		if fetched >= tamperMaxScannedFiles {
			break
		}
		fetched++
		patch, pErr := gitFilePatch(ctx, wtPath, rangeSpec, c.Path)
		if pErr != nil {
			if ctx.Err() != nil {
				return tamperReport{}, fmt.Errorf("git diff %s: %w", c.Path, ctx.Err())
			}
			e.logger.Warn("workflow.detect-tampering.file-patch", "task_id", taskID, "file", c.Path, "err", pErr)
			continue
		}
		c.Patch = patch
		// The worktree's working tree is checked out at HEAD, the "new" side of
		// rangeSpec in both resolveTamperRange branches, so the on-disk file is
		// the post-change content — read it to tell an established skip idiom
		// (already used elsewhere in the file) apart from a genuinely novel one.
		if content, rErr := os.ReadFile(filepath.Join(wtPath, c.Path)); rErr == nil {
			c.FullContent = string(content)
		}
	}
	report := buildTamperReport(taskID, base, changes)
	report.Range = rangeSpec
	return report, nil
}

func tamperBaselineVar(stepID string) string {
	return "step." + stepID + ".tamper_base"
}

func resolveTamperRange(ctx context.Context, wtPath string, t TaskInfo) (base, rangeSpec string) {
	if t.Workflow != nil {
		if stepID := t.Workflow.LastAgentStepID(); stepID != "" {
			if sha := strings.TrimSpace(t.Workflow.Variables[tamperBaselineVar(stepID)]); sha != "" {
				cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", sha+"^{commit}")
				cmd.Dir = wtPath
				if cmd.Run() == nil {
					return sha, sha + "..HEAD"
				}
			}
		}
	}
	base = resolveOriginBase(ctx, wtPath)
	return base, base + "...HEAD"
}

func gitFilePatch(ctx context.Context, wtPath, rangeSpec, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-c", "core.quotePath=false", "diff", rangeSpec, "--", path)
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseNameStatus parses `git diff --name-status` output into changes. Renames
// and copies (R/C) report old and new tab-separated paths; the new path (last
// field) is used.
func parseNameStatus(out string) []tamperChange {
	var changes []tamperChange
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		changes = append(changes, tamperChange{
			Status: strings.TrimSpace(fields[0]),
			Path:   strings.TrimSpace(fields[len(fields)-1]),
		})
	}
	return changes
}

// tamperReason builds the human-required status reason from high findings.
func tamperReason(r tamperReport) string {
	var b strings.Builder
	b.WriteString(TamperFlaggedReasonPrefix + " ")
	shown := 0
	for i := range r.Findings {
		f := r.Findings[i]
		if f.Severity != tamperHigh {
			continue
		}
		if shown > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s in %s", f.Rule, f.File)
		if f.Detail != "" {
			fmt.Fprintf(&b, " (%s)", f.Detail)
		}
		shown++
		if shown >= tamperMaxReasonFindings {
			b.WriteString("; …")
			break
		}
	}
	return b.String()
}

// trimDiffLine normalizes a diff content line for inclusion in a finding:
// trimmed and capped to a sane length.
func trimDiffLine(s string) string {
	s = strings.TrimSpace(s)
	const maxLen = 120
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
