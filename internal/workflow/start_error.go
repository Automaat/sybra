package workflow

import (
	"errors"
	"fmt"

	"github.com/Automaat/sybra/internal/project"
)

// startReasonMaxLen caps the human-facing reason written to task.StatusReason.
// Longer messages clutter the UI and rarely add value beyond the lead error.
const startReasonMaxLen = 200

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
	case errors.Is(err, project.ErrProjectNotRegistered):
		permanent = true
		reason = "agent start blocked: project not registered locally — create the project to resume"
	default:
		reason = "agent start failed: " + err.Error()
	}
	return truncateReason(reason), permanent
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
