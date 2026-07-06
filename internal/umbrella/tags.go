package umbrella

import (
	"strconv"
	"strings"
)

// MaxParallelTagPrefix carries the planner's max-parallel value on the tracker
// task so the cap can be enforced without re-planning. Value is the integer
// suffix, e.g. "umbrella-max-parallel:5".
const MaxParallelTagPrefix = "umbrella-max-parallel:"

// FallbackTag marks an umbrella tracker whose DAG was built by
// linearChainFallback (the planner exhausted its retries) instead of the
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
