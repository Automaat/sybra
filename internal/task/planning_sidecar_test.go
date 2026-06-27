package task

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreUpdateGetListPlanningSidecars(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("Plan artifacts", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	research := "# Research\n\nFacts."
	decisions := "# Decisions\n\nNone."
	brief := "# Final Brief\n\nReady."

	updated, err := store.Update(created.ID, Update{
		PlanResearch:  Ptr(research),
		PlanDecisions: Ptr(decisions),
		PlanBrief:     Ptr(brief),
	})
	if err != nil {
		t.Fatalf("Update planning sidecars: %v", err)
	}
	if updated.PlanResearch != research || updated.PlanDecisions != decisions || updated.PlanBrief != brief {
		t.Fatalf("updated sidecars = (%q, %q, %q)", updated.PlanResearch, updated.PlanDecisions, updated.PlanBrief)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanResearch != research {
		t.Errorf("Get PlanResearch = %q, want %q", got.PlanResearch, research)
	}
	if got.PlanDecisions != decisions {
		t.Errorf("Get PlanDecisions = %q, want %q", got.PlanDecisions, decisions)
	}
	if got.PlanBrief != brief {
		t.Errorf("Get PlanBrief = %q, want %q", got.PlanBrief, brief)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List length = %d, want 1", len(listed))
	}
	if listed[0].PlanResearch != research || listed[0].PlanDecisions != decisions || listed[0].PlanBrief != brief {
		t.Fatalf("listed sidecars = (%q, %q, %q)", listed[0].PlanResearch, listed[0].PlanDecisions, listed[0].PlanBrief)
	}
}

func TestStoreSequentialPlanningSidecarUpdatesPreserveWarmListCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("Warm cache", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil {
		t.Fatalf("warm list cache: %v", err)
	}

	research := "# Research\n"
	decisions := "# Decisions\n"
	brief := "# Brief\n"
	plan := "# Execution Plan\n"
	for _, update := range []Update{
		{PlanResearch: Ptr(research)},
		{PlanDecisions: Ptr(decisions)},
		{Plan: Ptr(plan)},
		{PlanBrief: Ptr(brief)},
	} {
		if _, err := store.Update(created.ID, update); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}

	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List length = %d, want 1", len(listed))
	}
	got := listed[0]
	if got.PlanResearch != research || got.PlanDecisions != decisions || got.Plan != plan || got.PlanBrief != brief {
		t.Fatalf("cached sidecars = plan:%q research:%q decisions:%q brief:%q", got.Plan, got.PlanResearch, got.PlanDecisions, got.PlanBrief)
	}
}

func TestStoreDeleteCascadesPlanningSidecars(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create("Delete sidecars", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(created.ID, Update{
		PlanResearch:  Ptr("# Research\n"),
		PlanDecisions: Ptr("# Decisions\n"),
		PlanBrief:     Ptr("# Brief\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".plan-research.md", ".plan-decisions.md", ".plan-brief.md"} {
		path := filepath.Join(dir, created.ID+suffix)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("sidecar %s still exists or stat failed: %v", path, err)
		}
	}
}
