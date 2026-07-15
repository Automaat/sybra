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

// ChildSpecs computes the child tasks to create for an umbrella expansion from
// a resolved, validated plan. It:
//   - skips a sub-issue that already has a task (idempotent re-expansion),
//   - skips a closed sub-issue (the work is done — no child),
//   - drops a dependency that points at a closed sub-issue (already satisfied,
//     and it would otherwise have no task to resolve against and block forever).
//
// existingRefs keys are normalized issue refs (NormalizeIssueRef). Plan child
// refs must already be canonical (see Plan.resolve).
func ChildSpecs(plan Plan, subs []SubIssue, existingRefs map[string]bool) []ChildSpec {
	byRef := make(map[string]SubIssue, len(subs))
	closed := make(map[string]bool, len(subs))
	for _, s := range subs {
		key := NormalizeIssueRef(s.Ref)
		byRef[key] = s
		if s.Closed {
			closed[key] = true
		}
	}

	var specs []ChildSpec
	for i := range plan.Children {
		c := &plan.Children[i]
		key := NormalizeIssueRef(c.Ref)
		if existingRefs[key] || closed[key] {
			continue
		}
		s, ok := byRef[key]
		if !ok {
			continue
		}
		deps := make([]string, 0, len(c.DependsOn))
		for _, d := range c.DependsOn {
			if !closed[NormalizeIssueRef(d)] {
				deps = append(deps, d)
			}
		}
		specs = append(specs, ChildSpec{
			Title:     s.Title,
			Body:      s.Body,
			Issue:     s.Ref,
			DependsOn: deps,
			Track:     c.Track,
		})
	}
	return specs
}
