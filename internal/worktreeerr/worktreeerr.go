// Package worktreeerr holds worktree sentinel errors that need to be
// classified from internal/workflow. It exists as a standalone leaf package
// (no internal/task or internal/workflow dependency) because internal/task
// already imports internal/workflow, and internal/worktree imports
// internal/task — importing internal/worktree directly from
// internal/workflow would close that cycle.
package worktreeerr

import "errors"

// ErrRebaseFailed indicates a reused task worktree could not be rebased onto
// the project base ref and must be repaired before another agent run.
var ErrRebaseFailed = errors.New("worktree rebase failed")

// ErrAgentRunning indicates PrepareForTask was asked to reuse (and rebase) a
// worktree that a tracked agent is still live in. Rebasing out from under a
// running agent corrupts its in-flight edits, so callers must treat this as
// a transient "retry once idle" condition (like workflow.ErrDispatchInFlight)
// rather than a real worktree conflict.
var ErrAgentRunning = errors.New("worktree busy: agent still running for task")

// RebaseBlockedReason is the human-facing status_reason written whenever
// ErrRebaseFailed escalates a task to human-required. Shared between
// internal/sybra's markRebaseBlocked and workflow.ClassifyAgentStartError so
// the two escalation paths can't drift apart in wording.
const RebaseBlockedReason = "branch stale: rebase failed before agent start; resolve conflicts or recreate the task branch"
