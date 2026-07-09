package workflow

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

// startReasonMaxLen caps the human-facing reason written to task.StatusReason.
// Longer messages clutter the UI and rarely add value beyond the lead error.
const startReasonMaxLen = 200

const transientFetchStatusReason = "agent start delayed: transient network failure reconciling worktree with remote"

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

// ErrAgentPoolBusy is returned by the agent-start path when the global agent
// pool (agent.MaxConcurrent) is already saturated. The sybra adapter maps
// agent.ErrMaxConcurrentReached onto this sentinel. Like ErrTestRunnerBusy it
// is benign and transient — a slot frees when any running agent completes — so
// the run_agent step parks in ExecWaiting and ResumeStalled retries it. It must
// never feed the circuit breaker or flip the task to human-required: a
// transient "too many agents running" is not a dispatch fault.
var ErrAgentPoolBusy = errors.New("agent pool concurrency cap reached")

// ErrNoProjectAssigned is returned by an agent-start path when a task needs
// an isolated worktree but has no project_id, and auto-assignment could not
// resolve one (no agent.default_project_id configured, and more than one
// project is registered — see agentorch.Orchestrator.AutoAssignProject).
// This is a permanent, structurally-guaranteed failure: nothing about
// retrying the dispatch changes the outcome, so ClassifyAgentStartError
// escalates it to human-required on the first attempt instead of burning
// the circuit breaker's retry budget on identical failures.
var ErrNoProjectAssigned = errors.New("no project_id: refusing to start agent without isolated worktree")

// ErrTaskCostExceeded is returned by an agent-start path when a task's
// cumulative AgentRuns cost already meets or exceeds agent.max_task_cost_usd.
// Unlike the per-run MaxCostUSD guardrail (which resets every attempt), this
// caps total spend across every retry/dispatch a task has ever had — a
// permanent failure until a human raises the cap or clears the task's spend,
// so ClassifyAgentStartError escalates to human-required on the first hit
// instead of burning the circuit breaker's retry budget on identical checks.
var ErrTaskCostExceeded = errors.New("task cumulative cost exceeds agent.max_task_cost_usd")

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
	case errors.Is(err, ErrAgentPoolBusy):
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
	case errors.Is(err, ErrNoProjectAssigned):
		permanent = true
		reason = "agent start blocked: no project could be assigned — set agent.default_project_id in config or assign a project to this task manually to resume"
	case errors.Is(err, ErrTaskCostExceeded):
		permanent = true
		reason = "agent start blocked: " + err.Error()
	case errors.Is(err, worktreeerr.ErrRebaseFailed):
		permanent = true
		reason = worktreeerr.RebaseBlockedReason
	case errors.Is(err, worktreeerr.ErrTransientFetch):
		// Transient: a network blip during the remote fetch/ls-remote, not a
		// genuine content conflict. Never escalate — let the resume loop retry
		// once connectivity recovers.
		reason = transientFetchStatusReason
	case errors.Is(err, provider.ErrProviderUnhealthy):
		reason = "agent start blocked: " + err.Error()
	default:
		reason = "agent start failed: " + err.Error()
	}
	return truncateReason(reason), permanent
}

// isTransientCapacityError reports whether err is a provider-capacity throttle
// (rate limit) that self-heals once the provider's cooldown window expires.
// Such errors must not be counted toward the circuit breaker or escalated to
// human-required — the task should park and retry when a provider frees up.
// Both the rate-limit reason and a cooldown deadline (Until) are accepted
// because resolveProviderDecision tags the reason but leaves Until zero, while
// other gate paths may carry only the deadline. A logged-out auth failure has
// neither, so it stays escalatable — a human must log in.
func isTransientCapacityError(err error) bool {
	var ue *provider.UnhealthyError
	if errors.As(err, &ue) {
		return ue.Reason == provider.RateLimitReason || !ue.Until.IsZero()
	}
	return false
}

func transientAgentStartError(err error) bool {
	return errors.Is(err, ErrDispatchInFlight) ||
		errors.Is(err, ErrTestRunnerBusy) ||
		errors.Is(err, ErrAgentPoolBusy) ||
		errors.Is(err, worktreeerr.ErrTransientFetch) ||
		errors.Is(err, worktreeerr.ErrAgentRunning) ||
		errors.Is(err, provider.ErrProviderUnhealthy)
}

func isTransientFetchReason(reason string) bool {
	return strings.TrimSpace(reason) == transientFetchStatusReason
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
