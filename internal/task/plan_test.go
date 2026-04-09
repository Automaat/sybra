package task

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanStoreReadNonexistent(t *testing.T) {
	t.Parallel()
	ps := NewPlanStore(t.TempDir())

	content, err := ps.Read("nonexistent")
	if err != nil {
		t.Fatalf("expected no error for missing plan, got %v", err)
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

func TestPlanStoreWriteRead(t *testing.T) {
	t.Parallel()
	ps := NewPlanStore(t.TempDir())

	want := "## Plan\n\nDo the thing."
	if err := ps.Write("task-abc", want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := ps.Read("task-abc")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPlanStoreDeleteExisting(t *testing.T) {
	t.Parallel()
	ps := NewPlanStore(t.TempDir())

	if err := ps.Write("task-del", "some plan"); err != nil {
		t.Fatal(err)
	}
	if err := ps.Delete("task-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	content, err := ps.Read("task-del")
	if err != nil {
		t.Fatalf("Read after delete: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty after delete, got %q", content)
	}
}

func TestPlanStoreDeleteNonexistent(t *testing.T) {
	t.Parallel()
	ps := NewPlanStore(t.TempDir())

	if err := ps.Delete("nope"); err != nil {
		t.Fatalf("Delete nonexistent should not error, got %v", err)
	}
}

func TestPlanStoreWriteEmptyClears(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ps := NewPlanStore(dir)

	if err := ps.Write("task-clear", "initial plan"); err != nil {
		t.Fatal(err)
	}
	if err := ps.Write("task-clear", ""); err != nil {
		t.Fatalf("Write empty: %v", err)
	}

	sidecar := filepath.Join(dir, "task-clear.plan.md")
	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Error("sidecar file should not exist after writing empty string")
	}
}

func TestStoreUpdateWritesPlanSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Plan task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	plan := "## Approach\n\nDo the thing step by step."
	updated, err := store.Update(created.ID, map[string]any{"plan": plan})
	if err != nil {
		t.Fatalf("Update with plan: %v", err)
	}
	if updated.Plan != plan {
		t.Errorf("Plan = %q, want %q", updated.Plan, plan)
	}

	// Verify sidecar exists and task file did not change
	sidecar := filepath.Join(dir, created.ID+".plan.md")
	data, readErr := os.ReadFile(sidecar)
	if readErr != nil {
		t.Fatalf("sidecar not written: %v", readErr)
	}
	if string(data) != plan {
		t.Errorf("sidecar = %q, want %q", string(data), plan)
	}
}

func TestStoreGetPopulatesPlan(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Plan get task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	plan := "## Plan\n\nstep 1\nstep 2"
	if _, err := store.Update(created.ID, map[string]any{"plan": plan}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != plan {
		t.Errorf("Get Plan = %q, want %q", got.Plan, plan)
	}
}

func TestStoreDeleteCascadesPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Cascade delete", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(created.ID, map[string]any{"plan": "## Plan\n\nsteps"}); err != nil {
		t.Fatal(err)
	}

	sidecar := filepath.Join(dir, created.ID+".plan.md")
	if _, statErr := os.Stat(sidecar); os.IsNotExist(statErr) {
		t.Fatal("sidecar should exist before delete")
	}

	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Error("sidecar should be removed after task delete")
	}
}

func TestStoreListPopulatesPlan(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("List plan task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	plan := "## Plan\n\nsteps here"
	if _, err := store.Update(created.ID, map[string]any{"plan": plan}); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].Plan != plan {
		t.Errorf("List Plan = %q, want %q", tasks[0].Plan, plan)
	}
}
