package umbrella

// ChildSpec is one child task an umbrella expansion will create: its source
// sub-issue content plus the dependency edges the planner assigned.
type ChildSpec struct {
	Title     string
	Body      string
	Issue     string // canonical issue ref/URL this child implements
	DependsOn []string
	Track     string
}

// ChildSpecs computes the child tasks to create for an umbrella expansion. It
// is idempotent: a sub-issue whose ref is already in existingRefs (a task
// exists for it) is skipped, so re-expanding an umbrella only materializes
// newly added sub-issues. existingRefs keys are normalized issue refs
// (NormalizeIssueRef). Plan children must already be validated against subs.
func ChildSpecs(plan Plan, subs []SubIssue, existingRefs map[string]bool) []ChildSpec {
	byRef := make(map[string]SubIssue, len(subs))
	for _, s := range subs {
		byRef[NormalizeIssueRef(s.Ref)] = s
	}
	var specs []ChildSpec
	for _, c := range plan.Children {
		key := NormalizeIssueRef(c.Ref)
		if existingRefs[key] {
			continue
		}
		s, ok := byRef[key]
		if !ok {
			continue
		}
		specs = append(specs, ChildSpec{
			Title:     s.Title,
			Body:      s.Body,
			Issue:     s.Ref,
			DependsOn: c.DependsOn,
			Track:     c.Track,
		})
	}
	return specs
}
