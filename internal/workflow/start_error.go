package workflow

import (
	"errors"
	"fmt"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

// startReasonMaxLen caps the human-facing reason written to task.StatusReason.
// Longer messages clutter the UI and rarely add value beyond the lead error.
const startReasonMaxLen = 200

// ErrDispatchInFlight is returned by an agent-start path when another dispatch
// for the same task is already in flight (the per-task dispatch claim is held).
// It is a benign, transient outcome — the in-flight dispatch will produce the
// task's agent — so it must never flip the task to human-required or write a
// scary status_reason. ClassifyAgentStartError maps it to an empty reason.
var ErrDispatchInFlight = errors.New("agent dispatch already in flight for task")

// ErrTestRunnerBusy is returned by the agent-start path when the per-machine
// test-runner concurrency cap (config.TestingMaxConcurrent) is saturated.
// Like ErrDispatchInFlight it is benign and transient: the run_agent step
// parks in ExecWaiting and ResumeStalled retries it once a testing slot frees,
// so it must never flip the task to human-required or write a status_reason.
var ErrTestRunnerBusy = errors.New("test-runner concurrency cap reached")

// ClassifyAgentStartError translates an agent-start error into a UI-safe
// status_reason and a "permanent" flag.
//
// permanent=true means retrying without human action will not help — the
// caller should flip the task to human-required and stop the resume loop
// from hammering it once a minute.
//
// Reason is a single line, capped at startReasonMaxLen. Empty err yields
// ("", false) so callers don't have to guard.
func ClassifyAgentStartError(err error) (reason string, permanent bool) {
	if err == nil {
		return "", false
	}
	switch {
	case errors.Is(err, ErrDispatchInFlight):
		// Transient and self-healing: another dispatcher holds the claim and
		// will start the agent. Suppress the reason entirely.
		return "", false
	case errors.Is(err, ErrTestRunnerBusy):
		// Transient: the testing slot frees and ResumeStalled retries. No reason.
		return "", false
	case errors.Is(err, worktreeerr.ErrAgentRunning):
		// Transient: PrepareForTask refused to rebase a worktree a tracked
		// agent is still live in. The agent's own completion (or a later
		// ResumeStalled tick once it's genuinely idle) drives the workflow
		// forward — no reason, no escalation.
		return "", false
	case errors.Is(err, project.ErrProjectNotRegistered):
		permanent = true
		reason = "agent start blocked: project not registered locally — create the project to resume"
	case errors.Is(err, worktreeerr.ErrRebaseFailed):
		permanent = true
		reason = worktreeerr.RebaseBlockedReason
	case errors.Is(err, provider.ErrProviderUnhealthy):
		reason = "agent start blocked: " + err.Error()
	default:
		reason = "agent start failed: " + err.Error()
	}
	return truncateReason(reason), permanent
}

func transientAgentStartError(err error) bool {
	return errors.Is(err, ErrDispatchInFlight) ||
		errors.Is(err, ErrTestRunnerBusy) ||
		errors.Is(err, worktreeerr.ErrAgentRunning) ||
		errors.Is(err, provider.ErrProviderUnhealthy)
}

// truncateReason caps a status_reason to startReasonMaxLen bytes with an
// ASCII ellipsis so the UI banner stays one line. Byte (not rune) bound so
// the caller can compare against len(reason) without surprises from
// multi-byte runes.
func truncateReason(s string) string {
	if len(s) <= startReasonMaxLen {
		return s
	}
	const tail = "..."
	return s[:startReasonMaxLen-len(tail)] + tail
}

// FormatStartFailure is a tiny helper for callers that want to log the same
// classified text. Keeps the log line and the on-task reason consistent.
func FormatStartFailure(taskID string, err error) string {
	reason, _ := ClassifyAgentStartError(err)
	return fmt.Sprintf("task %s: %s", taskID, reason)
}
