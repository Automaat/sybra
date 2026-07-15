package selfmonitor

import (
	"os"
	"sort"
	"testing"

	"github.com/Automaat/sybra/internal/health"
	"github.com/Automaat/sybra/internal/task"
)

// makeInv builds a minimal InvestigatedFinding for correlator tests.
func makeInv(fp string, cat health.Category, taskID string, ls *LogSummary) InvestigatedFinding {
	return InvestigatedFinding{
		Fingerprint: fp,
		Finding: health.Finding{
			Category: cat,
			TaskID:   taskID,
		},
		LogSummary: ls,
		Verdict:    Verdict{Classification: VerdictPending},
	}
}

// errorClassLS returns a LogSummary whose dominant error class is cls.
func errorClassLS(cls string) *LogSummary {
	return &LogSummary{
		ErrorClasses: []ErrorClass{{Class: cls, Count: 3}},
	}
}

func TestCorrelate_ReturnsNilForFewerThanTwoFindings(t *testing.T) {
	got := Correlate([]InvestigatedFinding{makeInv("fp1", health.CatCostOutlier, "t1", nil)}, nil)
	if got != nil {
		t.Errorf("Correlate(1 finding) = %v, want nil", got)
	}
}

func TestCorrelate_SameProject(t *testing.T) {
	taskStore := map[string]task.Task{
		"t1": {ID: "t1", ProjectID: "owner/repo"},
		"t2": {ID: "t2", ProjectID: "owner/repo"},
	}
	getTask := func(id string) (task.Task, error) {
		t, ok := taskStore[id]
		if !ok {
			return task.Task{}, os.ErrNotExist
		}
		return t, nil
	}

	findings := []InvestigatedFinding{
		makeInv("fp1", health.CatCostOutlier, "t1", nil),
		makeInv("fp2", health.CatStuckTask, "t2", nil),
	}

	cors := Correlate(findings, getTask)

	var sameProj []Correlation
	for _, c := range cors {
		if c.Kind == "same_project" {
			sameProj = append(sameProj, c)
		}
	}
	if len(sameProj) != 1 {
		t.Fatalf("same_project correlations = %d, want 1", len(sameProj))
	}
	c := sameProj[0]
	if c.Key != "owner/repo" {
		t.Errorf("Key = %q, want owner/repo", c.Key)
	}
	if c.Count != 2 {
		t.Errorf("Count = %d, want 2", c.Count)
	}
	fps := append([]string(nil), c.Fingerprints...)
	sort.Strings(fps)
	if fps[0] != "fp1" || fps[1] != "fp2" {
		t.Errorf("Fingerprints = %v, want [fp1 fp2]", fps)
	}
}

func TestCorrelate_SameProject_SkipsWhenGetTaskNil(t *testing.T) {
	findings := []InvestigatedFinding{
		makeInv("fp1", health.CatCostOutlier, "t1", nil),
		makeInv("fp2", health.CatStuckTask, "t2", nil),
	}
	cors := Correlate(findings, nil)
	for _, c := range cors {
		if c.Kind == "same_project" {
			t.Errorf("same_project emitted with nil getTask: %+v", c)
		}
	}
}

func TestCorrelate_SameProject_SkipsSingleFindingPerProject(t *testing.T) {
	taskStore := map[string]task.Task{
		"t1": {ID: "t1", ProjectID: "owner/a"},
		"t2": {ID: "t2", ProjectID: "owner/b"},
	}
	getTask := func(id string) (task.Task, error) {
		t, ok := taskStore[id]
		if !ok {
			return task.Task{}, os.ErrNotExist
		}
		return t, nil
	}
	findings := []InvestigatedFinding{
		makeInv("fp1", health.CatCostOutlier, "t1", nil),
		makeInv("fp2", health.CatStuckTask, "t2", nil),
	}
	cors := Correlate(findings, getTask)
	for _, c := range cors {
		if c.Kind == "same_project" {
			t.Errorf("same_project emitted for different projects: %+v", c)
		}
	}
}

func TestCorrelate_SameErrorClass(t *testing.T) {
	findings := []InvestigatedFinding{
		makeInv("fp1", health.CatCostOutlier, "t1", errorClassLS("permission_denied")),
		makeInv("fp2", health.CatAgentRetryLoop, "t2", errorClassLS("permission_denied")),
		makeInv("fp3", health.CatStuckTask, "t3", errorClassLS("auth_error")),
	}
	cors := Correlate(findings, nil)

	var sameErr []Correlation
	for _, c := range cors {
		if c.Kind == "same_error_class" {
			sameErr = append(sameErr, c)
		}
	}
	if len(sameErr) != 1 {
		t.Fatalf("same_error_class = %d, want 1", len(sameErr))
	}
	if sameErr[0].Key != "permission_denied" {
		t.Errorf("Key = %q, want permission_denied", sameErr[0].Key)
	}
	if sameErr[0].Count != 2 {
		t.Errorf("Count = %d, want 2", sameErr[0].Count)
	}
}

func TestCorrelate_SameErrorClass_IgnoresToolErrorAndUnknown(t *testing.T) {
	tests := []struct {
		name string
		cls  string
	}{
		{"tool_error ignored", "tool_error"},
		{"unknown ignored", "unknown"},
		{"empty ignored", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := []InvestigatedFinding{
				makeInv("fp1", health.CatCostOutlier, "t1", errorClassLS(tt.cls)),
				makeInv("fp2", health.CatStuckTask, "t2", errorClassLS(tt.cls)),
			}
			cors := Correlate(findings, nil)
			for _, c := range cors {
				if c.Kind == "same_error_class" {
					t.Errorf("%s: same_error_class emitted for class %q: %+v", tt.name, tt.cls, c)
				}
			}
		})
	}
}

func TestCorrelate_Cascade(t *testing.T) {
	findings := []InvestigatedFinding{
		makeInv("fp-retry", health.CatAgentRetryLoop, "task-shared", nil),
		makeInv("fp-stuck", health.CatStuckTask, "task-shared", nil),
	}
	cors := Correlate(findings, nil)

	var cascades []Correlation
	for _, c := range cors {
		if c.Kind == "cascade" {
			cascades = append(cascades, c)
		}
	}
	if len(cascades) != 1 {
		t.Fatalf("cascade = %d, want 1", len(cascades))
	}
	c := cascades[0]
	if c.Key != "task-shared" {
		t.Errorf("Key = %q, want task-shared", c.Key)
	}
	if c.Count != 2 {
		t.Errorf("Count = %d, want 2", c.Count)
	}
	fps := append([]string(nil), c.Fingerprints...)
	sort.Strings(fps)
	if fps[0] != "fp-retry" || fps[1] != "fp-stuck" {
		t.Errorf("Fingerprints = %v, want [fp-retry fp-stuck]", fps)
	}
}

func TestCorrelate_Cascade_NoMatchWhenDifferentTaskIDs(t *testing.T) {
	findings := []InvestigatedFinding{
		makeInv("fp-retry", health.CatAgentRetryLoop, "task-a", nil),
		makeInv("fp-stuck", health.CatStuckTask, "task-b", nil),
	}
	cors := Correlate(findings, nil)
	for _, c := range cors {
		if c.Kind == "cascade" {
			t.Errorf("cascade emitted for different task IDs: %+v", c)
		}
	}
}

func TestCorrelate_DeterministicOrder(t *testing.T) {
	taskStore := map[string]task.Task{
		"t1": {ID: "t1", ProjectID: "owner/b"},
		"t2": {ID: "t2", ProjectID: "owner/a"},
	}
	getTask := func(id string) (task.Task, error) {
		t, ok := taskStore[id]
		if !ok {
			return task.Task{}, os.ErrNotExist
		}
		return t, nil
	}

	findings := []InvestigatedFinding{
		makeInv("fp3", health.CatAgentRetryLoop, "task-z", errorClassLS("permission_denied")),
		makeInv("fp2", health.CatStuckTask, "task-z", errorClassLS("permission_denied")),
		makeInv("fp1", health.CatCostOutlier, "t1", errorClassLS("auth_error")),
		makeInv("fp4", health.CatStuckTask, "t2", errorClassLS("auth_error")),
	}

	cors := Correlate(findings, getTask)
	if len(cors) != 3 {
		t.Fatalf("correlations = %d, want 3", len(cors))
	}

	got := make([]string, 0, len(cors))
	for _, c := range cors {
		got = append(got, c.Kind+":"+c.Key)
	}
	want := []string{
		"same_error_class:auth_error",
		"same_error_class:permission_denied",
		"cascade:task-z",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}
