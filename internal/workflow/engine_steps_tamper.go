package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"log/slog"
	"os/exec"
	pathpkg "path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// TamperBlessedTag short-circuits the detector: a human who has reviewed a
// flagged diff and accepted it adds this tag, then moves the task back into the
// flow. Without it, relaunching a flagged task would re-scan the same
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
	// OldPath is the pre-rename path for a rename/copy ("R"/"C" status);
	// empty otherwise. The base commit only has the file under this path, so
	// base-content lookups (see BaseContent) must use it instead of Path.
	OldPath string
	Patch   string
	// BaseContent is the file content on the base (pre-change) side of the
	// diff, when available. Used to tell an added t.Skip (etc.) that follows
	// an existing, repo-established idiom apart from a genuinely novel skip —
	// see isEstablishedSkipIdiom. Deliberately the base side, not the
	// post-change file: counting occurrences in the final file would let two
	// brand-new identical skip lines added in the same commit "establish"
	// each other.
	BaseContent string
	// UpstreamContent is the file content on origin/<default> at diff time,
	// when available. Used to discount suspicious added lines that came from
	// merging upstream during the fix step rather than from the agent's own
	// edits in the same file.
	UpstreamContent string
}

type tamperPatchResult struct {
	Findings  []tamperFinding
	AddAssert int
	DelAssert int
	AddDecl   int
	DelDecl   int
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
	// tamperCapabilityGuardRe matches guards for host-dependent capabilities.
	tamperCapabilityGuardRe = regexp.MustCompile(
		`\b(os\.Symlink|os\.Link|os\.Readlink|filepath\.EvalSymlinks|` +
			`exec\.LookPath|LookPath|user\.Current|user\.Lookup|` +
			`net\.Listen|net\.Dial)\b|testing\.Short\s*\(\s*\)`)
	// tamperPlatformGuardLineRe requires a branch block; bare runtime.GOOS /
	// GOARCH references must not suppress a later skip.
	tamperPlatformGuardLineRe = regexp.MustCompile(`\bif\b.*\{`)
	tamperValidGOOS           = map[string]struct{}{
		"aix": {}, "android": {}, "darwin": {}, "dragonfly": {}, "freebsd": {}, "illumos": {},
		"ios": {}, "js": {}, "linux": {}, "netbsd": {}, "openbsd": {}, "plan9": {}, "solaris": {},
		"wasip1": {}, "windows": {},
	}
	tamperValidGOARCH = map[string]struct{}{
		"386": {}, "amd64": {}, "arm": {}, "arm64": {}, "loong64": {}, "mips": {}, "mips64": {},
		"mips64le": {}, "mipsle": {}, "ppc64": {}, "ppc64le": {}, "riscv64": {}, "s390x": {},
		"wasm": {},
	}
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
	// Task-spec parsing is intentionally conservative: only lines in a few
	// trusted spec sections that explicitly say delete/remove/drop and also
	// name a path-like token can bless a whole-file verification deletion.
	tamperDeletionVerbRe = regexp.MustCompile(
		`(?i)\b(delete|deletes|deleted|deleting|remove|removes|removed|removing|drop|drops|dropped|dropping|rm)\b`)
	tamperNonDeletionVerbRe = regexp.MustCompile(
		`(?i)\b(add|adds|added|adding|adjust|adjusts|adjusted|adjusting|change|changes|changed|changing|` +
			`create|creates|created|creating|edit|edits|edited|editing|fix|fixes|fixed|fixing|implement|implements|implemented|implementing|` +
			`modify|modifies|modified|modifying|move|moves|moved|moving|refactor|refactors|refactored|refactoring|` +
			`rename|renames|renamed|renaming|touch|touches|touched|touching|update|updates|updated|updating)\b`)
	tamperBacktickTokenRe = regexp.MustCompile("`([^`]+)`")
	tamperBarePathTokenRe = regexp.MustCompile(
		`(^|[\s([{"'])((?:\.?[\w-]+/)*\.?[\w-]+\.[\w.-]+)([\s)\]},"':;]|$)`)
)

type tamperDeletionAllowlist struct {
	ExactPaths map[string]bool `json:"exactPaths"`
	Basenames  map[string]bool `json:"basenames"`
}

// isEstablishedSkipIdiom reports whether the added skip line is already a
// repeated, pre-existing idiom in the file rather than a novel tampering
// attempt: an identical (trimmed) line already appears in the file's base
// (pre-change) content (e.g. the repo-wide
// `if !hasGit() { t.Skip("git not available") }` guard). Comparing against
// the base side — not the post-change file — is deliberate: two brand-new
// identical skip lines added in the same commit must not "establish" each
// other. baseContent is the full pre-change file; empty when unavailable, in
// which case the added line is always treated as novel (fail toward
// flagging, not toward silence).
func isEstablishedSkipIdiom(addedLine, baseContent string) bool {
	target := strings.TrimSpace(addedLine)
	if target == "" || baseContent == "" {
		return false
	}
	for line := range strings.SplitSeq(baseContent, "\n") {
		if strings.TrimSpace(line) == target {
			return true
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
func scanTamperPatch(path string, cat tamperCategory, patch, baseContent, upstreamContent string) []tamperFinding {
	return scanTamperPatchResult(path, cat, patch, baseContent, upstreamContent).Findings
}

func scanTamperPatchResult(path string, cat tamperCategory, patch, baseContent, upstreamContent string) tamperPatchResult {
	s := &tamperScan{
		path: path, cat: cat, isCI: cat == tamperCatCI, seen: map[string]bool{},
		baseContent: baseContent, mergedUpstreamSkips: mergedUpstreamSkipAllowance(baseContent, upstreamContent),
	}
	inHunk := false
	for line := range strings.SplitSeq(patch, "\n") {
		switch {
		case isDiffHeaderLine(line):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			s.resetHunkState()
			inHunk = true
		case !inHunk && (strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++")):
			// file header before any hunk; ignore
		case inHunk && strings.HasPrefix(line, "+"):
			s.feedAdded(line[1:])
		case inHunk && strings.HasPrefix(line, "-"):
			s.feedRemoved(line[1:])
		case inHunk && strings.HasPrefix(line, " "):
			s.feedContext(line[1:])
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
	path                string
	cat                 tamperCategory
	isCI                bool
	baseContent         string
	mergedUpstreamSkips map[string]int
	seen                map[string]bool
	findings            []tamperFinding
	addAssert           int
	delAssert           int
	addDecl             int
	delDecl             int
	addRun              int
	delRun              int
	guardWindow         int
	platformGuardDepth  int
}

const tamperGuardWindowLines = 3

func mergedUpstreamSkipAllowance(baseContent, upstreamContent string) map[string]int {
	upstreamCounts := trimmedMatchingLineCounts(upstreamContent, tamperAddedSkipRe)
	if len(upstreamCounts) == 0 {
		return nil
	}
	baseCounts := trimmedMatchingLineCounts(baseContent, tamperAddedSkipRe)
	allow := make(map[string]int, len(upstreamCounts))
	for line, n := range upstreamCounts {
		if delta := n - baseCounts[line]; delta > 0 {
			allow[line] = delta
		}
	}
	if len(allow) == 0 {
		return nil
	}
	return allow
}

func trimmedMatchingLineCounts(content string, re *regexp.Regexp) map[string]int {
	if content == "" {
		return nil
	}
	counts := map[string]int{}
	for line := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !re.MatchString(trimmed) {
			continue
		}
		counts[trimmed]++
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
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

func (s *tamperScan) resetHunkState() {
	s.platformGuardDepth = 0
}

func (s *tamperScan) feedContext(content string) {
	s.updatePlatformGuardDepth(content, false)
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
	isCapabilityGuard := tamperCapabilityGuardRe.MatchString(content)
	isPlatformGuard := isPlatformGuard(content)
	guarded := isCapabilityGuard || isPlatformGuard || s.guardWindow > 0 || s.platformGuardDepth > 0
	if isCapabilityGuard {
		s.guardWindow = tamperGuardWindowLines
	} else if s.guardWindow > 0 {
		s.guardWindow--
	}
	if tamperAddedSkipRe.MatchString(content) && !guarded &&
		!isEstablishedSkipIdiom(content, s.baseContent) &&
		!s.consumeMergedUpstreamSkip(content) {
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
	s.updatePlatformGuardDepth(content, isPlatformGuard)
}

func (s *tamperScan) updatePlatformGuardDepth(content string, startsGuard bool) {
	if startsGuard || s.platformGuardDepth > 0 {
		s.platformGuardDepth += codeBraceDelta(content)
	}
	if s.platformGuardDepth < 0 {
		s.platformGuardDepth = 0
	}
}

func isPlatformGuard(content string) bool {
	if !tamperPlatformGuardLineRe.MatchString(content) {
		return false
	}
	expr, ok := parsePlatformGuardExpr(content)
	return ok && isPlatformGuardExpr(expr)
}

func parsePlatformGuardExpr(content string) (ast.Expr, bool) {
	braceDelta := codeBraceDelta(content)
	if braceDelta < 0 {
		return nil, false
	}
	src := "package p\nfunc _(){\n" + content + strings.Repeat("\n}", braceDelta+1)
	file, err := parser.ParseFile(gotoken.NewFileSet(), "", src, 0)
	if err != nil || len(file.Decls) != 1 {
		return nil, false
	}
	fn, ok := file.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil || len(fn.Body.List) != 1 {
		return nil, false
	}
	stmt, ok := fn.Body.List[0].(*ast.IfStmt)
	if !ok || stmt.Init != nil || stmt.Cond == nil {
		return nil, false
	}
	return stmt.Cond, true
}

func isPlatformGuardExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return isPlatformGuardExpr(e.X)
	case *ast.BinaryExpr:
		switch e.Op {
		case gotoken.LAND, gotoken.LOR:
			return isPlatformGuardExpr(e.X) && isPlatformGuardExpr(e.Y)
		case gotoken.EQL, gotoken.NEQ:
			return isPlatformComparison(e.X, e.Y) || isPlatformComparison(e.Y, e.X)
		default:
			return false
		}
	default:
		return false
	}
}

func isPlatformComparison(left, right ast.Expr) bool {
	sel, ok := left.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "runtime" {
		return false
	}
	lit, ok := right.(*ast.BasicLit)
	if !ok || lit.Kind != gotoken.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	switch sel.Sel.Name {
	case "GOOS":
		_, ok = tamperValidGOOS[value]
	case "GOARCH":
		_, ok = tamperValidGOARCH[value]
	default:
		ok = false
	}
	return ok
}

func codeBraceDelta(content string) int {
	delta := 0
	var quote byte
	escaped := false
	inBlockComment := false
	for i := 0; i < len(content); i++ {
		ch := content[i]
		if inBlockComment {
			if ch == '*' && i+1 < len(content) && content[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			switch {
			case quote == '`' && ch == '`':
				quote = 0
			case quote != '`' && escaped:
				escaped = false
			case quote != '`' && ch == '\\':
				escaped = true
			case quote != '`' && ch == quote:
				quote = 0
			}
			continue
		}
		if ch == '/' && i+1 < len(content) {
			switch content[i+1] {
			case '/':
				return delta
			case '*':
				inBlockComment = true
				i++
				continue
			}
		}
		switch ch {
		case '"', '\'', '`':
			quote = ch
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

func (s *tamperScan) consumeMergedUpstreamSkip(content string) bool {
	if len(s.mergedUpstreamSkips) == 0 {
		return false
	}
	key := strings.TrimSpace(content)
	if s.mergedUpstreamSkips[key] <= 0 {
		return false
	}
	s.mergedUpstreamSkips[key]--
	return true
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
	return tamperPatchResult{
		Findings: s.findings, AddAssert: s.addAssert, DelAssert: s.delAssert,
		AddDecl: s.addDecl, DelDecl: s.delDecl,
	}
}

// buildTamperReport assembles the report from the parsed diff. Pure function:
// classification + per-file scan + medium fallback. No git access.
func buildTamperReport(taskID, base string, changes []tamperChange, allow tamperDeletionAllowlist) tamperReport {
	report := tamperReport{TaskID: taskID, Base: base}
	scanned := 0
	totalAddedAssertions := 0
	totalDeletedAssertions := 0
	totalAddedDecl := 0
	totalDeletedDecl := 0
	deletedBasenames := map[string]int{}
	for i := range changes {
		c := changes[i]
		if !strings.HasPrefix(c.Status, "D") {
			continue
		}
		if cat := classifyTamperPath(c.Path); cat != tamperCatOther {
			deletedBasenames[pathpkg.Base(c.Path)]++
		}
	}
	for i := range changes {
		c := changes[i]
		cat := classifyTamperPath(c.Path)
		if cat == tamperCatOther {
			continue
		}
		report.Files = append(report.Files, c.Path)

		// Whole-file deletion of a verification file is itself high-severity.
		if strings.HasPrefix(c.Status, "D") {
			severity := tamperHigh
			detail := string(cat) + " file deleted"
			if documentedDeletionMatches(c.Path, allow, deletedBasenames) {
				severity = tamperMedium
				detail += " (documented in task spec)"
			}
			report.Findings = append(report.Findings, tamperFinding{
				File: c.Path, Category: string(cat), Severity: severity,
				Rule: "deleted-verification-file", Detail: detail,
			})
			continue
		}
		if scanned >= tamperMaxScannedFiles {
			continue
		}
		scanned++
		res := scanTamperPatchResult(c.Path, cat, c.Patch, c.BaseContent, c.UpstreamContent)
		totalAddedAssertions += res.AddAssert
		totalDeletedAssertions += res.DelAssert
		totalAddedDecl += res.AddDecl
		totalDeletedDecl += res.DelDecl
		report.Findings = append(report.Findings, res.Findings...)
	}
	// An assertion or test declaration deleted in one file and re-added
	// (moved/renamed/consolidated) in another nets to zero across the diff
	// even though each file's own scan sees only its half — so the pass/fail
	// decision is based on the diff-wide net, not the per-file net computed
	// in finalize(). Both rules use the same strict net-offset formula so
	// they downgrade consistently.
	if totalDeletedAssertions-totalAddedAssertions <= 0 {
		report.Findings = downgradeFindingsByRule(report.Findings, "removed-assertions")
	}
	if totalDeletedDecl-totalAddedDecl <= 0 {
		report.Findings = downgradeFindingsByRule(report.Findings, "removed-test-cases")
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

// downgradeFindingsByRule lowers the severity of every finding matching rule
// to medium (non-blocking) — used once the diff-wide net for that rule shows
// the apparent removal was offset elsewhere in the same diff.
func downgradeFindingsByRule(findings []tamperFinding, rule string) []tamperFinding {
	out := findings[:0]
	for _, f := range findings {
		if f.Rule == rule {
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
	base, rangeSpec := resolveTamperRange(ctx, wtPath, t, taskID, e.logger)
	upstream := resolveOriginBase(ctx, wtPath)

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

	changes := dropUpstreamMergedChanges(ctx, wtPath, taskID, upstream, parseNameStatus(string(nsOut)), e.logger)
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
		// Base content is only ever consulted to resolve an added-skip finding
		// (see isEstablishedSkipIdiom), so skip the `git show` round-trip
		// entirely when the patch adds no skip-pattern candidate line —
		// avoids the IO cost on every scanned file, including large
		// snapshot/testdata diffs that never touch a skip idiom.
		if !tamperAddedSkipRe.MatchString(patch) {
			continue
		}
		// Read the file as it existed on the base side of the diff (pre-change)
		// to tell an established skip idiom (already used elsewhere in the file
		// before this change) apart from a genuinely novel one. Reading the
		// post-change file instead would let two brand-new identical skip lines
		// added in the same commit "establish" each other. For a rename/copy the
		// base commit only has the file under OldPath (Path doesn't exist there
		// yet), so look it up under whichever path the base side actually has.
		basePath := c.Path
		if c.OldPath != "" {
			basePath = c.OldPath
		}
		content, cErr := gitFileAtRef(ctx, wtPath, base, basePath)
		if cErr != nil {
			// Expected for newly added files (no base-side content) — only
			// exceptional if the context itself died.
			if ctx.Err() != nil {
				return tamperReport{}, fmt.Errorf("git show %s:%s: %w", base, basePath, ctx.Err())
			}
			e.logger.Debug("workflow.detect-tampering.base-content",
				"task_id", taskID, "file", c.Path, "err", cErr)
		} else {
			c.BaseContent = content
		}
		if upstream == "" {
			continue
		}
		upstreamContent, uErr := gitFileAtRef(ctx, wtPath, upstream, basePath)
		if uErr != nil {
			if ctx.Err() != nil {
				return tamperReport{}, fmt.Errorf("git show %s:%s: %w", upstream, basePath, ctx.Err())
			}
			e.logger.Debug("workflow.detect-tampering.upstream-content",
				"task_id", taskID, "file", c.Path, "err", uErr)
			continue
		}
		c.UpstreamContent = upstreamContent
	}
	report := buildTamperReport(taskID, base, changes, documentedDeletionAllowlistSnapshot(t))
	report.Range = rangeSpec
	return report, nil
}

func tamperBaselineVar(stepID string) string {
	return "step." + stepID + ".tamper_base"
}

const tamperDeletionAllowlistVar = "tamper_deletion_allowlist"

func captureTamperDeletionAllowlist(wfExec *Execution, stepID, role string, t TaskInfo) {
	if wfExec == nil || stepID == "" || !tamperCodeAuthorRole(role) {
		return
	}
	if strings.TrimSpace(wfExec.Variables[tamperDeletionAllowlistVar]) != "" {
		return
	}
	data, err := json.Marshal(documentedDeletionAllowlistForTrustedSpec(t))
	if err != nil {
		return
	}
	wfExec.SetVar(tamperDeletionAllowlistVar, string(data))
}

func tamperCodeAuthorRole(role string) bool {
	switch role {
	case "", "implementation", "fix-review", "pr-fix", "test-fix":
		return true
	default:
		return false
	}
}

func documentedDeletionAllowlistForTrustedSpec(t TaskInfo) tamperDeletionAllowlist {
	allow := tamperDeletionAllowlist{
		ExactPaths: map[string]bool{},
		Basenames:  map[string]bool{},
	}
	mergeDocumentedDeletionAllowlist(&allow, documentedDeletionAllowlist(t.Body))
	mergeDocumentedDeletionAllowlist(&allow, documentedDeletionAllowlist(t.Plan))
	return allow
}

func mergeDocumentedDeletionAllowlist(dst *tamperDeletionAllowlist, src tamperDeletionAllowlist) {
	if dst == nil {
		return
	}
	if dst.ExactPaths == nil {
		dst.ExactPaths = map[string]bool{}
	}
	if dst.Basenames == nil {
		dst.Basenames = map[string]bool{}
	}
	for path := range src.ExactPaths {
		dst.ExactPaths[path] = true
	}
	for base := range src.Basenames {
		dst.Basenames[base] = true
	}
}

func documentedDeletionAllowlistSnapshot(t TaskInfo) tamperDeletionAllowlist {
	if t.Workflow == nil {
		return tamperDeletionAllowlist{}
	}
	raw := strings.TrimSpace(t.Workflow.Variables[tamperDeletionAllowlistVar])
	if raw == "" {
		return tamperDeletionAllowlist{}
	}
	var allow tamperDeletionAllowlist
	if err := json.Unmarshal([]byte(raw), &allow); err != nil {
		return tamperDeletionAllowlist{}
	}
	return allow
}

func resolveTamperRange(ctx context.Context, wtPath string, t TaskInfo, taskID string, logger *slog.Logger) (base, rangeSpec string) {
	if t.Workflow != nil {
		if stepID := t.Workflow.LastAgentStepID(); stepID != "" {
			if sha := strings.TrimSpace(t.Workflow.Variables[tamperBaselineVar(stepID)]); sha != "" {
				verify := exec.CommandContext(ctx, "git", "rev-parse", "--verify", sha+"^{commit}")
				verify.Dir = wtPath
				if verify.Run() == nil {
					// A stored baseline can go stale (e.g. the underlying branch
					// was force-pushed after the baseline was captured) and stay
					// git-resolvable while no longer being an ancestor of HEAD.
					// Diffing against such an orphaned base with two dots spans
					// the entire divergent history instead of the agent's actual
					// change, so require ancestry before trusting it.
					ancestor := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", sha, "HEAD")
					ancestor.Dir = wtPath
					if ancestor.Run() == nil {
						return sha, sha + "..HEAD"
					}
					if logger != nil {
						logger.Warn("workflow.detect-tampering.baseline-orphaned",
							"task_id", taskID, "baseline", sha)
					}
				}
			}
		}
	}
	base = resolveOriginBase(ctx, wtPath)
	return base, base + "...HEAD"
}

func dropUpstreamMergedChanges(ctx context.Context, wtPath, taskID, upstream string, changes []tamperChange, logger *slog.Logger) []tamperChange {
	if upstream == "" {
		return changes
	}
	kept := changes[:0]
	dropped := 0
	for _, c := range changes {
		if classifyTamperPath(c.Path) != tamperCatOther && pathIdenticalToUpstream(ctx, wtPath, upstream, c.Path) {
			dropped++
			continue
		}
		kept = append(kept, c)
	}
	if dropped > 0 && logger != nil {
		logger.Info("workflow.detect-tampering.upstream-merged-skipped",
			"task_id", taskID, "upstream", upstream, "dropped", dropped)
	}
	return kept
}

func pathIdenticalToUpstream(ctx context.Context, wtPath, upstream, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "diff", "--quiet", upstream, "HEAD", "--", path)
	cmd.Dir = wtPath
	return cmd.Run() == nil
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

// gitFileAtRef returns a file's content as it existed at ref (e.g. the base
// commit of a diff range). Errors for files that don't exist at ref — a
// newly added file — which callers treat as "no base content" rather than a
// hard failure.
func gitFileAtRef(ctx context.Context, wtPath, ref, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "show", ref+":"+path)
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseNameStatus parses `git diff --name-status` output into changes. Renames
// and copies (R/C) report old and new tab-separated paths; the new path (last
// field) is used as Path, the old path (for R/C) is kept as OldPath so
// base-content lookups can find the file where it actually lived pre-change.
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
		status := strings.TrimSpace(fields[0])
		change := tamperChange{
			Status: status,
			Path:   strings.TrimSpace(fields[len(fields)-1]),
		}
		if (strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C")) && len(fields) >= 3 {
			change.OldPath = strings.TrimSpace(fields[1])
		}
		changes = append(changes, change)
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

// documentedDeletionAllowlist extracts explicitly documented file deletions
// from trusted task-spec sections. It stays conservative on purpose: only
// exact repo-relative paths and unambiguous basenames are allowed, and only
// when the surrounding line itself says delete/remove/drop.
func documentedDeletionAllowlist(body string) tamperDeletionAllowlist {
	if strings.TrimSpace(body) == "" {
		return tamperDeletionAllowlist{}
	}
	allow := tamperDeletionAllowlist{
		ExactPaths: map[string]bool{},
		Basenames:  map[string]bool{},
	}
	for _, heading := range []string{
		"## Scope",
		"## Files",
		"## Steps",
		"## Deletions",
		"## File Deletions",
		"## Removed Files",
	} {
		start, end, ok := topLevelSectionRange(body, heading)
		if !ok {
			continue
		}
		collectDocumentedDeletionTokens(body[start:end], allow)
	}
	return allow
}

func collectDocumentedDeletionTokens(section string, allow tamperDeletionAllowlist) {
	if allow.ExactPaths == nil || allow.Basenames == nil {
		return
	}
	inFence := false
	for line := range strings.SplitSeq(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !tamperDeletionVerbRe.MatchString(trimmed) {
			continue
		}
		for segment := range strings.SplitSeq(line, ";") {
			if !tamperDeletionVerbRe.MatchString(segment) {
				continue
			}
			for _, candidate := range deletionPathsFromSegment(segment) {
				allow.ExactPaths[candidate] = true
				if !strings.Contains(candidate, "/") {
					allow.Basenames[candidate] = true
				}
			}
		}
	}
}

type documentedPathToken struct {
	path       string
	start, end int
}

func deletionPathsFromSegment(segment string) []string {
	verbs := tamperDeletionVerbRe.FindAllStringIndex(segment, -1)
	if len(verbs) == 0 {
		return nil
	}
	tokens := deletionPathTokensFromSegment(segment, verbs)
	if len(tokens) == 0 {
		return nil
	}
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, token.path)
	}
	return out
}

func deletionPathTokensFromSegment(segment string, deletionVerbs [][]int) []documentedPathToken {
	actionVerbs := append([][]int{}, deletionVerbs...)
	actionVerbs = append(actionVerbs, tamperNonDeletionVerbRe.FindAllStringIndex(segment, -1)...)
	slices.SortFunc(actionVerbs, func(a, b []int) int { return a[0] - b[0] })

	tokens := pathTokensFromSegment(segment)
	out := tokens[:0]
	for _, token := range tokens {
		if !pathTokenIsDocumentedDeletion(segment, token, actionVerbs, deletionVerbs) {
			continue
		}
		out = append(out, token)
	}
	return out
}

func pathTokenIsDocumentedDeletion(segment string, token documentedPathToken, actionVerbs, deletionVerbs [][]int) bool {
	var previous []int
	for _, verb := range actionVerbs {
		if verb[1] > token.start {
			break
		}
		previous = verb
	}
	if previous != nil {
		return verbRangeInList(previous, deletionVerbs) &&
			!documentedDeletionVerbIsNegated(segment, previous) &&
			tokenHasDeletionVerbLead(segment, previous, token)
	}
	next := nextActionVerbAfterToken(token, actionVerbs)
	return next != nil &&
		verbRangeInList(next, deletionVerbs) &&
		!documentedDeletionVerbIsNegated(segment, next) &&
		tokenHasDeletionVerbTrailer(segment, token, next)
}

func nextActionVerbAfterToken(token documentedPathToken, actionVerbs [][]int) []int {
	for _, verb := range actionVerbs {
		if verb[0] >= token.end {
			return verb
		}
	}
	return nil
}

func tokenHasDeletionVerbTrailer(segment string, token documentedPathToken, verb []int) bool {
	trailer := strings.TrimSpace(segment[token.end:verb[0]])
	trailer = strings.TrimSpace(strings.Trim(trailer, "`\"'"))
	if trailer == "" {
		return true
	}
	return trailer == "-" || trailer == ":" || trailer == "("
}

func tokenHasDeletionVerbLead(segment string, verb []int, token documentedPathToken) bool {
	lead := segment[verb[1]:token.start]
	if lead == "" {
		return true
	}
	lead = redactDocumentedPathTokens(lead)
	lead = strings.TrimSpace(lead)
	lead = strings.TrimSpace(strings.Trim(lead, "`\"'"))
	if lead == "" {
		return true
	}
	if strings.ContainsAny(lead, ".?!") {
		return false
	}
	parts := strings.Split(lead, ",")
	for _, part := range parts[:len(parts)-1] {
		if !documentedDeletionLeadConnectorOnly(part) {
			return false
		}
	}
	lead = strings.TrimSpace(parts[len(parts)-1])
	if lead == "" || documentedDeletionLeadConnectorOnly(lead) {
		return true
	}
	words := documentedDeletionLeadWords(lead)
	if len(words) == 0 {
		return true
	}
	if len(words) > 5 {
		return false
	}
	return documentedDeletionLeadWholeFileDescriptor(words) ||
		documentedDeletionLeadContentGroupFromDescriptor(words)
}

func documentedDeletionVerbIsNegated(segment string, verb []int) bool {
	prefix := segment[:verb[0]]
	lastBoundary := -1
	for i, r := range prefix {
		switch r {
		case '.', '?', '!', ';', ':', ',', '\n', '\r':
			lastBoundary = i
		}
	}
	if lastBoundary >= 0 {
		prefix = prefix[lastBoundary+1:]
	}
	words := documentedDeletionContextWords(prefix)
	const maxWordsBeforeVerb = 6
	if len(words) > maxWordsBeforeVerb {
		words = words[len(words)-maxWordsBeforeVerb:]
	}
	return slices.ContainsFunc(words, documentedDeletionNegationWord)
}

func documentedDeletionLeadConnectorOnly(s string) bool {
	words := documentedDeletionLeadWords(s)
	if len(words) == 0 {
		return true
	}
	for _, word := range words {
		if !slices.Contains([]string{"and", "or", "plus"}, word) {
			return false
		}
	}
	return true
}

func documentedDeletionLeadWords(s string) []string {
	return documentedDeletionContextWords(s)
}

func documentedDeletionContextWords(s string) []string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return (r < 'A' || r > 'Z') &&
			(r < 'a' || r > 'z') &&
			(r < '0' || r > '9') &&
			r != '_' &&
			r != '\''
	})
	for i := range words {
		words[i] = strings.ToLower(words[i])
	}
	return words
}

func documentedDeletionNegationWord(word string) bool {
	switch word {
	case "avoid", "avoided", "avoiding", "avoids",
		"cannot", "cant", "can't",
		"dont", "don't",
		"forbid", "forbidden", "forbids",
		"mustnt", "mustn't",
		"never",
		"no",
		"not",
		"shouldnt", "shouldn't",
		"without",
		"wont", "won't":
		return true
	}
	return strings.HasSuffix(word, "n't")
}

func documentedDeletionLeadWholeFileDescriptor(words []string) bool {
	if len(words) == 0 {
		return true
	}
	hasFileNoun := false
	for _, word := range words {
		switch word {
		case "a", "an", "the", "old", "legacy", "obsolete", "stale", "unused":
		case "test", "tests", "snapshot", "snapshots", "fixture", "fixtures", "ci", "workflow", "workflows":
		case "file", "files":
			hasFileNoun = true
		default:
			return false
		}
	}
	return hasFileNoun
}

func documentedDeletionLeadContentGroupFromDescriptor(words []string) bool {
	if len(words) < 2 || words[len(words)-1] != "from" {
		return false
	}
	descriptor := words[:len(words)-1]
	if len(descriptor) == 0 || len(descriptor) > 4 {
		return false
	}
	switch descriptor[len(descriptor)-1] {
	case "cases", "tests", "snapshots", "fixtures", "specs":
	default:
		return false
	}
	for _, word := range descriptor[:len(descriptor)-1] {
		if word == "from" || documentedDeletionNegationWord(word) {
			return false
		}
	}
	return true
}

func redactDocumentedPathTokens(s string) string {
	tokens := pathTokensFromSegment(s)
	if len(tokens) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	cursor := 0
	for _, token := range tokens {
		if token.start < cursor {
			continue
		}
		b.WriteString(s[cursor:token.start])
		b.WriteString(strings.Repeat(" ", token.end-token.start))
		cursor = token.end
	}
	b.WriteString(s[cursor:])
	return b.String()
}

func verbRangeInList(needle []int, haystack [][]int) bool {
	for _, candidate := range haystack {
		if candidate[0] == needle[0] && candidate[1] == needle[1] {
			return true
		}
	}
	return false
}

func pathTokensFromSegment(segment string) []documentedPathToken {
	var out []documentedPathToken
	add := func(raw string, start, end int) {
		token, ok := normalizeDocumentedPath(raw)
		if !ok {
			return
		}
		out = append(out, documentedPathToken{path: token, start: start, end: end})
	}
	for _, m := range tamperBacktickTokenRe.FindAllStringSubmatchIndex(segment, -1) {
		if len(m) >= 4 {
			add(segment[m[2]:m[3]], m[2], m[3])
		}
	}
	for _, m := range tamperBarePathTokenRe.FindAllStringSubmatchIndex(segment, -1) {
		if len(m) >= 6 {
			add(segment[m[4]:m[5]], m[4], m[5])
		}
	}
	return out
}

func normalizeDocumentedPath(raw string) (string, bool) {
	token := strings.TrimSpace(raw)
	token = strings.Trim(token, "`\"'()[]{}<>.,:;")
	if token == "" || strings.Contains(token, `\`) || strings.HasPrefix(token, "/") {
		return "", false
	}
	token = strings.TrimPrefix(token, "./")
	token = pathpkg.Clean(token)
	if token == "." || token == ".." || strings.HasPrefix(token, "../") {
		return "", false
	}
	return token, true
}

func documentedDeletionMatches(pathname string, allow tamperDeletionAllowlist, deletedBasenames map[string]int) bool {
	normalized, ok := normalizeDocumentedPath(pathname)
	if !ok {
		return false
	}
	if allow.ExactPaths[normalized] {
		return true
	}
	base := pathpkg.Base(normalized)
	return allow.Basenames[base] && deletedBasenames[base] == 1
}
