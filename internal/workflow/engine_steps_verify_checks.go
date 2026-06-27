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
	cmds := e.checks.VerifyCommands(taskID)
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

	timeout := e.verifyTimeout
	if timeout <= 0 {
		timeout = verifyChecksDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()

	maybeMiseTrust(ctx, wtPath)
	failedCmd, output, runErr := e.runVerifyCommands(ctx, taskID, wtPath, cmds)

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
			reason := "verify suite exceeded the time budget (" + timeout.String() +
				") — fix slow or hanging tests, or add the `verify-blessed` tag to override"
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
