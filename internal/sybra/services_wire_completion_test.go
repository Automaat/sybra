package sybra

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestPostRunReconciliationSkipsDegradedApp(t *testing.T) {
	t.Parallel()
	if got := (*App)(nil).postRunReconciliation(); got != nil {
		t.Fatalf("nil app reconciler = %v, want nil", got)
	}
	if got := (&App{}).postRunReconciliation(); got != nil {
		t.Fatalf("app without task manager reconciler = %v, want nil", got)
	}
}

func TestReconciliationAuditIdentityFailsClosedWithoutProjectStore(t *testing.T) {
	t.Parallel()
	store, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	projectID := "missing/project"
	created, err := tasks.CreateFull("scoped", "", task.AgentModeHeadless, task.Update{ProjectID: &projectID})
	if err != nil {
		t.Fatal(err)
	}

	app := &App{tasks: tasks}
	taskID, scope, confidential := app.reconciliationAuditIdentity(created.ID)
	if !confidential || scope != "work-unknown" {
		t.Fatalf("identity = (%q, %q, %v), want fail-closed work identity", taskID, scope, confidential)
	}
	if taskID == created.ID || taskID == "" {
		t.Fatalf("task ID was not pseudonymized: %q", taskID)
	}
}
