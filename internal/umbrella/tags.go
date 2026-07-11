package umbrella

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxParallelTagPrefix carries the planner's max-parallel value on the tracker
// task so the cap can be enforced without re-planning. Value is the integer
// suffix, e.g. "umbrella-max-parallel:5".
const MaxParallelTagPrefix = "umbrella-max-parallel:"

// FallbackTag marks an umbrella tracker whose DAG was built by
// independentFallback (the planner exhausted its retries) instead of the
// model, so a systematically-failing planner is board-visible rather than
// silently masked.
const FallbackTag = "umbrella-planner-fallback"

// MaxParallelTag renders the tracker tag encoding n.
func MaxParallelTag(n int) string {
	return MaxParallelTagPrefix + strconv.Itoa(n)
}

// ExpandFailTagPrefix carries a tracker's consecutive expansion-failure count,
// e.g. "umbrella-expand-fail:2". Persisted on the task so the count survives
// restarts and is shared by every caller of Expand (issue fetcher, manual
// stub enrichment, `sybra-cli umbrella`) instead of living in one process's
// memory.
const ExpandFailTagPrefix = "umbrella-expand-fail:"

// ExpandFailThreshold is how many consecutive Expand failures against an
// already-materialized tracker are tolerated before Expand stops calling the
// planner and parks the tracker human-required — see #1570.
const ExpandFailThreshold = 3

// ExpandFailTag renders the tracker tag encoding n consecutive failures.
func ExpandFailTag(n int) string {
	return ExpandFailTagPrefix + strconv.Itoa(n)
}

// ParseExpandFailCount reads the consecutive expansion-failure count from a
// tracker's tags, defaulting to 0 when absent or malformed.
func ParseExpandFailCount(tags []string) int {
	for _, t := range tags {
		if rest, ok := strings.CutPrefix(t, ExpandFailTagPrefix); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && n >= 0 {
				return n
			}
		}
	}
	return 0
}

// ParseMaxParallel reads the max-parallel value from a tracker's tags, falling
// back to DefaultMaxParallel when the tag is absent or malformed.
func ParseMaxParallel(tags []string) int {
	for _, t := range tags {
		if rest, ok := strings.CutPrefix(t, MaxParallelTagPrefix); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && n > 0 {
				return n
			}
		}
	}
	return DefaultMaxParallel
}

// HasMaxParallelTag reports whether tags already carry a MaxParallelTag,
// distinguishing "never set" from "set to DefaultMaxParallel" — ParseMaxParallel
// alone cannot tell those apart since both resolve to the same int.
func HasMaxParallelTag(tags []string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, MaxParallelTagPrefix) {
			return true
		}
	}
	return false
}

// RecoverFailTagPrefix carries a degraded tracker's consecutive recovery
// (re-plan) failure count, e.g. "umbrella-recover-fail:2". Distinct from
// ExpandFailTagPrefix: recovery only ever runs against an already-degraded
// tracker (one carrying FallbackTag), so it needs its own counter and its own
// backoff rather than sharing/resetting the expansion failure state.
const RecoverFailTagPrefix = "umbrella-recover-fail:"

// RecoverAfterTagPrefix carries the Unix-seconds timestamp before which
// recovery must not re-attempt a degraded tracker, e.g.
// "umbrella-recover-after:1735689600". Written on every recovery failure with
// an exponential backoff (see RecoverBackoff) so a systematically-unplannable
// umbrella is retried on a bounded schedule instead of every gate tick.
const RecoverAfterTagPrefix = "umbrella-recover-after:"

// RecoverExhaustedTag marks a degraded tracker that has failed recovery
// RecoverFailThreshold times in a row. App-level scheduling skips an
// exhausted tracker outright (no planner call) even though it still carries
// FallbackTag, so a permanently-unplannable umbrella stops burning planner
// calls and stays board-visible for a human to intervene.
const RecoverExhaustedTag = "umbrella-recover-exhausted"

// RecoverFailThreshold is how many consecutive recovery failures are
// tolerated before a degraded tracker is marked RecoverExhaustedTag.
const RecoverFailThreshold = 3

// recoverBackoffBase and recoverBackoffMax bound RecoverBackoff: 1 hour base,
// doubling per consecutive failure, capped at 24 hours so a systematically
// failing planner is retried at most once a day.
const (
	recoverBackoffBase = time.Hour
	recoverBackoffMax  = 24 * time.Hour
)

// RecoveryFailureReasonPrefix marks a tracker StatusReason as owned by the
// recovery failure-recording path (see formatRecoveryFailureReason), so
// callers (e.g. the app's eligibility filter, clearRecoveryState) can tell a
// recovery-authored reason apart from one set by unrelated tracker rollup.
const RecoveryFailureReasonPrefix = "umbrella recovery failed (attempt "

// RecoverFailTag renders the tracker tag encoding n consecutive recovery
// failures.
func RecoverFailTag(n int) string {
	return RecoverFailTagPrefix + strconv.Itoa(n)
}

// RecoverAfterTag renders the tracker tag encoding the Unix-seconds instant
// before which recovery must not retry.
func RecoverAfterTag(t time.Time) string {
	return RecoverAfterTagPrefix + strconv.FormatInt(t.Unix(), 10)
}

// ParseRecoverFailCount reads the consecutive recovery-failure count from a
// tracker's tags, defaulting to 0 when absent, malformed, or negative.
func ParseRecoverFailCount(tags []string) int {
	for _, t := range tags {
		if rest, ok := strings.CutPrefix(t, RecoverFailTagPrefix); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && n >= 0 {
				return n
			}
		}
	}
	return 0
}

// RecoverDue reports whether recovery is allowed to run against a tracker
// carrying tags, at instant now. Recovery is due when no RecoverAfterTag is
// present, the tag is malformed (fails closed to "due" rather than blocking a
// retry forever on a corrupt tag), or the encoded instant is not in the
// future.
func RecoverDue(tags []string, now time.Time) bool {
	for _, t := range tags {
		if rest, ok := strings.CutPrefix(t, RecoverAfterTagPrefix); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return true
			}
			return !now.Before(time.Unix(sec, 0))
		}
	}
	return true
}

// HasRecoverExhaustedTag reports whether tags already carry
// RecoverExhaustedTag.
func HasRecoverExhaustedTag(tags []string) bool {
	return slices.Contains(tags, RecoverExhaustedTag)
}

// RecoverBackoff returns the delay before the next recovery attempt is due,
// given the failure count after the attempt that just failed (1 for the
// first failure). Doubles per failure from a 1 hour base, capped at 24 hours.
func RecoverBackoff(failCount int) time.Duration {
	if failCount < 1 {
		failCount = 1
	}
	shift := failCount - 1
	const maxShift = 10 // 1h<<10 already exceeds the 24h cap; bounds the shift against overflow
	if shift > maxShift {
		shift = maxShift
	}
	d := recoverBackoffBase * time.Duration(1<<uint(shift))
	return min(d, recoverBackoffMax)
}

// ReplaceTagPrefix returns tags with every entry sharing prefix removed and
// newTag appended in its place (unless newTag is empty, which just deletes).
// Used to collapse any stale/duplicate tags sharing a prefix (e.g. a tracker
// that somehow accumulated more than one umbrella-max-parallel:* tag) down to
// exactly one canonical tag.
func ReplaceTagPrefix(tags []string, prefix, newTag string) []string {
	out := slices.DeleteFunc(slices.Clone(tags), func(t string) bool {
		return strings.HasPrefix(t, prefix)
	})
	if newTag != "" {
		out = append(out, newTag)
	}
	return out
}

// controlTags are Sybra-load-bearing tags a child task must not inherit from a
// GitHub sub-issue's labels — they route the task into other automations
// (reviews, handoff lanes, the gate itself, bug containment).
var controlTags = map[string]bool{
	"review":    true,
	"blocked":   true,
	"handoff":   true,
	"umbrella":  true,
	GatedTag:    true,
	"sybra-bug": true,
	"scrubbed":  true,
}

// InheritableLabels filters a sub-issue's GitHub labels down to those safe to
// copy onto a child task, dropping any tag that would mis-route it into another
// automation (exact control tags, or the handoff-/umbrella-/monitor: families).
func InheritableLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if controlTags[l] ||
			strings.HasPrefix(l, "handoff-") ||
			strings.HasPrefix(l, "umbrella-") ||
			strings.HasPrefix(l, "monitor:") {
			continue
		}
		out = append(out, l)
	}
	return out
}
