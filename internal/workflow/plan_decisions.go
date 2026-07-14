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
	firstSubstantive := ""
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") ||
			strings.HasPrefix(strings.ToLower(trimmed), "question:") ||
			strings.HasPrefix(strings.ToLower(trimmed), "recommended:") ||
			strings.HasPrefix(strings.ToLower(trimmed), "options:") {
			return true
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if firstSubstantive == "" {
			firstSubstantive = trimmed
		}
	}
	return !strings.HasPrefix(strings.ToLower(firstSubstantive), "no open decisions")
}
