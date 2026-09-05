package intervention

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func TestFromUnblock_ClassificationIsCallerSupplied(t *testing.T) {
	cur := task.Task{ID: "task-a", Status: task.StatusHumanRequired, StatusReason: "no project assigned"}
	proj := project.Project{ID: "owner/repo"}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	human := FromUnblock(cur, proj, "todo", "assigned project manually", OperatorActionHuman, now)
	if human.OperatorActionClass != OperatorActionHuman {
		t.Fatalf("OperatorActionClass = %q, want %q", human.OperatorActionClass, OperatorActionHuman)
	}

	auto := FromUnblock(cur, proj, "todo", "auto-recovered", OperatorActionAutoRecovery, now)
	if auto.OperatorActionClass != OperatorActionAutoRecovery {
		t.Fatalf("OperatorActionClass = %q, want %q", auto.OperatorActionClass, OperatorActionAutoRecovery)
	}

	if human.Fingerprint == auto.Fingerprint {
		t.Fatalf("human and auto_recovery fingerprints must differ: got %q for both", human.Fingerprint)
	}
}

func TestFromUnblock_SetsSystemStateFields(t *testing.T) {
	cur := task.Task{
		ID:           "task-a",
		Status:       task.StatusHumanRequired,
		StatusReason: "disk space exhausted",
		AgentRuns: []task.AgentRun{
			{Role: ""},
			{Role: "pr-fix"},
			{Role: "pr-fix"}, // duplicate role must not appear twice
		},
	}
	proj := project.Project{ID: "owner/repo"}
	rec := FromUnblock(cur, proj, "in-progress", "  repaired worktree  ", OperatorActionHuman, time.Now())

	if rec.FromStatus != string(task.StatusHumanRequired) || rec.ToStatus != "in-progress" {
		t.Fatalf("FromStatus/ToStatus = %q/%q, want %q/%q", rec.FromStatus, rec.ToStatus, task.StatusHumanRequired, "in-progress")
	}
	if rec.OperatorReason != "repaired worktree" {
		t.Fatalf("OperatorReason = %q, want trimmed", rec.OperatorReason)
	}
	wantActions := []string{"pr-fix", "disk space exhausted"}
	if len(rec.AttemptedActions) != len(wantActions) {
		t.Fatalf("AttemptedActions = %v, want %v", rec.AttemptedActions, wantActions)
	}
	for i, want := range wantActions {
		if rec.AttemptedActions[i] != want {
			t.Fatalf("AttemptedActions[%d] = %q, want %q", i, rec.AttemptedActions[i], want)
		}
	}
	if rec.ReplayStatus != ReplayStatusUnsupportedSimulation {
		t.Fatalf("ReplayStatus = %q, want %q", rec.ReplayStatus, ReplayStatusUnsupportedSimulation)
	}
	if rec.Fingerprint == "" {
		t.Fatal("Fingerprint must be set")
	}
}

func TestProjectKey_PublicVsWork(t *testing.T) {
	pet := project.Project{ID: "owner/repo", Type: project.ProjectTypePet}
	if got := ProjectKey(pet); got != "owner/repo" {
		t.Fatalf("ProjectKey(pet) = %q, want %q", got, "owner/repo")
	}

	work := project.Project{ID: "acme/api", Type: project.ProjectTypeWork, Owner: "acme", Repo: "api"}
	got := ProjectKey(work)
	if got == work.ID {
		t.Fatalf("ProjectKey(work) = %q, must not equal the plain project ID", got)
	}
	if got != ProjectKey(work) {
		t.Fatal("ProjectKey(work) must be deterministic")
	}
}

func TestWorkRecordID_OpaqueAndDeterministic(t *testing.T) {
	id := WorkRecordID("task-a")
	if id == "task-a" {
		t.Fatal("WorkRecordID must not return the plain task ID")
	}
	if id != WorkRecordID("task-a") {
		t.Fatal("WorkRecordID must be deterministic")
	}
	if id == WorkRecordID("task-b") {
		t.Fatal("WorkRecordID must differ for different task IDs")
	}
}
