package artifact

import (
	"fmt"
	"strings"
)

// ManualDecisionMessage renders a durable, human-readable decision log entry
// for an operator-authored status transition. The progress kind carries the
// machine-readable "decision" classification; this string preserves the
// specific transition and rationale even after status_reason changes again.
func ManualDecisionMessage(from, to, reason string) string {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	reason = strings.TrimSpace(reason)

	msg := fmt.Sprintf("Operator decision: moved task from %s to %s", from, to)
	if reason != "" {
		msg += " — " + reason
	}
	return msg
}
