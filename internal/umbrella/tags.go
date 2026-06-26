package umbrella

import (
	"strconv"
	"strings"
)

// MaxParallelTagPrefix carries the planner's max-parallel value on the tracker
// task so the cap can be enforced without re-planning. Value is the integer
// suffix, e.g. "umbrella-max-parallel:5".
const MaxParallelTagPrefix = "umbrella-max-parallel:"

// MaxParallelTag renders the tracker tag encoding n.
func MaxParallelTag(n int) string {
	return MaxParallelTagPrefix + strconv.Itoa(n)
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
