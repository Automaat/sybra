package sybra

import (
	"reflect"
	"testing"

	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/task"
)

func TestLoadProtectedEvidenceTasksDoesNotScanUnrelatedHistory(t *testing.T) {
	findings := []cleanup.Finding{
		{TaskID: "active", State: cleanup.FindingOpen},
		{TaskID: "original", LinkedTaskID: "linked", State: cleanup.FindingReattached},
		{TaskID: "active", State: cleanup.FindingOpen},
		{TaskID: "closed", State: cleanup.FindingResolved},
		{TaskID: "discarded", State: cleanup.FindingDiscarded},
	}
	want := map[string]task.Task{
		"active": {ID: "active"},
		"linked": {ID: "linked"},
	}
	var requested []string
	got := loadProtectedEvidenceTasks(findings, func(id string) (task.Task, error) {
		requested = append(requested, id)
		return want[id], nil
	})
	if !reflect.DeepEqual(requested, []string{"active", "linked"}) {
		t.Fatalf("Get calls = %v, want only evidence task ids", requested)
	}
	if !reflect.DeepEqual(got, []task.Task{want["active"], want["linked"]}) {
		t.Fatalf("tasks = %+v, want referenced evidence tasks", got)
	}
}
