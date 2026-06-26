package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

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
	// without false-positiving on pure refactors/renames.
	tamperAssertionRe = regexp.MustCompile(
		`\bassert\b|\brequire\b|\bassert\.|\brequire\.|\bexpect\s*\(|` +
			`\bt\.(Error|Errorf|Fatal|Fatalf)\b|\bEXPECT_|\bASSERT_|\.should\b|\bshould\.`)
	// tamperTestDeclRe matches a test-case declaration. Net removal flags
	// deleted test cases.
	tamperTestDeclRe = regexp.MustCompile(
		`\bfunc\s+(Test|Benchmark|Fuzz|Example)[A-Z_0-9]|\bdef\s+test_|\b(it|test|describe)\s*\(`)
)

// scanTamperPatch inspects one file's unified diff for high-severity tampering
// signals. Pure — no git, no IO — so it is exhaustively unit-tested.
func scanTamperPatch(path string, cat tamperCategory, patch string) []tamperFinding {
	var findings []tamperFinding
	seen := map[string]bool{} // dedupe direct-match rules to one finding per file
	addAssert, delAssert := 0, 0
	addDecl, delDecl := 0, 0

	add := func(rule, detail string) {
		if seen[rule] {
			return
		}
		seen[rule] = true
		findings = append(findings, tamperFinding{
			File: path, Category: string(cat), Severity: tamperHigh, Rule: rule, Detail: detail,
		})
	}

	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"), strings.HasPrefix(line, "@@"):
			continue
		case strings.HasPrefix(line, "+"):
			content := line[1:]
			if tamperAddedSkipRe.MatchString(content) {
				add("added-skip", trimDiffLine(content))
			}
			if tamperAddedExitRe.MatchString(content) {
				add("added-early-exit", trimDiffLine(content))
			}
			if tamperBuildIgnoreRe.MatchString(strings.TrimSpace(content)) {
				add("added-build-ignore", trimDiffLine(content))
			}
			if tamperAssertionRe.MatchString(content) {
				addAssert++
			}
			if tamperTestDeclRe.MatchString(content) {
				addDecl++
			}
		case strings.HasPrefix(line, "-"):
			content := line[1:]
			if tamperAssertionRe.MatchString(content) {
				delAssert++
			}
			if tamperTestDeclRe.MatchString(content) {
				delDecl++
			}
		}
	}

	if net := delAssert - addAssert; net > 0 {
		findings = append(findings, tamperFinding{
			File: path, Category: string(cat), Severity: tamperHigh,
			Rule: "removed-assertions", Detail: fmt.Sprintf("net %d assertion line(s) removed", net),
		})
	}
	if net := delDecl - addDecl; net > 0 {
		findings = append(findings, tamperFinding{
			File: path, Category: string(cat), Severity: tamperHigh,
			Rule: "removed-test-cases", Detail: fmt.Sprintf("net %d test declaration(s) removed", net),
		})
	}
	return findings
}

// buildTamperReport assembles the report from the parsed diff. Pure function:
// classification + per-file scan + medium fallback. No git access.
func buildTamperReport(taskID, base string, changes []tamperChange) tamperReport {
	report := tamperReport{TaskID: taskID, Base: base}
	scanned := 0
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
		report.Findings = append(report.Findings, scanTamperPatch(c.Path, cat, c.Patch)...)
	}

	// Verification files changed but nothing high fired: record one medium
	// finding per file so the stored report and the reviewer still have the
	// context (benign test edits are normal for feature work and do not block).
	if report.highCount() == 0 {
		for _, f := range report.Files {
			report.Findings = append(report.Findings, tamperFinding{
				File: f, Category: string(classifyTamperPath(f)),
				Severity: tamperMedium, Rule: "verification-file-changed",
			})
		}
	}
	return report
}

// execDetectTampering inspects the worktree diff (base...HEAD) for reward-
// hacking / test-tampering signals. A high-severity finding flips the task to
// human-required so it cannot reach done without an explicit human bless;
// benign verification-file changes are recorded but do not block.
//
// Skip conditions (no-op, returns "clean"):
//   - No WorktreeGetter configured
//   - No worktree found for the task
//   - No verification files changed
//
// Fail-open on git/context errors: verify_commits already gates broken
// worktrees to human-required, so a diff failure here is logged and passed
// through rather than double-flipping or stranding the task.
func (e *Engine) execDetectTampering(taskID string, step *Step, _ TaskInfo) (StepOutput, error) {
	if e.worktrees == nil {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree getter configured"}, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return StepOutput{StepID: step.ID, Status: "completed", Output: "skipped: no worktree for task"}, nil
	}

	report, err := e.collectTamperReport(taskID, wtPath)
	if err != nil {
		if errors.Is(e.ctx.Err(), context.Canceled) || errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
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
func (e *Engine) collectTamperReport(taskID, wtPath string) (tamperReport, error) {
	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	base := resolveOriginBase(ctx, wtPath)
	rangeSpec := base + "...HEAD"

	nsCmd := exec.CommandContext(ctx, "git", "diff", "--name-status", rangeSpec)
	nsCmd.Dir = wtPath
	nsOut, err := nsCmd.Output()
	if err != nil {
		return tamperReport{}, fmt.Errorf("git diff --name-status: %w", err)
	}

	changes := parseNameStatus(string(nsOut))
	for i := range changes {
		c := &changes[i]
		if classifyTamperPath(c.Path) == tamperCatOther || strings.HasPrefix(c.Status, "D") {
			continue
		}
		patch, pErr := gitFilePatch(ctx, wtPath, rangeSpec, c.Path)
		if pErr != nil {
			e.logger.Warn("workflow.detect-tampering.file-patch", "task_id", taskID, "file", c.Path, "err", pErr)
			continue
		}
		c.Patch = patch
	}
	return buildTamperReport(taskID, base, changes), nil
}

func gitFilePatch(ctx context.Context, wtPath, rangeSpec, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", rangeSpec, "--", path)
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
	b.WriteString("possible test tampering — needs human bless before review: ")
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
