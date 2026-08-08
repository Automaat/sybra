package triage

import (
	"strings"

	"github.com/Automaat/sybra/internal/textutil"
)

// RetryableStatusReasonPrefix marks a classifier failure as re-runnable. The
// workflow engine's triage retry-coercion path matches on it
// (internal/workflow/engine_advance.go), so a task carrying it is dispatched
// again instead of parked for a human.
const RetryableStatusReasonPrefix = "triage retryable: "

// RetryableStatusReason renders a classifier failure into that marker. Every
// caller that runs the classifier has to stamp it — the CLI against a local
// board and the server on the CLI's behalf both do, or the same failure parks
// a task on one path and retries it on the other.
func RetryableStatusReason(err error) string {
	detail := ""
	if err != nil {
		detail = strings.TrimSpace(err.Error())
	}
	if detail == "" {
		detail = "unknown classifier failure"
	}
	detail = strings.Join(strings.Fields(detail), " ")
	const maxDetailLen = 500
	detail = textutil.TruncateBytes(detail, maxDetailLen, "...")
	return RetryableStatusReasonPrefix + "classifier failed: " + detail
}
