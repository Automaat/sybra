package task

import "slices"

// Flag is a well-known, load-bearing tag: unlike the free-form entries in
// Task.Tags, a Flag is read back by name across package boundaries (triage,
// umbrella, the human-review filer, the monitor router) to make routing
// decisions — a sybra-bug tracker task, one whose body was scrubbed before
// filing, one filed locally instead of on GitHub, one whose GitHub filing
// attempt failed. Before this type existed each package re-declared its own
// copy of these strings, so a rename in one place silently desynced the
// others. Centralizing them here gives every caller a compile-time-checked
// name instead of an independently-hardcoded literal.
type Flag string

const (
	// FlagSybraBug marks a task as Sybra's own bug tracker entry, as opposed
	// to a task for the project under review.
	FlagSybraBug Flag = "sybra-bug"
	// FlagScrubbed marks a sybra-bug task whose title/body was redacted via
	// internal/scrub before it was filed locally instead of on GitHub, because
	// its originating task belonged to a work-typed project (see the
	// Work-Data Confidentiality rule in CLAUDE.md).
	FlagScrubbed Flag = "scrubbed"
	// FlagLocal marks a sybra-bug task that was filed as a local task instead
	// of a GitHub issue by explicit configuration
	// (human_review.sybra_bug_action: local_task), as opposed to FlagScrubbed
	// routing (forced local by work-project confidentiality).
	FlagLocal Flag = "local"
	// FlagIssueFilingFailed marks a sybra-bug task whose GitHub issue filing
	// attempt failed, so the task itself is the durable record until a human
	// retries.
	FlagIssueFilingFailed Flag = "issue-filing-failed"
)

// HasFlag reports whether tags carries f.
func HasFlag(tags []string, f Flag) bool {
	return slices.Contains(tags, string(f))
}
