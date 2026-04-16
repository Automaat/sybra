package task

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodeReviewStoreReadNonexistent(t *testing.T) {
	t.Parallel()
	crs := NewCodeReviewStore(t.TempDir())

	content, err := crs.Read("nonexistent")
	if err != nil {
		t.Fatalf("expected no error for missing review, got %v", err)
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

func TestCodeReviewStoreWriteRead(t *testing.T) {
	t.Parallel()
	crs := NewCodeReviewStore(t.TempDir())

	want := "# Code Review\n\n## Verdict: APPROVE\n"
	if err := crs.Write("task-abc", want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := crs.Read("task-abc")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCodeReviewStoreDeleteExisting(t *testing.T) {
	t.Parallel()
	crs := NewCodeReviewStore(t.TempDir())

	if err := crs.Write("task-del", "some review"); err != nil {
		t.Fatal(err)
	}
	if err := crs.Delete("task-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	content, err := crs.Read("task-del")
	if err != nil {
		t.Fatalf("Read after delete: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty after delete, got %q", content)
	}
}

func TestCodeReviewStoreDeleteNonexistent(t *testing.T) {
	t.Parallel()
	crs := NewCodeReviewStore(t.TempDir())

	if err := crs.Delete("nope"); err != nil {
		t.Fatalf("Delete nonexistent should not error, got %v", err)
	}
}

func TestCodeReviewStoreWriteEmptyClears(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	crs := NewCodeReviewStore(dir)

	if err := crs.Write("task-clear", "initial review"); err != nil {
		t.Fatal(err)
	}
	if err := crs.Write("task-clear", ""); err != nil {
		t.Fatalf("Write empty: %v", err)
	}

	sidecar := filepath.Join(dir, "task-clear.review.md")
	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Error("sidecar file should not exist after writing empty string")
	}
}

func TestStoreUpdateWritesCodeReviewSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Review task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	review := "# Code Review\n\n## Findings\n\n- Missing tests for X.\n"
	updated, err := store.Update(created.ID, Update{CodeReview: Ptr(review)})
	if err != nil {
		t.Fatalf("Update with code_review: %v", err)
	}
	if updated.CodeReview != review {
		t.Errorf("CodeReview = %q, want %q", updated.CodeReview, review)
	}

	sidecar := filepath.Join(dir, created.ID+".review.md")
	data, readErr := os.ReadFile(sidecar)
	if readErr != nil {
		t.Fatalf("sidecar not written: %v", readErr)
	}
	if string(data) != review {
		t.Errorf("sidecar = %q, want %q", string(data), review)
	}
}

func TestStoreGetPopulatesCodeReview(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Review get task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	review := "# Code Review\n\n## Verdict: APPROVE\n"
	if _, err := store.Update(created.ID, Update{CodeReview: Ptr(review)}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CodeReview != review {
		t.Errorf("Get CodeReview = %q, want %q", got.CodeReview, review)
	}
}

func TestStoreDeleteCascadesCodeReview(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("Cascade review delete", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update(created.ID, Update{CodeReview: Ptr("# Code Review\n")}); err != nil {
		t.Fatal(err)
	}

	sidecar := filepath.Join(dir, created.ID+".review.md")
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

func TestStoreListPopulatesCodeReview(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create("List review task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}

	review := "# Code Review\n\n## Verdict: APPROVE\n"
	if _, err := store.Update(created.ID, Update{CodeReview: Ptr(review)}); err != nil {
		t.Fatal(err)
	}

	tasks, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].CodeReview != review {
		t.Errorf("List CodeReview = %q, want %q", tasks[0].CodeReview, review)
	}
}
