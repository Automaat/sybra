package umbrella

import (
	"regexp"
	"strings"
)

// crossProgramAfterRe matches a free-text blocking precondition a sub-issue
// body names in prose — "after #N", "strictly after #N", or the qualified
// "owner/repo#N" form — e.g. "strictly after #2464". The planner's DependsOn
// can never carry this: buildPlanSchema constrains dependsOn entries to the
// enum of the current umbrella's own sub-issue refs, so a reference to a
// different program's issue is structurally unrepresentable there. Put the
// qualified alternative first so it wins over the bare "#N" form when both
// would otherwise match the same position.
var crossProgramAfterRe = regexp.MustCompile(`(?i)\b(?:strictly\s+)?after\s+([A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+#\d+|#\d+)`)

// ExternalBlockers extracts issue refs named as free-text blocking
// preconditions in body (see crossProgramAfterRe), canonicalized via
// NormalizeIssueRef. selfRef (the referencing task's own issue ref) is
// excluded so a sub-issue that mentions its own number in prose never gates
// on itself — a bare "#N" excludes on number alone (GitHub's own convention
// reads an unqualified "#N" as same-repo), since selfRef's repo is otherwise
// unknowable from the bare form. Order-preserving, de-duplicated.
func ExternalBlockers(body, selfRef string) []string {
	self := NormalizeIssueRef(selfRef)
	selfNum := numberOf(self)
	var out []string
	seen := make(map[string]bool)
	for _, m := range crossProgramAfterRe.FindAllStringSubmatch(body, -1) {
		ref := NormalizeIssueRef(m[1])
		if ref == "" || ref == self || seen[ref] {
			continue
		}
		if !strings.Contains(ref, "/") && selfNum != "" && numberOf(ref) == selfNum {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}
