package task

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanCritiqueStoreReadNonexistent(t *testing.T) {
	t.Parallel()
	pcs := NewPlanCritiqueStore(t.TempDir())

	content, err := pcs.Read("nonexistent")
	if err != nil {
		t.Fatalf("expected no error for missing critique, got %v", err)
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

func TestPlanCritiqueStoreWriteRead(t *testing.T) {
	t.Parallel()
	pcs := NewPlanCritiqueStore(t.TempDir())

	want := "# Plan Review\n\n## Verdict: APPROVE\n"
	if err := pcs.Write("task-abc", want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := pcs.Read("task-abc")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPlanCritiqueStoreDeleteExisting(t *testing.T) {
	t.Parallel()
	pcs := NewPlanCritiqueStore(t.TempDir())

	if err := pcs.Write("task-del", "some critique"); err != nil {
		t.Fatal(err)
	}
	if err := pcs.Delete("task-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	content, err := pcs.Read("task-del")
	if err != nil {
		t.Fatalf("Read after delete: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty after delete, got %q", content)
	}
}

func TestPlanCritiqueStoreDeleteNonexistent(t *testing.T) {
	t.Parallel()
	pcs := NewPlanCritiqueStore(t.TempDir())

	if err := pcs.Delete("nope"); err != nil {
		t.Fatalf("Delete nonexistent should not error, got %v", err)
	}
}

func TestPlanCritiqueStoreWriteEmptyClears(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pcs := NewPlanCritiqueStore(dir)

	if err := pcs.Write("task-clear", "initial critique"); err != nil {
		t.Fatal(err)
	}
	if err := pcs.Write("task-clear", ""); err != nil {
		t.Fatalf("Write empty: %v", err)
	}

	sidecar := filepath.Join(dir, "task-clear.plan-critique.md")
	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Error("sidecar file should not exist after writing empty string")
	}
}

func TestStoreUpdateWritesPlanCritiqueSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Critique task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	critique := "# Plan Review\n\n## Verdict: REFINE\n\n- Add edge case handling.\n"
	updated, err := store.Update(created.ID, Update{PlanCritique: Ptr(critique)})
	if err != nil {
		t.Fatalf("Update with plan_critique: %v", err)
	}
	if updated.PlanCritique != critique {
		t.Errorf("PlanCritique = %q, want %q", updated.PlanCritique, critique)
	}

	sidecar := filepath.Join(dir, created.ID+".plan-critique.md")
	data, readErr := os.ReadFile(sidecar)
	if readErr != nil {
		t.Fatalf("sidecar not written: %v", readErr)
	}
	if string(data) != critique {
		t.Errorf("sidecar = %q, want %q", string(data), critique)
	}
}

func TestStoreGetPopulatesPlanCritique(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Critique get task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	critique := "# Plan Review\n\n## Verdict: APPROVE\n"
	if _, err := store.Update(created.ID, Update{PlanCritique: Ptr(critique)}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanCritique != critique {
		t.Errorf("Get PlanCritique = %q, want %q", got.PlanCritique, critique)
	}
}

func TestStoreDeleteCascadesPlanCritique(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Cascade critique delete", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(created.ID, Update{PlanCritique: Ptr("# Plan Review\n")}); err != nil {
		t.Fatal(err)
	}

	sidecar := filepath.Join(dir, created.ID+".plan-critique.md")
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

func TestStoreListPopulatesPlanCritique(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("List critique task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	critique := "# Plan Review\n\n## Verdict: APPROVE\n"
	if _, err := store.Update(created.ID, Update{PlanCritique: Ptr(critique)}); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].PlanCritique != critique {
		t.Errorf("List PlanCritique = %q, want %q", tasks[0].PlanCritique, critique)
	}
}
