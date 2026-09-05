package taskdb

import "github.com/Automaat/sybra/internal/task"

// SidecarsFromTask extracts t's planning/review sidecar fields — everything
// task.MarshalStored's frontmatter deliberately excludes because the file
// backend keeps them in their own files — into the row shape PutBy/PutFnBy
// store them in. Empty fields are omitted rather than written as empty rows,
// matching the file backend's Read/List returning zero value for "absent"
// rather than a present-but-empty sidecar.
func SidecarsFromTask(t task.Task) []Sidecar {
	var out []Sidecar
	add := func(kind, content string) {
		if content != "" {
			out = append(out, Sidecar{Kind: kind, Content: content})
		}
	}
	add(SidecarPlan, t.Plan)
	add(SidecarPlanContract, t.PlanContract)
	add(SidecarPlanCritique, t.PlanCritique)
	add(SidecarPlanResearch, t.PlanResearch)
	add(SidecarPlanDecision, t.PlanDecisions)
	add(SidecarPlanBrief, t.PlanBrief)
	add(SidecarCodeReview, t.CodeReview)
	add(SidecarCurrentTestFailures, t.CurrentTestFailures)
	add(SidecarAcceptanceLedger, t.AcceptanceLedger)
	add(SidecarSpecDecision, t.SpecDecision)
	for name, content := range t.PlanDrafts {
		if content != "" {
			out = append(out, Sidecar{Kind: SidecarPlanDraft, Name: name, Content: content})
		}
	}
	return out
}

// ApplySidecars populates t's planning/review sidecar fields from sidecars,
// the inverse of SidecarsFromTask — how PutFnBy and a Get-style read hand a
// caller the same fully-populated Task the file backend's Store.Get does.
func ApplySidecars(t *task.Task, sidecars []Sidecar) {
	t.PlanDrafts = nil
	for _, sc := range sidecars {
		switch sc.Kind {
		case SidecarPlan:
			t.Plan = sc.Content
		case SidecarPlanContract:
			t.PlanContract = sc.Content
		case SidecarPlanCritique:
			t.PlanCritique = sc.Content
		case SidecarPlanResearch:
			t.PlanResearch = sc.Content
		case SidecarPlanDecision:
			t.PlanDecisions = sc.Content
		case SidecarPlanBrief:
			t.PlanBrief = sc.Content
		case SidecarCodeReview:
			t.CodeReview = sc.Content
		case SidecarCurrentTestFailures:
			t.CurrentTestFailures = sc.Content
		case SidecarAcceptanceLedger:
			t.AcceptanceLedger = sc.Content
		case SidecarSpecDecision:
			t.SpecDecision = sc.Content
		case SidecarPlanDraft:
			if t.PlanDrafts == nil {
				t.PlanDrafts = make(map[string]string)
			}
			t.PlanDrafts[sc.Name] = sc.Content
		}
	}
}
