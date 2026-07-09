package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

// verifyChecksDefaultTimeout bounds the whole verify run (every command). A test
// suite is slower than the 30s shellTimeout used by lighter steps. Overridable
// per-engine via SetVerifyTimeout (tests use a short value).
const verifyChecksDefaultTimeout = 10 * time.Minute

// verifyChecksMaxOutput bounds how much command output is retained in memory.
// A noisy or malicious verify command must not be able to OOM the engine, so
// output streams into a fixed-size tail buffer rather than a growing slice.
const verifyChecksMaxOutput = 64 * 1024

// verifyChecksOutputTail caps how much of that buffer is stored in the artifact.
const verifyChecksOutputTail = 8000

// verifyBlessedTag lets a human accept a verify failure (e.g. a known-flaky
// suite) and let the task proceed instead of re-blocking on every re-dispatch.
const verifyBlessedTag = "verify-blessed"

// verifyChecksFlakeRetries is how many extra times a failed verify command is
// re-run before the gate blocks. A single retry absorbs a nondeterministic
// flake (e.g. a test-teardown TempDir race) that would otherwise escalate
// unrelated work to human-required. A genuine failure fails every attempt and
// still blocks; the cost is one extra suite run only on the failing command.
const verifyChecksFlakeRetries = 1

const verifyChecksTimeoutRetries = 1

// npmReinstallTimeout bounds a single toolchain-repair `npm ci`, run only when
// ensureNodeToolchain detects a corrupt node_modules right before a verify
// command. Separate from the outer verify timeout so a repair attempt cannot
// eat the whole suite budget.
const npmReinstallTimeout = 3 * time.Minute

// cdDirPattern extracts the directory of a leading `cd <dir> &&` (optionally
// wrapped in parens/quotes) from a verify command string, e.g.
// "(cd frontend && mise exec -- npm run build:web)" -> "frontend". Verify
// commands are opaque shell strings (see resolveSetupCommands) with no
// metadata marking which ones touch a Node toolchain, so this is the only
// signal available for locating the package.json to integrity-check.
var cdDirPattern = regexp.MustCompile(`cd\s+([^\s&|;)]+)`)

// verifyChecksReport is the structured result, stored as a generic artifact.
type verifyChecksReport struct {
	Commands   []string `json:"commands"`
	FailedCmd  string   `json:"failedCmd,omitempty"`
	OutputTail string   `json:"outputTail,omitempty"`
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
// verify commands configured, or no worktree. A genuine command failure — or
// the suite exceeding the time budget (an agent could hang a test to dodge) —
// blocks. Only engine-shutdown cancellation fails open, so a harness problem on
// teardown never strands work.
func (e *Engine) execVerifyChecks(taskID string, step *Step, t TaskInfo) (StepOutput, error) {
	if slices.Contains(t.Tags, verifyBlessedTag) {
		e.logger.Info("workflow.verify-checks.blessed", "task_id", taskID)
		return stepDone(step, "blessed")
	}
	if e.checks == nil {
		return stepDone(step, "skipped: no check config getter")
	}

	timeout := e.verifyTimeout
	if timeout <= 0 {
		timeout = verifyChecksDefaultTimeout
	}
	cmds := e.checks.VerifyCommands(e.ctx, taskID)
	if len(cmds) == 0 {
		return stepDone(step, "skipped: no verify commands configured")
	}
	if e.worktrees == nil {
		return stepDone(step, "skipped: no worktree getter configured")
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok {
		return stepDone(step, "skipped: no worktree for task")
	}

	failedCmd, output, runErr := e.runVerifySuiteWithRetry(taskID, wtPath, cmds, timeout, step.ID)

	report := verifyChecksReport{
		Commands: cmds, FailedCmd: failedCmd, OutputTail: tailString(output, verifyChecksOutputTail),
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
		e.logger.Warn("workflow.verify-checks.canceled", "task_id", taskID, "err", runErr)
		return stepDone(step, "skipped: context canceled")
	}

	if failedCmd != "" {
		reason := "implementation does not pass the project verify suite: " + trimDiffLine(failedCmd) +
			" — fix the code, or add the `verify-blessed` tag to override (e.g. a known-flaky suite)"
		return e.flagVerifyChecks(taskID, step, reason, failedCmd)
	}

	e.logger.Info("workflow.verify-checks.clean", "task_id", taskID, "commands", len(cmds))
	return stepDone(step, "clean")
}

// flagVerifyChecks flips the task to human-required. A failed status write
// returns an error so the workflow stalls instead of advancing past the gate —
// the YAML transition keys off task.status, so a silently-failed write would
// otherwise route a failing implementation straight to review.
func (e *Engine) runVerifySuiteWithRetry(taskID, wtPath string, cmds []string, timeout time.Duration, stepID string) (failedCmd, output string, runErr error) {
	for attempt := 0; attempt <= verifyChecksTimeoutRetries; attempt++ {
		ctx, cancel := context.WithTimeout(e.ctx, timeout)
		maybeMiseTrust(ctx, wtPath)
		failedCmd, output, runErr = e.runVerifyCommands(ctx, taskID, wtPath, cmds)
		cancel()
		if !errors.Is(runErr, context.DeadlineExceeded) || e.ctx.Err() != nil {
			return failedCmd, output, runErr
		}
		if attempt < verifyChecksTimeoutRetries {
			e.logger.Warn("workflow.verify-checks.timeout-retry",
				"task_id", taskID, "step", stepID, "attempt", attempt+1, "budget", timeout.String())
		}
	}
	return failedCmd, output, runErr
}

func (e *Engine) flagVerifyChecks(taskID string, step *Step, reason, detail string) (StepOutput, error) {
	if statusErr := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); statusErr != nil {
		return StepOutput{}, fmt.Errorf("verify-checks: set human-required: %w", statusErr)
	}
	e.logger.Warn("workflow.verify-checks.flagged", "task_id", taskID, "detail", detail)
	return stepDone(step, "flagged")
}

// maybeMiseTrust trusts a mise config in the worktree before running verify
// commands, mirroring worktree setup. A task that adds or edits mise config
// would otherwise hit "config not trusted" and fail verify on honest work.
// Best-effort: errors are ignored (the verify command surfaces any real issue).
func maybeMiseTrust(ctx context.Context, wtPath string) {
	for _, name := range []string{"mise.toml", ".mise.toml", "mise.local.toml"} {
		if _, err := os.Stat(filepath.Join(wtPath, name)); err == nil {
			cmd := exec.CommandContext(ctx, "sh", "-c", "mise trust --yes")
			cmd.Dir = wtPath
			_ = cmd.Run()
			return
		}
	}
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
// original failure signal.
func (e *Engine) ensureNodeToolchain(ctx context.Context, taskID, wtPath, rawCmd string, tail io.Writer) {
	dir := wtPath
	if m := cdDirPattern.FindStringSubmatch(rawCmd); m != nil {
		dir = filepath.Join(wtPath, m[1])
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return // not a Node project — nothing to check
	}
	binDir := filepath.Join(dir, "node_modules", ".bin")
	entries, err := os.ReadDir(binDir)
	if err != nil || len(entries) == 0 {
		return // never installed (or setup's own npm ci is still the right owner) — leave it to the verify command
	}
	if nodeModulesBinNonEmpty(binDir, entries) {
		return // toolchain looks intact
	}

	e.logger.Warn("workflow.verify-checks.toolchain-corrupt", "task_id", taskID, "dir", dir)
	_, _ = fmt.Fprintf(tail,
		"[verify] node_modules/.bin in %s looks corrupt (entries present but empty) — re-running npm ci\n", dir)

	repairCtx, cancel := context.WithTimeout(ctx, npmReinstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(repairCtx, "sh", "-c", "npm ci")
	cmd.Dir = dir
	cmd.Stdout = tail
	cmd.Stderr = tail
	if repairErr := cmd.Run(); repairErr != nil {
		e.logger.Warn("workflow.verify-checks.toolchain-repair-failed", "task_id", taskID, "dir", dir, "err", repairErr)
		_, _ = fmt.Fprintf(tail, "[verify] npm ci repair failed: %v\n", repairErr)
		return
	}
	e.logger.Info("workflow.verify-checks.toolchain-repaired", "task_id", taskID, "dir", dir)
	_, _ = io.WriteString(tail, "[verify] npm ci repair completed\n")
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

// runVerifyCommands runs each command in order in the worktree via `sh -c`.
// Returns the first command that exited non-zero on every attempt (a real
// failure → caller blocks). A failing command is retried up to
// verifyChecksFlakeRetries times to absorb nondeterministic flakes; a command
// that passes on any attempt moves on. A non-nil err means the run could not
// complete (ctx timeout/cancel) and is never retried — the budget is already
// spent; the caller decides the policy (fail closed on our deadline, open on
// shutdown). Output streams into a fixed-size tail buffer so a flood of
// stdout/stderr cannot exhaust memory.
func (e *Engine) runVerifyCommands(ctx context.Context, taskID, wtPath string, cmds []string) (failedCmd, output string, err error) {
	tail := &boundedTail{max: verifyChecksMaxOutput}
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

// boundedTail is a concurrency-safe io.Writer that retains only the last `max`
// bytes written. os/exec writes stdout and stderr from separate goroutines when
// they share a non-*os.File writer, so Write must be guarded.
type boundedTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *boundedTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

func (b *boundedTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// tailString returns the last n bytes of s, prefixed with an elision marker
// when truncated. Debug output — not rune-aligned at the cut.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-n:]
}
