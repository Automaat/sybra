package sybra

import (
	"reflect"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/cleanup"
	"github.com/Automaat/sybra/internal/health"
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

func TestSnapshotOwnedProcessesIncludesServerAndAgentProcessGroups(t *testing.T) {
	t.Parallel()

	got := snapshotOwnedProcesses(10, []*agent.Agent{
		{PID: 21},
		nil,
		{PID: 0},
		{PID: 22},
	}, func(pid int) (int, error) {
		switch pid {
		case 10:
			return 100, nil
		case 21:
			return 210, nil
		case 22:
			return 0, nil
		default:
			return 0, nil
		}
	})

	want := health.OwnedProcesses{
		PIDs: map[int]bool{
			10: true,
			21: true,
			22: true,
		},
		ProcessGroups: map[int]bool{
			100: true,
			210: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshotOwnedProcesses() = %#v, want %#v", got, want)
	}
}
