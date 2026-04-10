package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestDef(id string) Definition {
	return Definition{
		ID:      id,
		Name:    "Test Workflow",
		Trigger: Trigger{On: "task.created"},
		Steps: []Step{
			{ID: "s1", Type: StepSetStatus, Config: StepConfig{Status: "todo"}},
		},
	}
}

func TestNewStore(t *testing.T) {
	t.Parallel()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestStore_SaveAndGet(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())

	def := newTestDef("my-workflow")
	if err := s.Save(def); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Get("my-workflow")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "my-workflow" {
		t.Errorf("got ID %q, want %q", got.ID, "my-workflow")
	}
	if got.Name != "Test Workflow" {
		t.Errorf("got Name %q, want %q", got.Name, "Test Workflow")
	}
}

func TestStore_Save_EmptyID(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())
	if err := s.Save(Definition{}); err == nil {
		t.Fatal("expected error for empty ID")
	}
}

func TestStore_Save_Invalid(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())
	def := Definition{
		ID:    "bad-wf",
		Steps: []Step{{ID: "s1", Config: StepConfig{MaxRetries: 11}}},
	}
	if err := s.Save(def); err == nil {
		t.Fatal("expected validation error for MaxRetries > 10")
	}
}

func TestStore_List(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())

	for _, id := range []string{"wf-a", "wf-b", "wf-c"} {
		if err := s.Save(newTestDef(id)); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	defs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(defs) != 3 {
		t.Errorf("got %d defs, want 3", len(defs))
	}
}

func TestStore_List_SkipsBadFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, _ := NewStore(dir)

	if err := s.Save(newTestDef("good-wf")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Write a malformed YAML file.
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte(": : :"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	defs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(defs) != 1 {
		t.Errorf("got %d defs, want 1 (bad file skipped)", len(defs))
	}
}

func TestStore_Delete(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())

	if err := s.Save(newTestDef("to-delete")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete("to-delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("to-delete"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestStore_Delete_NotFound(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())
	if err := s.Delete("nonexistent"); err == nil {
		t.Fatal("expected error for non-existent workflow")
	}
}

func TestStore_SafePath_Traversal(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())

	traversalIDs := []string{
		"../etc/passwd",
		"foo/../../bar",
		"../outside",
	}
	for _, id := range traversalIDs {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			if _, err := s.Get(id); err == nil {
				t.Errorf("expected error for traversal ID %q", id)
			}
		})
	}
}

func TestStore_SafePath_Valid(t *testing.T) {
	t.Parallel()
	s, _ := NewStore(t.TempDir())

	// Should not error on path validation (file not found is fine here).
	_, err := s.Get("valid-id")
	// Expect "not found" error, not a path-traversal error.
	if err == nil {
		t.Fatal("expected error (file not found)")
	}
	// Should not contain "invalid workflow ID".
	if err.Error() == "invalid workflow ID \"valid-id\"" {
		t.Error("got path traversal error for valid ID")
	}
}

// TestStore_ParseFile_WarnOnInvalidFieldsButReturn confirms the read path
// stays non-disruptive: a hand-edited user workflow with an unknown field
// still loads (so the app boots and the user can fix it in the GUI),
// while the Save path remains strict (enforced by other tests). This
// split is deliberate — failing Load on any invalid workflow would
// cascade a single bad file into a crashed app startup.
func TestStore_ParseFile_WarnOnInvalidFieldsButReturn(t *testing.T) {
	t.Parallel()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Write a workflow directly to disk (bypassing Save's validation) that
	// references an unknown field — mimicking a hand-edited user file.
	raw := `id: hand-edited
name: Hand Edited
trigger:
  on: pr.event
  conditions:
    - field: project.type
      operator: equals
      value: pet
steps:
  - id: noop
    name: Noop
    type: set_status
    config:
      status: done
    next:
      - goto: ""
`
	path := filepath.Join(s.Dir(), "hand-edited.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write hand-edited: %v", err)
	}

	// List must still return the def (warn-not-fail on load).
	defs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *Definition
	for i := range defs {
		if defs[i].ID == "hand-edited" {
			found = &defs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("hand-edited workflow should still load despite invalid field")
	}

	// But Save must reject the same definition — strict write-path check.
	if err := s.Save(*found); err == nil {
		t.Error("Save should reject workflow with unknown field")
	}
}
