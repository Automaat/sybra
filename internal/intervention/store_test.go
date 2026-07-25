package intervention

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

func TestStore_PutDedupsByFingerprintAndTracksRecurrence(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	rec1 := Record{
		TaskID:              "task-1",
		CreatedAt:           base,
		BlockerKind:         "operator_decision",
		BlockerCode:         "no_project_assigned",
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
	}
	rec1.Fingerprint = Fingerprint(rec1)
	if err := store.Put("owner/repo", rec1); err != nil {
		t.Fatal(err)
	}

	got, err := store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Recurrences != 1 {
		t.Fatalf("after first Put: got %+v, want one record with Recurrences=1", got)
	}
	if got[0].ReplayStatus != ReplayStatusUnsupportedSimulation {
		t.Fatalf("ReplayStatus = %q, want %q", got[0].ReplayStatus, ReplayStatusUnsupportedSimulation)
	}

	// Equivalent intervention on a different task, later in time.
	rec2 := Record{
		TaskID:              "task-2",
		CreatedAt:           base.Add(time.Hour),
		BlockerKind:         "operator_decision",
		BlockerCode:         "no_project_assigned",
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
	}
	rec2.Fingerprint = Fingerprint(rec2)
	if err := store.Put("owner/repo", rec2); err != nil {
		t.Fatal(err)
	}

	got, err = store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Query) = %d, want 1 (equivalent interventions must dedup to one file)", len(got))
	}
	if got[0].Recurrences != 2 {
		t.Fatalf("Recurrences = %d, want 2 on the repeat", got[0].Recurrences)
	}
	if got[0].TaskID != "task-2" {
		t.Fatalf("TaskID = %q, want the newest occurrence's task-2", got[0].TaskID)
	}
	if !got[0].FirstSeen.Equal(base) {
		t.Fatalf("FirstSeen = %v, want the original occurrence's %v", got[0].FirstSeen, base)
	}
	if !got[0].LastSeen.Equal(base.Add(time.Hour)) {
		t.Fatalf("LastSeen = %v, want the newest occurrence's %v", got[0].LastSeen, base.Add(time.Hour))
	}

	// A distinct intervention (different blocker code) must not collapse
	// into the same record.
	rec3 := Record{
		TaskID:              "task-3",
		CreatedAt:           base.Add(2 * time.Hour),
		BlockerKind:         "operator_decision",
		BlockerCode:         "task_cost_exceeded",
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
	}
	rec3.Fingerprint = Fingerprint(rec3)
	if err := store.Put("owner/repo", rec3); err != nil {
		t.Fatal(err)
	}

	got, err = store.Query("owner/repo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(Query) = %d, want 2 (distinct fingerprint must not dedup)", len(got))
	}
}

func TestStore_ProjectScoping(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := Record{
		TaskID:              "task-a",
		CreatedAt:           time.Now(),
		BlockerKind:         "operator_decision",
		OperatorActionClass: OperatorActionHuman,
		FromStatus:          "human-required",
		ToStatus:            "in-progress",
	}
	rec.Fingerprint = Fingerprint(rec)
	if err := store.Put("owner/repo-a", rec); err != nil {
		t.Fatal(err)
	}

	workKey := ProjectKey(mustWorkProject(t))
	rec2 := rec
	rec2.TaskID = "task-b"
	if err := store.Put(workKey, rec2); err != nil {
		t.Fatal(err)
	}

	a, err := store.Query("owner/repo-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].TaskID != "task-a" {
		t.Fatalf("owner/repo-a Query = %+v, want only task-a", a)
	}

	w, err := store.Query(workKey, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 || w[0].TaskID != "task-b" {
		t.Fatalf("work project Query = %+v, want only task-b", w)
	}
}

func mustWorkProject(t *testing.T) project.Project {
	t.Helper()
	return project.Project{
		ID:    "acme/api",
		Type:  project.ProjectTypeWork,
		Owner: "acme",
		Repo:  "api",
		URL:   "https://github.com/acme/api",
	}
}

func TestStore_PutRejectsEmptyFingerprint(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("owner/repo", Record{TaskID: "task-a"}); err == nil {
		t.Fatal("Put with empty fingerprint: want error, got nil")
	}
}
