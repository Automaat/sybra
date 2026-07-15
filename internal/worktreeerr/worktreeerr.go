// Package worktreeerr holds worktree sentinel errors that need to be
// classified from internal/workflow. It exists as a standalone leaf package
// (no internal/task or internal/workflow dependency) because internal/task
// already imports internal/workflow, and internal/worktree imports
// internal/task — importing internal/worktree directly from
// internal/workflow would close that cycle.
package worktreeerr

import (
	"errors"
	"strings"
)

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

// ErrTransientFetch indicates a reused task worktree's remote reconcile step
// failed for a transient network/transport reason (e.g. SSH connection
// refused, DNS resolution failure, timeout) rather than a genuine content
// conflict. Unlike ErrRebaseFailed, this must never escalate a task to
// human-required — callers should treat it like any other transient
// agent-start failure and let the normal resume/retry loop pick it back up
// once connectivity recovers.
var ErrTransientFetch = errors.New("worktree remote fetch failed transiently")

// diskSpaceErrorMarkers are substrings of git/OS I/O failures that indicate
// the host ran out of disk space (ENOSPC) rather than a genuine content
// conflict. Narrow, lower-cased substring match mirrors
// project.IsTransientNetworkError's approach — git/filesystem failures
// surface here as plain-text subprocess stderr, not typed syscall errors, so
// there is no *os.PathError/syscall.ENOSPC to unwrap by the time an error
// reaches this package.
var diskSpaceErrorMarkers = []string{
	"no space left on device",
	"disk quota exceeded",
}

// IsDiskSpaceError reports whether err (or anything wrapped inside it) looks
// like a disk-full (ENOSPC) failure from a git or filesystem operation, as
// opposed to a genuine content conflict. A retry loop that treats an
// ENOSPC-caused git failure as an ordinary content conflict keeps hammering a
// full disk — worsening it — and can leave worktree state corrupted enough
// that a LATER retry fails with a derived symptom (e.g. "branch diverged")
// that no longer mentions disk space at all. Callers should check this before
// falling back to a generic/derived reason so the human-facing status_reason
// names the actual root cause while the evidence is still in the error.
func IsDiskSpaceError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range diskSpaceErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// DiskSpaceExhaustedReason is the human-facing status_reason written when an
// agent-start failure is traced to the host running out of disk space,
// instead of the misleading RebaseBlockedReason a downstream git symptom
// (rebase/reconcile failure) would otherwise produce.
const DiskSpaceExhaustedReason = "agent start blocked: host disk space exhausted (no space left on device) — free disk space before this task can proceed"
