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

// RebaseBlockedReason is the human-facing status_reason written whenever
// ErrRebaseFailed escalates a task to human-required. Shared between
// internal/sybra's markRebaseBlocked and workflow.ClassifyAgentStartError so
// the two escalation paths can't drift apart in wording.
const RebaseBlockedReason = "branch stale: rebase failed before agent start; resolve conflicts or recreate the task branch"

// ErrTransientFetch indicates a reused task worktree's remote reconcile step
// failed for a transient network/transport reason (e.g. SSH connection
// refused, DNS resolution failure, timeout) rather than a genuine content
// conflict. Unlike ErrRebaseFailed, this must never escalate a task to
// human-required — callers should treat it like any other transient
// agent-start failure and let the normal resume/retry loop pick it back up
// once connectivity recovers.
var ErrTransientFetch = errors.New("worktree remote fetch failed transiently")
