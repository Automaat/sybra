package workflow

import "strings"

// PlanHasOpenDecisions reports whether the planner's decision sidecar contains
// human choices that must be reviewed before implementation. The planner prompt
// requires an explicit "No open decisions" sentence when there are none; missing
// or ambiguous content is treated conservatively as requiring human review.
func PlanHasOpenDecisions(markdown string) bool {
	text := strings.TrimSpace(markdown)
	if text == "" {
		return true
	}
	if strings.Contains(strings.ToLower(text), "no open decisions") {
		return false
	}
	return true
}
