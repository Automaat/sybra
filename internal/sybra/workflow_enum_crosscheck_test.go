package sybra

import (
	"sort"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// TestWorkflowFieldAllowedValues_MatchEnumSources is the drift guard for
// the workflow.FieldAllowedValues registry. The workflow package can't
// import internal/task (that would cycle through task.Update's workflow
// field), so the registry is hand-maintained. This test — which runs in
// the main package and imports everything — asserts the hand-maintained
// set matches the authoritative enum sources exactly:
//
//   - task.status     ↔ task.AllStatuses()
//   - task.task_type  ↔ task.AllTaskTypes()
//   - pr.issue_kind   ↔ github.PRIssueConflict / PRIssueCIFailure / PRIssueReadyToMerge
//
// If a new Status or PR issue kind is added (or one is renamed, the shape
// of the original ci-failure bug), this test fails immediately, pointing
// at the specific drift rather than letting workflows silently stop
// matching at runtime.
func TestWorkflowFieldAllowedValues_MatchEnumSources(t *testing.T) {
	taskStatuses := make(map[string]bool, len(task.AllStatuses()))
	for _, s := range task.AllStatuses() {
		taskStatuses[string(s)] = true
	}
	taskTypes := make(map[string]bool, len(task.AllTaskTypes()))
	for _, tt := range task.AllTaskTypes() {
		taskTypes[string(tt)] = true
	}
	prKinds := map[string]bool{
		string(github.PRIssueConflict):     true,
		string(github.PRIssueCIFailure):    true,
		string(github.PRIssueReadyToMerge): true,
	}

	cases := []struct {
		field  string
		source map[string]bool
	}{
		{"task.status", taskStatuses},
		{"task.task_type", taskTypes},
		{"pr.issue_kind", prKinds},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			got, ok := workflow.FieldAllowedValues[tc.field]
			if !ok {
				t.Fatalf("workflow.FieldAllowedValues missing field %q", tc.field)
			}
			assertSameSet(t, tc.field, tc.source, got)
		})
	}
}

func assertSameSet(t *testing.T, label string, want, got map[string]bool) {
	t.Helper()
	missing := diffSet(want, got)
	extra := diffSet(got, want)
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("%s missing from workflow.FieldAllowedValues: %v", label, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s extra in workflow.FieldAllowedValues (not in enum source): %v", label, extra)
	}
}

func diffSet(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	return out
}
