package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/evidence"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/workflow/failureclassify"
)

// verifyChecksDefaultTimeout bounds the whole verify run (every command). A test
// suite is slower than the 30s shellTimeout used by lighter steps. Overridable
// per-engine via SetVerifyTimeout (tests use a short value).
const verifyChecksDefaultTimeout = 10 * time.Minute

// verifyChecksMaxOutput bounds how much command output is retained in memory.
// A noisy or malicious verify command must not be able to OOM the engine, so
// output streams into a fixed-size head+tail buffer (see boundedTail) rather
// than a growing slice.
const verifyChecksMaxOutput = 64 * 1024

// verifyBlessedTag lets a human accept a verify failure (e.g. a known-flaky
// suite) and let the task proceed instead of re-blocking on every re-dispatch.
const verifyBlessedTag = "verify-blessed"

const (
	verifyChecksImplStepID = "implement"
	verifyReaskNoteVar     = "verify_reask_note"
	verifyRetryModelVar    = "verify_retry_model"
	verifyAutoFixRunIDVar  = "verify_auto_fix.rewound_run_agent_id"
	// verifyChecksAutoFixBackoff is the base re-dispatch delay before the next
	// auto-fix attempt; autoFixBackoff grows it with the attempt count up to
	// autoFixBackoffMax.
	verifyChecksAutoFixBackoff = 90 * time.Second
	autoFixBackoffMax          = 15 * time.Minute
	// verifyChecksAutoFixCeiling bounds auto-fix re-asks so a deterministic
	// failure no agent can fix reaches a human instead of looping forever.
	verifyChecksAutoFixCeiling = 3
	// Full verify suites are CPU-heavy and already serialized by workflow
	// retries; local slots prevent one saturated host from piling multiple
	// suites on top of each other and timing them all out. The fallback slot
	// count when the engine has no configured/derived value is deliberately
	// conservative (a single slot) — see verifyChecksSlot and
	// config.Config.VerifyChecksMaxConcurrent for the CPU-derived default.
	verifyChecksBackoff              = 1 * time.Minute
	verifyChecksDefaultMaxConcurrent = 1
)

const verifyChecksBusyReason = "verify suite deferred: another local verify run is already in flight"

// verifyChecksFlakeRetries is how many extra times a failed verify command is
// re-run before the gate blocks. A single retry absorbs a nondeterministic
// flake (e.g. a test-teardown TempDir race) that would otherwise escalate
// unrelated work to human-required. A genuine failure fails every attempt and
// still blocks; the cost is one extra suite run only on the failing command.
const verifyChecksFlakeRetries = 1

const verifyChecksTimeoutRetries = 1

// npmReinstallTimeout caps a single toolchain-repair `npm ci`, run only when
// ensureNodeToolchain detects a corrupt node_modules right before a verify
// command. repairCtx derives from the outer verify-suite ctx, so the repair is
// bounded by the smaller of this and whatever remains of the suite budget: it
// keeps a hung repair from running unbounded, but it does draw down the shared
// suite budget rather than getting a separate allowance on top of it.
const npmReinstallTimeout = 3 * time.Minute

// cdDirPattern extracts the directory of every `cd <dir>` in a verify command
// string (optionally quoted), e.g.
// "(cd frontend && mise exec -- npm run build:web)" -> "frontend". Verify
// commands are opaque shell strings (see resolveSetupCommands) with no
// metadata marking which ones touch a Node toolchain, so this is the only
// signal available for locating the package.json to integrity-check.
//
// The `(?:^|[\s(])` prefix requires `cd` to start the string or follow
// whitespace/`(`, so a substring like `test:cd main` is not a false match. The
// capture group accepts a single- or double-quoted path (so a directory with a
// space survives) or an unquoted run of non-delimiter chars. FindAll is used to
// resolve every leg of a chained command, not just the first.
var cdDirPattern = regexp.MustCompile(`(?:^|[\s(])cd\s+("[^"]*"|'[^']*'|[^\s&|;)]+)`)

// verifyChecksNodeModulesRepairTimeout bounds a single repair `npm ci`
// invocation, mirroring the project's frontend setup batch timeout
// (docs/CONFIG.md's `setup:` commands get 5 minutes).
const verifyChecksNodeModulesRepairTimeout = 5 * time.Minute

var verifyGolangCILintFindingRe = regexp.MustCompile(`^([^\s:][^:\n]*\.go):\d+:\d+:\s.+$`)

var verifyFrontendPathMentionRe = regexp.MustCompile(`(?:^|[\s("'` + "`" + `])((?:frontend/|(?:\./)?src/)[A-Za-z0-9._/\-]+\.(?:[cm]?[jt]sx?|svelte))(?:[:(]\d+(?::\d+)?)?`)

var verifyFrontendDeterministicSignalRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\[vitest\].*no ".*" export is defined`),
	regexp.MustCompile(`(?i)\[vitest\].*did you forget to return it from "vi\.mock"`),
	regexp.MustCompile(`(?i)^(?:AssertionError|TypeError|ReferenceError|SyntaxError|Error):`),
	regexp.MustCompile(`(?i)\bexpected\b.+\bto\b`),
	regexp.MustCompile(`(?i)\bdoes not provide an export named\b`),
	regexp.MustCompile(`(?i)\bcannot find module\b`),
}

// verifyChecksReport is the structured result, stored as a generic artifact.
type verifyChecksReport struct {
	Commands       []string `json:"commands"`
	FailedCmd      string   `json:"failedCmd,omitempty"`
	OutputTail     string   `json:"outputTail,omitempty"`
	Classification string   `json:"classification,omitempty"`
	FailedPackages []string `json:"failedPackages,omitempty"`
	ChangedFiles   []string `json:"changedFiles,omitempty"`
}

type verifyFailureClassification struct {
	Kind           failureclassify.Kind
	Reason         string
	FailedPackages []string
	ChangedFiles   []string
	AutoFixable    bool
}

// execVerifyChecks runs the project's deterministic verify suite
// (`checks.verify`, opt-in) in the agent's worktree before it hands off to
// review. A non-zero exit flips the task to human-required so an implementation
// that does not pass its own declared verification suite cannot reach a PR.
// Complements detect_tampering (phase 1): structural test tampering is caught
// there; this catches incomplete/broken work the agent committed without the
// suite passing.
//
// The commands run in the agent's already-set-up worktree with no git mutation,
// reusing the same `sh -c` + inherited-env mechanism as worktree setup so the
// toolchain (mise) resolves identically.
//
// Short-circuits (no block): the verify-blessed tag, no CheckConfigGetter, no
// verify commands configured, or no worktree. A genuine command failure, an
// unrepaired corrupted node_modules, or the suite exceeding the time budget
// (an agent could hang a test to dodge) blocks.
// Only engine-shutdown cancellation fails open, so an implementation cannot
// skip verification by leaving the toolchain in an unrepaired bad state.
func (e *Engine) execVerifyChecks(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	if slices.Contains(t.Tags, verifyBlessedTag) {
		e.logger.Info("workflow.verify-checks.blessed", "task_id", taskID)
		e.recordEvidence(taskID, step.ID, evidenceCriterionVerifyChecks, evidence.ProofManual,
			0, "human bless ("+verifyBlessedTag+" tag)", "blessed")
		return stepDone(step, "blessed")
	}
	cmds, wtPath, timeout, skip := e.loadVerifyChecksInputs(taskID)
	if skip != "" {
		return stepDone(step, skip)
	}

	treeSHA, checksHash := e.verifyChecksCacheKey(e.ctx, wtPath, cmds)
	if treeSHA != "" {
		if out, hit := e.verifyChecksCacheHit(taskID, step, wtPath, treeSHA, checksHash); hit {
			return out, nil
		}
	}

	slot := e.verifyChecksSlot()
	releaseVerifySlot, ok := e.acquireVerifyChecksSlot(slot)
	if !ok {
		if wfExec == nil {
			select {
			case slot <- struct{}{}:
				releaseVerifySlot = func() { <-slot }
			case <-e.ctx.Done():
				e.logger.Warn("workflow.verify-checks.canceled", "task_id", taskID, "err", e.ctx.Err())
				return stepDone(step, "skipped: context canceled")
			}
		} else {
			return e.parkVerifyChecksForBackpressure(taskID, step, wfExec, t)
		}
	}
	defer releaseVerifySlot()

	if e.repairCorruptedNodeModules(e.ctx, taskID, wtPath) {
		// Corruption was detected but the repair itself failed (e.g. `npm ci`
		// killed again, or no network). Do not continue into verify with a
		// known-corrupt install, and do not mark the gate skipped: an agent
		// could otherwise leave node_modules broken to bypass the suite.
		e.logger.Warn("workflow.verify-checks.node-modules-repair-unresolved", "task_id", taskID)
		return e.flagVerifyChecks(taskID, step,
			"verify suite could not repair corrupted node_modules before running checks — rerun setup or fix the toolchain state",
			"node_modules-repair")
	}
	e.repairTornNodeModules(e.ctx, taskID, wtPath)

	failedCmd, output, runErr := e.runVerifySuiteWithRetry(e.ctx, taskID, wtPath, cmds, timeout, step.ID)

	if failedCmd != "" && failureclassify.IsMissingToolchain(output) {
		if healed, fc, out, rErr := e.healToolchainAndRetry(taskID, wtPath, cmds, timeout, step.ID); healed {
			failedCmd, output, runErr = fc, out, rErr
		}
	}

	report := verifyChecksReport{
		Commands: cmds, FailedCmd: failedCmd, OutputTail: output,
	}
	classification := e.classifyVerifyFailure(taskID, wtPath, failedCmd, output)
	if classification != nil {
		report.Classification = classification.Kind.String()
		report.FailedPackages = classification.FailedPackages
		report.ChangedFiles = classification.ChangedFiles
	}
	if e.recorder != nil {
		if data, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
			if recErr := e.recorder.PutGeneric(taskID, "verify-checks.json", step.ID, string(data)); recErr != nil {
				e.logger.Warn("workflow.verify-checks.artifact", "task_id", taskID, "err", recErr)
			}
		}
	}

	// Engine-shutdown cancellation is the only fail-open: there is no point
	// blocking work the engine is tearing down. Our own deadline fails CLOSED —
	// otherwise an agent could hang a test past the budget to dodge the gate.
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			reason := fmt.Sprintf(
				"verify suite exceeded the time budget (%s) on all %d attempts"+
					" — fix slow or hanging tests, or add the `verify-blessed` tag to override",
				timeout, verifyChecksTimeoutRetries+1)
			return e.flagVerifyChecks(taskID, step, reason, "timeout")
		}
		if errors.Is(runErr, context.Canceled) && e.ctx.Err() != nil {
			e.logger.Warn("workflow.verify-checks.canceled", "task_id", taskID, "err", runErr)
			return stepDone(step, "skipped: context canceled")
		}
		reason := "verify suite could not prepare its isolated cache or run cleanly: " + trimDiffLine(runErr.Error())
		return e.flagVerifyChecks(taskID, step, reason, "setup")
	}

	if failedCmd != "" {
		if classification != nil {
			if classification.AutoFixable {
				return e.autoFixOrFlagVerifyChecks(
					taskID, step, wfExec, t, classification.Reason, failedCmd, output)
			}
			return e.blockVerifyChecks(taskID, step, classification.Reason, classification.Kind.String())
		}
		reason := "implementation does not pass the project verify suite: " + trimDiffLine(failedCmd) +
			" — fix the code, or add the `verify-blessed` tag to override (e.g. a known-flaky suite)"
		return e.autoFixOrFlagVerifyChecks(taskID, step, wfExec, t, reason, failedCmd, output)
	}

	e.logger.Info("workflow.verify-checks.clean", "task_id", taskID, "commands", len(cmds))
	e.recordVerifyChecksEvidence(taskID, step.ID, strings.Join(cmds, " && "), report.OutputTail, treeSHA, checksHash)
	return stepDone(step, "clean")
}

// verifyChecksCacheKey computes the (tree SHA, verify-commands hash) memo key
// for a verify_checks run. treeSHA is "" on any error resolving the worktree
// tree (dirty index race, git failure, etc.) — the caller treats that as a
// definite cache miss rather than risk matching an empty/incomplete key.
// checksHash never fails (a pure digest of the resolved command strings), so
// it is always populated once cmds is non-empty.
func (e *Engine) verifyChecksCacheKey(ctx context.Context, wtPath string, cmds []string) (treeSHA, checksHash string) {
	checksHash = evidence.Digest(strings.Join(cmds, "\x00"))
	sha, err := currentWorktreeTree(ctx, wtPath)
	if err != nil {
		return "", checksHash
	}
	return sha, checksHash
}

// verifyChecksCacheHit re-stamps existing verify_checks evidence to the
// current HEAD and reports a hit when the durable evidence store already
// holds a passing entry for this exact (tree SHA, commands hash) — letting an
// unchanged tree skip the suite entirely, before it ever contends for a
// concurrency slot. Only ever a cache HIT for a PASSING prior run (an
// entry.Passed() check gates it); a failing run is never memoized in the
// first place (see the exit-0-only recordVerifyChecksEvidence call site), so
// there is nothing here to re-run a known-bad state against a fresh key.
// Mirrors refreshReviewEvidenceFreshness's re-stamp-in-place shape. Any read
// or write failure along the way is a miss, never a hit — the memo must stay
// strictly additive and never let a broken lookup substitute for a suite that
// never actually ran against this tree.
func (e *Engine) verifyChecksCacheHit(taskID string, step *Step, wtPath, treeSHA, checksHash string) (StepOutput, bool) {
	if e.evidenceRecorder == nil {
		return StepOutput{}, false
	}
	ce, err := e.evidenceRecorder.Evidence(taskID)
	if err != nil {
		return StepOutput{}, false
	}
	entry, ok := ce.ByCriterion(evidenceCriterionVerifyChecks)
	if !ok || !entry.Passed() || entry.TreeSHA == "" || entry.TreeSHA != treeSHA || entry.ChecksHash != checksHash {
		return StepOutput{}, false
	}

	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	entry.FinalRev = revParseCommit(ctx, wtPath, "HEAD")
	entry.StepID = step.ID
	entry.Timestamp = time.Now().UTC()
	if err := e.evidenceRecorder.AppendCriterion(taskID, entry); err != nil {
		e.logger.Warn("workflow.evidence.refresh-failed",
			"task_id", taskID, "criterion", evidenceCriterionVerifyChecks, "err", err)
		return StepOutput{}, false
	}

	e.logger.Info("workflow.verify-checks.cached", "task_id", taskID, "tree_sha", treeSHA)
	out, _ := stepDone(step, "clean (cached: "+treeSHA+")")
	return out, true
}

func (e *Engine) loadVerifyChecksInputs(taskID string) (cmds []string, wtPath string, timeout time.Duration, skip string) {
	if e.checks == nil {
		return nil, "", 0, "skipped: no check config getter"
	}
	timeout = resolveWorkflowCheckTimeout(e.verifyTimeout)
	cmds = e.checks.VerifyCommands(e.ctx, taskID)
	if len(cmds) == 0 {
		return nil, "", 0, "skipped: no verify commands configured"
	}
	if e.worktrees == nil {
		return nil, "", 0, "skipped: no worktree getter configured"
	}
	var ok bool
	wtPath, ok = e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return nil, "", 0, "skipped: no worktree for task"
	}
	e.logVerifyChecksTimeout(taskID, timeout)
	return cmds, wtPath, timeout, ""
}

func (e *Engine) logVerifyChecksTimeout(taskID string, timeout time.Duration) {
	base := e.verifyTimeout
	if base <= 0 {
		base = verifyChecksDefaultTimeout
	}
	if timeout == base {
		return
	}
	e.logger.Info("workflow.verify-checks.timeout-scaled",
		"task_id", taskID, "base", base.String(), "effective", timeout.String())
}

func (e *Engine) verifyChecksSlot() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.verifyChecksSlots == nil {
		n := e.verifyChecksMaxConcurrent
		if n <= 0 {
			n = verifyChecksDefaultMaxConcurrent
		}
		e.verifyChecksSlots = make(chan struct{}, n)
	}
	return e.verifyChecksSlots
}

func (e *Engine) acquireVerifyChecksSlot(slot chan struct{}) (release func(), ok bool) {
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	default:
		return nil, false
	}
}

func (e *Engine) parkVerifyChecksForBackpressure(taskID string, step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	wfExec.CurrentStep = step.ID
	wfExec.State = ExecWaiting
	wfExec.SetVar(workflowRetryAfterVar, e.now().Add(verifyChecksBackoff).Format(time.RFC3339))
	if err := e.tasks.SetStatusAndWorkflow(taskID, string(t.Status), verifyChecksBusyReason, wfExec); err != nil {
		return StepOutput{}, err
	}
	e.logger.Warn("workflow.verify-checks.backpressure", "task_id", taskID, "step", step.ID)
	return StepOutput{}, errStepParked
}

// VerifyTaskNow re-runs a task's configured verify commands against its
// current worktree state on demand, outside the normal verify_checks step —
// used by internal/watchdog to distinguish a genuine implementation defect
// from a stale/false-positive loop-stop verdict before escalating to
// human-required (#2155). verified is false whenever nothing could actually
// be checked (no CheckConfigGetter/WorktreeGetter wired, no verify commands
// configured, no worktree yet, or preflight toolchain repair failed); callers
// must treat that the same as "could not verify" and fall back to their own
// default behavior rather than treat it as a pass.
func (e *Engine) VerifyTaskNow(ctx context.Context, taskID string) (verified, passed bool, failedCmd, output string, err error) {
	if e.checks == nil || e.worktrees == nil {
		return false, false, "", "", nil
	}
	cmds := e.checks.VerifyCommands(ctx, taskID)
	if len(cmds) == 0 {
		return false, false, "", "", nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return false, false, "", "", nil
	}
	if e.repairCorruptedNodeModules(ctx, taskID, wtPath) {
		return false, false, "", "node_modules repair failed", errors.New("verify: node_modules repair failed")
	}
	e.repairTornNodeModules(ctx, taskID, wtPath)

	timeout := resolveWorkflowCheckTimeout(e.verifyTimeout)
	if timeout != e.verifyTimeout && e.verifyTimeout > 0 {
		e.logger.Info("workflow.verify-now.timeout-scaled",
			"task_id", taskID, "base", e.verifyTimeout.String(), "effective", timeout.String())
	}
	if e.verifyTimeout <= 0 && timeout != verifyChecksDefaultTimeout {
		e.logger.Info("workflow.verify-now.timeout-scaled",
			"task_id", taskID, "base", verifyChecksDefaultTimeout.String(), "effective", timeout.String())
	}

	failedCmd, output, err = e.runVerifySuiteWithRetry(ctx, taskID, wtPath, cmds, timeout, "verify_now")
	if err != nil {
		return true, false, failedCmd, output, err
	}
	return true, failedCmd == "", failedCmd, output, nil
}

// flagVerifyChecks flips the task to human-required. A failed status write
// returns an error so the workflow stalls instead of advancing past the gate —
// the YAML transition keys off task.status, so a silently-failed write would
// otherwise route a failing implementation straight to review.
func (e *Engine) runVerifySuiteWithRetry(parent context.Context, taskID, wtPath string, cmds []string, timeout time.Duration, stepID string) (failedCmd, output string, runErr error) {
	for attempt := 0; attempt <= verifyChecksTimeoutRetries; attempt++ {
		ctx, cancel := context.WithTimeout(parent, timeout)
		maybeMiseTrust(ctx, wtPath)
		failedCmd, output, runErr = e.runVerifyCommands(ctx, taskID, wtPath, cmds)
		cancel()
		if !errors.Is(runErr, context.DeadlineExceeded) || parent.Err() != nil {
			return failedCmd, output, runErr
		}
		if attempt < verifyChecksTimeoutRetries {
			e.logger.Warn("workflow.verify-checks.timeout-retry",
				"task_id", taskID, "step", stepID, "attempt", attempt+1, "budget", timeout.String())
		}
	}
	return failedCmd, output, runErr
}

func (e *Engine) healToolchainAndRetry(taskID, wtPath string, cmds []string, timeout time.Duration, stepID string) (attempted bool, failedCmd, output string, runErr error) {
	setupCtx, cancel := context.WithTimeout(e.ctx, timeout)
	setup := e.checks.SetupCommands(setupCtx, taskID)
	if len(setup) == 0 {
		cancel()
		return false, "", "", nil
	}
	e.logger.Warn("workflow.verify-checks.toolchain-heal",
		"task_id", taskID, "step", stepID, "setup_commands", len(setup))
	maybeMiseTrust(setupCtx, wtPath)
	sFailed, sOut, sErr := e.runVerifyCommands(setupCtx, taskID, wtPath, setup)
	cancel()
	if sFailed != "" || sErr != nil {
		e.logger.Warn("workflow.verify-checks.toolchain-heal.setup-failed",
			"task_id", taskID, "cmd", trimDiffLine(sFailed), "err", sErr, "tail", tailString(sOut, 400))
		return false, "", "", nil
	}
	failedCmd, output, runErr = e.runVerifySuiteWithRetry(e.ctx, taskID, wtPath, cmds, timeout, stepID)
	e.logger.Info("workflow.verify-checks.toolchain-heal.reran",
		"task_id", taskID, "step", stepID, "still_failing", failedCmd != "" || runErr != nil)
	return true, failedCmd, output, runErr
}

func (e *Engine) flagVerifyChecks(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("verify-checks: set human-required: %w", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionVerifyChecks, evidence.ProofDeterministicCheck, 1, "", reason)
	e.logger.Warn("workflow.verify-checks.flagged", "task_id", taskID, "detail", detail)
	return stepDone(step, "flagged")
}

func (e *Engine) blockVerifyChecks(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.Blocked, reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("verify-checks: set blocked: %w", statusErr)
	}
	e.recordEvidence(taskID, step.ID, evidenceCriterionVerifyChecks, evidence.ProofDeterministicCheck, 1, "", reason)
	e.logger.Warn("workflow.verify-checks.blocked", "task_id", taskID, "detail", detail)
	return stepDone(step, "blocked")
}

func (e *Engine) classifyVerifyFailure(taskID, wtPath, failedCmd, output string) *verifyFailureClassification {
	if failedCmd == "" {
		return nil
	}
	if failureclassify.IsGoInfraFailure(output) {
		return &verifyFailureClassification{
			Kind:   failureclassify.InfraFailure,
			Reason: "verify suite hit Go toolchain/build-cache instability (linker terminated or cache artifacts vanished) — blocked as verifier infrastructure, not an implementation failure",
		}
	}
	lintFiles, changedFiles, ok := classifyCodeFixableLintFailure(
		e.ctx, taskID, wtPath, failedCmd, output, e.focusedChecksBaseRef(taskID))
	if ok {
		target := "changed Go file `" + lintFiles[0] + "`"
		if len(lintFiles) > 1 {
			target = "changed Go files `" + strings.Join(lintFiles, "`, `") + "`"
		}
		return &verifyFailureClassification{
			Kind:         failureclassify.CodeFixableLint,
			Reason:       "verify suite failed on " + target + " in " + trimDiffLine(failedCmd) + " — deterministic lint finding; re-ask implementation to fix the code without weakening the check",
			ChangedFiles: changedFiles,
			AutoFixable:  true,
		}
	}
	if excerpt, changedFiles, ok := classifyDeterministicFrontendVerifyFailure(
		e.ctx, taskID, wtPath, failedCmd, output, e.focusedChecksBaseRef(taskID)); ok {
		reason := "verify suite failed on deterministic frontend check in " + trimDiffLine(failedCmd) +
			" — re-ask implementation to fix the code without weakening the check"
		if excerpt != "" {
			reason += " (latest failure: " + trimDiffLine(excerpt) + ")"
		}
		return &verifyFailureClassification{
			Kind:         failureclassify.FrontendDeterministicFailure,
			Reason:       reason,
			ChangedFiles: changedFiles,
			AutoFixable:  true,
		}
	}
	pkgs, changedFiles, ok := classifyUnrelatedVerifyGoFailure(e.ctx, taskID, wtPath, failedCmd, output, e.focusedChecksBaseRef(taskID))
	if !ok {
		return nil
	}
	return &verifyFailureClassification{
		Kind:           failureclassify.UnrelatedFailure,
		Reason:         "verify suite failed only in untouched Go package(s): " + strings.Join(pkgs, ", ") + " — blocked as a pre-existing/unrelated verifier failure, not this diff",
		FailedPackages: pkgs,
		ChangedFiles:   changedFiles,
	}
}

func classifyDeterministicFrontendVerifyFailure(
	parentCtx context.Context,
	taskID, wtPath, failedCmd, output, worktreeBaseRef string,
) (excerpt string, changedFiles []string, ok bool) {
	if !looksLikeFrontendVerifyCommand(failedCmd) {
		return "", nil, false
	}
	excerpt = highestSignalVerifyFailureExcerpt(failedCmd, output)
	if excerpt == "" {
		return "", nil, false
	}
	changedFiles, err := changedFilesSinceProjectBase(parentCtx, wtPath, worktreeBaseRef)
	if err != nil {
		return "", nil, false
	}
	changed := changedFrontendFiles(changedFiles)
	if len(changed) == 0 {
		return "", changedFiles, false
	}
	mentioned := frontendPathsMentioned(output)
	if len(mentioned) == 0 {
		return excerpt, changedFiles, true
	}
	for _, file := range mentioned {
		if changed[file] {
			return excerpt, changedFiles, true
		}
	}
	return "", changedFiles, false
}

func classifyCodeFixableLintFailure(
	parentCtx context.Context,
	taskID, wtPath, failedCmd, output, worktreeBaseRef string,
) (lintFiles, changedFiles []string, ok bool) {
	if !strings.Contains(failedCmd, "golangci-lint") {
		return nil, nil, false
	}
	changedFiles, err := changedFilesSinceProjectBase(parentCtx, wtPath, worktreeBaseRef)
	if err != nil {
		return nil, nil, false
	}
	lintFiles = parseGolangCILintGoFiles(output)
	if len(lintFiles) == 0 {
		return nil, changedFiles, false
	}
	for _, lintFile := range lintFiles {
		if !slices.Contains(changedFiles, lintFile) {
			return nil, changedFiles, false
		}
	}
	return lintFiles, changedFiles, true
}

func parseGolangCILintGoFiles(output string) []string {
	seen := map[string]bool{}
	var files []string
	for raw := range strings.SplitSeq(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		match := verifyGolangCILintFindingRe.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		file, ok := normalizeRepoGoFile(match[1])
		if !ok || seen[file] {
			continue
		}
		seen[file] = true
		files = append(files, file)
	}
	return files
}

func normalizeRepoGoFile(file string) (string, bool) {
	file = strings.TrimSpace(file)
	if file == "" || filepath.Ext(file) != ".go" || filepath.IsAbs(file) {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(file))
	if clean == "." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func looksLikeFrontendVerifyCommand(cmd string) bool {
	lower := strings.ToLower(strings.Join(strings.Fields(cmd), " "))
	if lower == "" {
		return false
	}
	if !strings.Contains(lower, "frontend") &&
		!strings.Contains(lower, "vitest") &&
		!strings.Contains(lower, "svelte-check") &&
		!strings.Contains(lower, "test:coverage") {
		return false
	}
	return strings.Contains(lower, "npm") ||
		strings.Contains(lower, "pnpm") ||
		strings.Contains(lower, "yarn") ||
		strings.Contains(lower, "vitest") ||
		strings.Contains(lower, "svelte-check")
}

func changedFrontendFiles(files []string) map[string]bool {
	out := map[string]bool{}
	for _, file := range files {
		file = filepath.ToSlash(strings.TrimSpace(file))
		if strings.HasPrefix(file, "frontend/") {
			out[file] = true
		}
	}
	return out
}

func frontendPathsMentioned(output string) []string {
	seen := map[string]bool{}
	var out []string
	for _, match := range verifyFrontendPathMentionRe.FindAllStringSubmatch(output, -1) {
		if len(match) < 2 {
			continue
		}
		file, ok := normalizeFrontendMentionedPath(match[1])
		if !ok || seen[file] {
			continue
		}
		seen[file] = true
		out = append(out, file)
	}
	return out
}

func normalizeFrontendMentionedPath(file string) (string, bool) {
	file = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
	if file == "." || strings.HasPrefix(file, "../") || filepath.IsAbs(file) {
		return "", false
	}
	file = strings.TrimPrefix(file, "./")
	if strings.HasPrefix(file, "src/") {
		file = "frontend/" + file
	}
	if !strings.HasPrefix(file, "frontend/") {
		return "", false
	}
	return file, true
}

func highestSignalVerifyFailureExcerpt(failedCmd, output string) string {
	if looksLikeFrontendVerifyCommand(failedCmd) {
		if excerpt := frontendFailureExcerpt(output); excerpt != "" {
			return excerpt
		}
	}
	return ""
}

func frontendFailureExcerpt(output string) string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(output), "\r\n", "\n"), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || line == "$" || strings.HasPrefix(line, "$ ") {
			continue
		}
		if !matchesAnyVerifyFrontendSignal(line) {
			continue
		}
		var excerpt []string
		for j := i; j < len(lines) && len(excerpt) < 4; j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" {
				if len(excerpt) > 0 {
					break
				}
				continue
			}
			if strings.HasPrefix(next, "$ ") {
				break
			}
			excerpt = append(excerpt, next)
		}
		return strings.Join(excerpt, "\n")
	}
	return ""
}

func matchesAnyVerifyFrontendSignal(line string) bool {
	for _, re := range verifyFrontendDeterministicSignalRes {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

func classifyUnrelatedVerifyGoFailure(
	parentCtx context.Context,
	taskID, wtPath, failedCmd, output, worktreeBaseRef string,
) (failedPackages, changedFiles []string, ok bool) {
	if !strings.Contains(failedCmd, "go test") {
		return nil, nil, false
	}
	changedFiles, err := changedFilesSinceProjectBase(parentCtx, wtPath, worktreeBaseRef)
	if err != nil || hasGlobalGoSurfaceChange(changedFiles) {
		return nil, nil, false
	}
	modulePath := goModulePath(wtPath)
	failedPackages = parseFailedGoPackages(output, modulePath)
	if len(failedPackages) == 0 {
		return nil, changedFiles, false
	}
	changedPkgs := changedGoPackages(changedFiles)
	for _, pkg := range failedPackages {
		affected, err := goPackageAffectedByChanges(parentCtx, taskID, wtPath, pkg, changedPkgs, modulePath)
		if err != nil || affected {
			return nil, changedFiles, false
		}
	}
	return failedPackages, changedFiles, true
}

func hasGlobalGoSurfaceChange(changedFiles []string) bool {
	for _, file := range changedFiles {
		switch file {
		case "go.mod", "go.sum", "go.work", "go.work.sum":
			return true
		}
	}
	return false
}

func goModulePath(wtPath string) string {
	data, err := os.ReadFile(filepath.Join(wtPath, "go.mod"))
	if err != nil {
		return ""
	}
	for raw := range strings.SplitSeq(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if moduleLine, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(moduleLine)
		}
	}
	return ""
}

func parseFailedGoPackages(output, modulePath string) []string {
	seen := map[string]bool{}
	var out []string
	for raw := range strings.SplitSeq(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "# "):
			pkg := normalizeFailedGoPackage(strings.TrimSpace(strings.TrimPrefix(line, "# ")), modulePath)
			if pkg != "" && !seen[pkg] {
				seen[pkg] = true
				out = append(out, pkg)
			}
		case strings.HasPrefix(line, "FAIL"):
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "FAIL" {
				continue
			}
			pkg := normalizeFailedGoPackage(fields[1], modulePath)
			if pkg != "" && !seen[pkg] {
				seen[pkg] = true
				out = append(out, pkg)
			}
		}
	}
	return out
}

func normalizeFailedGoPackage(pkg, modulePath string) string {
	pkg = strings.TrimSpace(strings.TrimSuffix(pkg, ":"))
	if pkg == "" || strings.Contains(pkg, "[") {
		return ""
	}
	if modulePath != "" {
		if pkg == modulePath {
			return "."
		}
		if rel, ok := strings.CutPrefix(pkg, modulePath+"/"); ok {
			return filepath.ToSlash(rel)
		}
	}
	if strings.HasPrefix(pkg, "./") {
		return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(pkg)), "./")
	}
	if strings.HasPrefix(pkg, "internal/") || strings.HasPrefix(pkg, "cmd/") {
		return filepath.ToSlash(filepath.Clean(pkg))
	}
	return ""
}

func changedGoPackages(changedFiles []string) map[string]bool {
	out := map[string]bool{}
	for _, file := range changedFiles {
		if filepath.Ext(file) != ".go" {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(file))
		if dir == "." {
			out["."] = true
			continue
		}
		out[dir] = true
	}
	return out
}

func goPackageAffectedByChanges(
	parentCtx context.Context,
	taskID, wtPath, failedPkg string,
	changedPkgs map[string]bool,
	modulePath string,
) (bool, error) {
	if failedPkg == "." || changedPkgs[failedPkg] {
		return true, nil
	}
	if len(changedPkgs) == 0 {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(parentCtx, shellTimeout)
	defer cancel()
	target := "."
	if failedPkg != "." {
		target = "./" + failedPkg
	}
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", target)
	cmd.Dir = wtPath
	env, err := verifyCommandEnv(ctx, taskID, wtPath)
	if err != nil {
		return false, err
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	for raw := range strings.SplitSeq(string(out), "\n") {
		pkg := normalizeFailedGoPackage(raw, modulePath)
		if pkg != "" && changedPkgs[pkg] {
			return true, nil
		}
	}
	return false, nil
}

// autoFixBackoff is the re-dispatch delay before the next verify/focused
// auto-fix attempt. It grows linearly with the attempt count so a genuinely
// stuck fix loop paces itself down instead of hammering the fleet, capped at
// autoFixBackoffMax so a long-running loop still makes steady progress.
func autoFixBackoff(attempts int) time.Duration {
	backoff := verifyChecksAutoFixBackoff * time.Duration(attempts+1)
	if backoff > autoFixBackoffMax {
		return autoFixBackoffMax
	}
	return backoff
}

// verifyDiagnosticName is the sidecar a verify escalation leaves behind.
const verifyDiagnosticName = ".sybra-verify-%s.md"

// writeVerifyDiagnostic persists the failing command and the highest-signal
// excerpt, returning the path it wrote or "".
//
// status_reason records the command trimmed to a single line and drops the
// diagnostic entirely, so whoever picks the task up next — a human, or the
// human-review autonomy agent whose mandate tells it to "re-run the exact
// failing command" — gets a command but not the finding, and has to re-run a
// multi-minute suite to learn what it already said. The re-ask path has built
// this excerpt all along; only the escalation threw it away.
//
// Best-effort: a task that cannot be escalated because a sidecar would not
// write is strictly worse than one escalated without it.
func (e *Engine) writeVerifyDiagnostic(taskID, failedCmd, output string) string {
	dir := e.resolveSidecarDir(taskID)
	if dir == "" || taskID == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Verify failure — task ")
	b.WriteString(taskID)
	b.WriteString("\n\n## Failing command\n\n```\n")
	b.WriteString(failedCmd)
	b.WriteString("\n```\n")
	if excerpt := highestSignalVerifyFailureExcerpt(failedCmd, output); excerpt != "" {
		b.WriteString("\n## Highest-signal failure excerpt\n\n```\n")
		b.WriteString(excerpt)
		b.WriteString("\n```\n")
	}
	// The structured excerpt only covers frontend commands, so a Go lint or
	// test failure — the majority of what escalates — would otherwise record
	// nothing but the command. Mirror buildVerifyReaskNote and keep the tail.
	if tail := tailString(strings.TrimSpace(output), 3000); tail != "" {
		b.WriteString("\n## Output tail\n\n```\n")
		b.WriteString(tail)
		b.WriteString("\n```\n")
	}
	path := filepath.Join(dir, fmt.Sprintf(verifyDiagnosticName, taskID))
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		e.logger.Warn("workflow.verify-checks.diagnostic", "task_id", taskID, "err", err)
		return ""
	}
	return path
}

// withDiagnostic appends a pointer to the sidecar so status_reason stays short
// while still leading somewhere with the actual finding.
func withDiagnostic(reason, path string) string {
	if path == "" {
		return reason
	}
	return reason + " — diagnostic: " + path
}

// autoFixOrFlagVerifyChecks rewinds the workflow to the implementation step and
// re-asks the agent to fix a code-fixable verify failure at its root cause,
// looping instead of escalating an ordinary lint/test failure the agent can fix
// itself. It stays in that loop up to verifyChecksAutoFixCeiling re-asks; only a
// deterministic failure that survives that many attempts, or the structural case
// with no implementation step to rewind into (wfExec nil, or the step never
// ran), escalates to human-required.
func (e *Engine) autoFixOrFlagVerifyChecks(taskID string, step *Step, wfExec *Execution, t TaskInfo, reason, failedCmd, output string) (StepOutput, error) {
	if wfExec == nil || wfExec.CountStep(verifyChecksImplStepID) == 0 {
		diag := e.writeVerifyDiagnostic(taskID, failedCmd, output)
		return e.flagVerifyChecks(taskID, step, withDiagnostic(reason, diag), failedCmd)
	}
	rewoundRunAgentID := verifyAutoFixRewoundRunAgentID(wfExec, t)
	fingerprint := autoFixFailureFingerprint(failedCmd, output)
	armed, attempt, err := e.rewindRetry(taskID, wfExec, t, rewindRetryPolicy{
		counterKey:  "step." + step.ID + ".auto_fix",
		max:         verifyChecksAutoFixCeiling,
		rewindStep:  verifyChecksImplStepID,
		backoff:     autoFixBackoff,
		fingerprint: fingerprint,
		// The first occurrence earns one autonomous repair attempt. If the
		// exact command and failure evidence survive that code-author run, the
		// second occurrence proves the repair made no progress and must stop
		// the loop immediately.
		maxSameFingerprintRuns: 1,
		attemptProducedWork:    lastAuthorRunProducedWork,
		onArm: func(wfExec *Execution, attempt int) {
			wfExec.SetVar(verifyReaskNoteVar, buildVerifyReaskNote(failedCmd, output))
			wfExec.SetVar(verifyRetryModelVar, "expensive")
			if rewoundRunAgentID != "" {
				wfExec.SetVar(verifyAutoFixRunIDVar, rewoundRunAgentID)
			}
		},
		reason: func(attempt int) string {
			return fmt.Sprintf("auto-fixing failed verify check (attempt %d): %s", attempt, trimDiffLine(failedCmd))
		},
	})
	if err != nil {
		return StepOutput{}, fmt.Errorf("verify-checks: rewind to implement: %w", err)
	}
	if !armed {
		exhausted := fmt.Sprintf("%s — escalating after repeated identical auto-fix failures or %d attempts without passing",
			reason, verifyChecksAutoFixCeiling)
		diag := e.writeVerifyDiagnostic(taskID, failedCmd, output)
		return e.flagVerifyChecks(taskID, step, withDiagnostic(exhausted, diag), "auto-fix-exhausted: "+trimDiffLine(failedCmd))
	}
	e.logger.Info("workflow.verify-checks.auto-fix",
		"task_id", taskID, "attempt", attempt, "cmd", trimDiffLine(failedCmd))
	return StepOutput{}, errStepParked
}

func autoFixFailureFingerprint(failedCmd, output string) string {
	excerpt := highestSignalVerifyFailureExcerpt(failedCmd, output)
	if excerpt == "" {
		excerpt = tailString(strings.TrimSpace(output), 1200)
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(failedCmd) + "\n" + excerpt))
	return hex.EncodeToString(h[:])
}

func buildVerifyReaskNote(failedCmd, output string) string {
	var b strings.Builder
	excerpt := highestSignalVerifyFailureExcerpt(failedCmd, output)
	b.WriteString("A prior implementation FAILED the project verify suite. Fix the ROOT CAUSE ")
	b.WriteString("so the failing command passes on a clean run — do NOT weaken, skip, or edit ")
	b.WriteString("the check to make it pass. Then COMMIT and push your fix: the suite runs ")
	b.WriteString("against your branch HEAD in a freshly prepared worktree, so an uncommitted ")
	b.WriteString("change is not picked up and the same failure recurs; some projects also ")
	b.WriteString("enforce a clean working tree (e.g. a `git diff --exit-code` / generated-file ")
	b.WriteString("gate) that fails outright on uncommitted changes.\n\n## Failing verify command\n\n`")
	b.WriteString(failedCmd)
	b.WriteString("`")
	if excerpt != "" {
		b.WriteString("\n\n## Highest-signal failure excerpt\n\n```\n")
		b.WriteString(excerpt)
		b.WriteString("\n```")
	}
	b.WriteString("\n\n## Output (tail)\n\n```\n")
	b.WriteString(tailString(strings.TrimSpace(output), 3000))
	b.WriteString("\n```")
	return b.String()
}

func verifyAutoFixRewoundRunAgentID(wfExec *Execution, t TaskInfo) string {
	if wfExec != nil {
		if rec := wfExec.RecordForStep(verifyChecksImplStepID); rec != nil && strings.TrimSpace(rec.AgentID) != "" {
			return rec.AgentID
		}
	}
	for i := range slices.Backward(t.AgentRuns) {
		role := strings.TrimSpace(t.AgentRuns[i].Role)
		if role == "" || role == "implementation" {
			if id := strings.TrimSpace(t.AgentRuns[i].AgentID); id != "" {
				return id
			}
		}
	}
	return ""
}

// miseConfigNames are the mise config filenames that gate `mise exec` behind
// "config not trusted", checked wherever this code needs to know whether a
// directory carries mise config — kept as one list so npmReinstallCommand and
// maybeMiseTrust can never drift out of sync on which names count (notably
// .mise.local.toml, easy to miss since it's less common than mise.toml).
var miseConfigNames = []string{"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml"}

// maybeMiseTrust trusts a mise config in dir before running verify commands or
// a toolchain repair there, mirroring worktree setup. A task that adds or
// edits mise config would otherwise hit "config not trusted" and fail verify
// on honest work. Best-effort: errors are ignored (the verify command surfaces
// any real issue).
func maybeMiseTrust(ctx context.Context, dir string) {
	for _, name := range miseConfigNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			cmd := exec.CommandContext(ctx, "sh", "-c", "mise trust --yes")
			cmd.Dir = dir
			_ = cmd.Run()
			return
		}
	}
}

// repairTornNodeModulesTimeout bounds the best-effort `npm ci` repair below,
// separate from the verify budget so a repair can never eat into the time
// the actual verify commands get.
const repairTornNodeModulesTimeout = 3 * time.Minute

// repairTornNodeModules scans the worktree root and immediate subdirectories
// for an npm project (a package.json) whose node_modules looks torn and repairs
// it with a single `npm ci --ignore-scripts` before verify commands trust the
// install. This is a defensive fix for a race between verify_checks and a
// still-running (or killed mid-flight) `npm ci` left behind by something else
// in the same worktree, such as an implementation agent's own backgrounded
// `mise run verify`/`npm ci` that its session never waited on: the setup-time
// install was healthy, but by the time verify_checks runs, node_modules can be
// left half torn-down, and every npm-run command in it then fails with
// "command not found" — a false implementation-defect signal. Best-effort: a
// repair failure surfaces via the verify command's own failure, same as before
// this existed.
//
// Lifecycle scripts are deliberately disabled because package.json is
// branch-controlled; verify command configuration comes from the trusted base
// branch, and this best-effort repair must not introduce a new unsandboxed
// execution path controlled by the implementation branch.
//
// "Torn" mirrors the staleness check scripts/sybra-dev-supervisor.sh already
// uses for the desktop dev loop: node_modules/.bin missing (an install that
// never completed or was gutted mid-run), or npm's own
// node_modules/.package-lock.json stamp missing/older than package-lock.json
// (an install that was interrupted before npm finished writing its stamp).
func (e *Engine) repairTornNodeModules(ctx context.Context, taskID, wtPath string) {
	e.repairTornNodeModulesInDir(ctx, taskID, wtPath, ".")
	entries, err := os.ReadDir(wtPath)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(wtPath, entry.Name())
		e.repairTornNodeModulesInDir(ctx, taskID, dir, entry.Name())
	}
}

func (e *Engine) repairTornNodeModulesInDir(ctx context.Context, taskID, dir, label string) {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "package-lock.json")); err != nil {
		return // npm ci requires a lockfile; skip non-npm or lockfile-less projects
	}
	nodeModules := filepath.Join(dir, "node_modules")
	info, err := os.Stat(nodeModules)
	if err != nil || !info.IsDir() {
		return // no install yet — not this gate's problem
	}
	if !nodeModulesTorn(dir, nodeModules) {
		return
	}

	e.logger.Warn("workflow.verify-checks.npm-repair", "task_id", taskID, "dir", label)
	rctx, cancel := context.WithTimeout(ctx, repairTornNodeModulesTimeout)
	defer cancel()
	cmd := exec.CommandContext(rctx, "npm", "ci", "--ignore-scripts")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		e.logger.Warn("workflow.verify-checks.npm-repair-failed", "task_id", taskID, "dir", label, "err", err)
	}
}

// nodeModulesTorn reports whether nodeModules (an existing directory inside
// dir) looks like an install that never finished cleanly.
func nodeModulesTorn(dir, nodeModules string) bool {
	binInfo, err := os.Stat(filepath.Join(nodeModules, ".bin"))
	if err != nil || !binInfo.IsDir() {
		return true
	}
	lockPath := filepath.Join(dir, "package-lock.json")
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		return false // no lockfile to compare against — .bin presence is all we can check
	}
	stampInfo, err := os.Stat(filepath.Join(nodeModules, ".package-lock.json"))
	if err != nil {
		return true // npm never finished writing its own stamp
	}
	return stampInfo.ModTime().Before(lockInfo.ModTime())
}

func stepDone(step *Step, output string) (StepOutput, error) {
	return StepOutput{StepID: step.ID, Status: "completed", Output: output}, nil
}

// ensureNodeToolchain re-provisions a verify command's Node toolchain when it
// has silently gone corrupt since worktree setup. Setup's own `npm ci`
// (internal/worktree.runSetup) can succeed and then have its output emptied
// later by unrelated disk/memory pressure from concurrent worktree
// provisioning — the worktree ends up with node_modules entries present but
// zero-byte, so a later verify command like `npm run build:web` fails with
// e.g. "vite: command not found" on an implementation that never touched the
// frontend. Best-effort: any error here is swallowed and left for the verify
// command itself to surface, so a repair failure never masks or replaces the
// original failure signal. Every `cd`-scoped directory in the command is
// checked (see nodeToolchainDirs), so a chained command that touches more than
// one package repairs each.
func (e *Engine) ensureNodeToolchain(ctx context.Context, taskID, wtPath, rawCmd string, tail io.Writer) {
	for _, dir := range nodeToolchainDirs(wtPath, rawCmd) {
		e.repairCorruptToolchain(ctx, taskID, wtPath, dir, tail)
	}
}

// nodeToolchainDirs returns the worktree directories whose Node toolchain a
// verify command touches: every `cd <dir>` leg of the command (a chained
// command like `(cd a && ...) && (cd b && ...)` touches both), or the worktree
// root when the command has no `cd`. Captures that would escape the worktree
// (absolute paths or `..` traversal) are dropped as a defense-in-depth guard —
// rawCmd is project-config-controlled, so this stays inside the worktree.
func nodeToolchainDirs(wtPath, rawCmd string) []string {
	matches := cdDirPattern.FindAllStringSubmatch(rawCmd, -1)
	if len(matches) == 0 {
		return []string{wtPath}
	}
	var dirs []string
	seen := map[string]bool{}
	for _, m := range matches {
		rel := strings.Trim(m[1], `"'`)
		if rel == "" || filepath.IsAbs(rel) {
			continue
		}
		joined := filepath.Join(wtPath, rel)
		within, err := filepath.Rel(wtPath, joined)
		if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
			continue // escapes the worktree — skip
		}
		if !seen[joined] {
			seen[joined] = true
			dirs = append(dirs, joined)
		}
	}
	return dirs
}

// repairCorruptToolchain re-provisions a single directory's Node toolchain when
// it looks missing or has silently gone corrupt since worktree setup (see
// ensureNodeToolchain). A truncated install can leave node_modules/.bin
// entirely absent (ReadDir error), present but empty, or present with
// zero-byte entries — all three shapes are treated as "needs a repair",
// matching the missing-or-corrupted contract from the PR description.
func (e *Engine) repairCorruptToolchain(ctx context.Context, taskID, wtPath, dir string, tail io.Writer) {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return // not a Node project — nothing to check
	}
	binDir := filepath.Join(dir, "node_modules", ".bin")
	entries, err := os.ReadDir(binDir)
	if err == nil && nodeModulesBinNonEmpty(binDir, entries) {
		return // toolchain looks intact
	}

	reinstall := npmReinstallCommand(dir, wtPath)
	e.logger.Warn("workflow.verify-checks.toolchain-corrupt", "task_id", taskID, "dir", dir)
	_, _ = fmt.Fprintf(tail,
		"[verify] node_modules/.bin in %s looks missing or corrupt — re-running %q\n", dir, reinstall)

	repairCtx, cancel := context.WithTimeout(ctx, npmReinstallTimeout)
	defer cancel()
	maybeMiseTrust(repairCtx, wtPath)
	if dir != wtPath {
		maybeMiseTrust(repairCtx, dir)
	}
	cmd := exec.CommandContext(repairCtx, "sh", "-c", reinstall)
	cmd.Dir = dir
	cmd.Stdout = tail
	cmd.Stderr = tail
	if repairErr := cmd.Run(); repairErr != nil {
		e.logger.Warn("workflow.verify-checks.toolchain-repair-failed", "task_id", taskID, "dir", dir, "err", repairErr)
		_, _ = fmt.Fprintf(tail, "[verify] %s repair failed: %v\n", reinstall, repairErr)
		return
	}
	e.logger.Info("workflow.verify-checks.toolchain-repaired", "task_id", taskID, "dir", dir)
	_, _ = io.WriteString(tail, "[verify] npm ci repair completed\n")
}

// npmReinstallCommand routes the repair through `mise exec --` when either the
// repaired dir or the worktree root carries a mise config. Some repos keep the
// config in `frontend/` only; falling back to the repair dir avoids a false
// bare `npm ci` on hosts where npm is available only through mise.
func npmReinstallCommand(paths ...string) string {
	for _, dir := range paths {
		for _, name := range miseConfigNames {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return "mise exec -- npm ci"
			}
		}
	}
	return "npm ci"
}

// nodeModulesBinNonEmpty reports whether at least one entry under
// node_modules/.bin resolves to a non-empty file. A truncated npm install
// leaves the directory entries in place (ls still lists them) but their
// content emptied (`du -sh` reports 0 bytes) — checking sizes, not just
// presence, is what catches that failure mode.
func nodeModulesBinNonEmpty(binDir string, entries []os.DirEntry) bool {
	for _, ent := range entries {
		info, err := os.Stat(filepath.Join(binDir, ent.Name())) // Stat follows symlinks (.bin entries are usually symlinks)
		if err == nil && info.Size() > 0 {
			return true
		}
	}
	return false
}

// repairCorruptedNodeModules scans the worktree root and its immediate
// subdirectories (e.g. `frontend/`) for an npm project whose node_modules
// was left behind by an interrupted install, and repairs it with a fresh
// `npm ci` before the verify commands run.
//
// Under host memory pressure, a concurrent `npm ci` in another worktree can
// get SIGKILLed mid-write, leaving a partially-populated node_modules that
// fails every subsequent build deterministically (e.g. "vite: command not
// found") even though package.json/package-lock.json are untouched and an
// earlier build in the same worktree succeeded. That looks like a genuine
// build regression caused by the task's diff but is really a broken
// toolchain state. A failed repair is reported back to the caller (rather
// than silently left for the verify command to surface) so a still-broken
// install is never run through verify and misattributed to the
// implementation as a product failure.
func (e *Engine) repairCorruptedNodeModules(ctx context.Context, taskID, wtPath string) (repairFailed bool) {
	entries, err := os.ReadDir(wtPath)
	if err != nil {
		return false
	}
	candidates := []string{wtPath}
	for _, entry := range entries {
		if entry.IsDir() {
			candidates = append(candidates, filepath.Join(wtPath, entry.Name()))
		}
	}
	for _, dir := range candidates {
		if !isCorruptedNodeModules(dir) {
			continue
		}
		e.logger.Warn("workflow.verify-checks.node-modules-repair", "task_id", taskID, "dir", dir)
		repairCtx, cancel := context.WithTimeout(ctx, verifyChecksNodeModulesRepairTimeout)
		maybeMiseTrust(repairCtx, wtPath)
		if dir != wtPath {
			maybeMiseTrust(repairCtx, dir)
		}
		reinstall := npmReinstallCommand(dir, wtPath)
		cmd := exec.CommandContext(repairCtx, "sh", "-c", reinstall)
		cmd.Dir = dir
		repairErr := cmd.Run()
		cancel()
		if repairErr != nil {
			e.logger.Warn("workflow.verify-checks.node-modules-repair-failed",
				"task_id", taskID, "dir", dir, "cmd", reinstall, "err", repairErr)
			repairFailed = true
		}
	}
	return repairFailed
}

// isCorruptedNodeModules reports whether dir looks like an npm project whose
// node_modules was left partially installed by a killed `npm ci`: present
// and non-empty, but missing node_modules/.bin or
// node_modules/.package-lock.json — both of which a completed `npm ci`
// always writes. A node_modules that was never installed (absent entirely)
// is not corrupted, just not yet set up, so it is left alone here.
//
// The repair runs `npm ci`, so it only fires for npm-owned installs: dir must
// have an npm lockfile (package-lock.json or npm-shrinkwrap.json) and must not
// carry a pnpm/yarn/bun lockfile that hands the install to another package
// manager. Otherwise a pnpm/yarn/bun workspace — whose node_modules
// legitimately lacks node_modules/.package-lock.json — would be mistaken for
// corruption and clobbered by an unrelated `npm ci`.
func isCorruptedNodeModules(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return false
	}
	if !ownedByNpm(dir) {
		return false
	}
	nm := filepath.Join(dir, "node_modules")
	if !dirNonEmpty(nm) {
		return false
	}
	_, binErr := os.Stat(filepath.Join(nm, ".bin"))
	_, lockErr := os.Stat(filepath.Join(nm, ".package-lock.json"))
	return binErr != nil || lockErr != nil
}

// dirNonEmpty reports whether dir exists and contains at least one entry,
// without reading the full directory listing — node_modules on a large
// install can hold tens of thousands of entries.
func dirNonEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// ownedByNpm reports whether dir's install is owned by npm: it has an npm
// lockfile and no competing pnpm/yarn/bun lockfile. A non-npm lockfile wins,
// since `npm ci` must not run against another package manager's workspace.
func ownedByNpm(dir string) bool {
	for _, name := range []string{"pnpm-lock.yaml", "yarn.lock", "bun.lockb", "bun.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return false
		}
	}
	for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// runVerifyCommands runs each command in order in the worktree via `sh -c`.
// Returns the first command that exited non-zero on every attempt (a real
// failure → caller blocks). A failing command is retried up to
// verifyChecksFlakeRetries times to absorb nondeterministic flakes; a command
// that passes on any attempt moves on. A non-nil err means the suite could not
// even be prepared/run cleanly (ctx timeout/cancel or isolated-cache prep
// failure) and is never retried — the budget is already spent; the caller
// decides the policy. Output streams into a fixed-size head+tail buffer (see
// boundedTail) so a flood of stdout/stderr cannot exhaust memory.
func (e *Engine) runVerifyCommands(ctx context.Context, taskID, wtPath string, cmds []string) (failedCmd, output string, err error) {
	tail := &boundedTail{max: verifyChecksMaxOutput}
	cmdEnv, err := verifyCommandEnv(ctx, taskID, wtPath, cmds...)
	if err != nil {
		return "", tail.String(), err
	}
	for _, raw := range cmds {
		e.ensureNodeToolchain(ctx, taskID, wtPath, raw, tail)
		passed := false
		for attempt := 0; attempt <= verifyChecksFlakeRetries; attempt++ {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", tail.String(), ctxErr
			}
			if attempt > 0 {
				_, _ = fmt.Fprintf(tail,
					"[verify] command failed; retry %d/%d to rule out a flake\n", attempt, verifyChecksFlakeRetries)
				e.logger.Info("workflow.verify-checks.retry",
					"task_id", taskID, "attempt", attempt, "cmd", trimDiffLine(raw))
			}
			_, _ = io.WriteString(tail, "$ "+raw+"\n")
			cmd := exec.CommandContext(ctx, "sh", "-c", raw)
			cmd.Dir = wtPath
			cmd.Env = cmdEnv
			cmd.Stdout = tail
			cmd.Stderr = tail
			runErr := cmd.Run()
			_, _ = io.WriteString(tail, "\n")
			if runErr == nil {
				passed = true
				break // passed (possibly on retry) — go to the next command
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", tail.String(), ctxErr // deadline/cancel: do not retry
			}
		}
		if !passed {
			return raw, tail.String(), nil // failed every attempt → block
		}
	}
	return "", tail.String(), nil
}

// boundedTail is a concurrency-safe io.Writer that retains only `max` bytes
// total, so a flood of stdout/stderr cannot exhaust memory. os/exec writes
// stdout and stderr from separate goroutines when they share a non-*os.File
// writer, so Write must be guarded.
//
// It keeps the first half and the last half of the stream rather than a plain
// tail: `go test ./internal/...` across ~80 packages reports the failing
// package near where it fails, then keeps emitting `ok` lines for every
// package that finishes afterward — a tail-only buffer reliably evicts the
// one line (`--- FAIL: TestXxx`) a human needs to diagnose the run.
type boundedTail struct {
	mu         sync.Mutex
	max        int
	buf        []byte // accumulates here until total exceeds max
	head       []byte // frozen first-half once truncation begins
	tail       []byte // rolling last-half once truncation begins
	total      int
	truncating bool
}

func (b *boundedTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(p)
	if !b.truncating {
		b.buf = append(b.buf, p...)
		if len(b.buf) <= b.max {
			return len(p), nil
		}
		b.truncating = true
		half := min(b.max/2, len(b.buf))
		b.head = append([]byte(nil), b.buf[:half]...)
		rest := b.buf[half:]
		tailCap := b.max - len(b.head)
		if len(rest) > tailCap {
			rest = rest[len(rest)-tailCap:]
		}
		b.tail = append([]byte(nil), rest...)
		b.buf = nil
		return len(p), nil
	}
	tailCap := b.max - len(b.head)
	b.tail = append(b.tail, p...)
	if len(b.tail) > tailCap {
		b.tail = b.tail[len(b.tail)-tailCap:]
	}
	return len(p), nil
}

func (b *boundedTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.truncating {
		return string(b.buf)
	}
	elided := b.total - len(b.head) - len(b.tail)
	return string(b.head) + fmt.Sprintf("\n...(%d bytes elided)...\n", elided) + string(b.tail)
}

// tailString returns the last n bytes of s, prefixed with an elision marker
// when truncated. Debug output — not rune-aligned at the cut.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-n:]
}
