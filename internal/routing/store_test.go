package routing

import (
	"testing"
	"time"
)

func TestStore_LoadMissingReturnsNotOK(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	_, ok, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if ok {
		t.Fatalf("Load() ok = true, want false for a fresh store")
	}
}

func TestStore_SaveLoadRoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	want := Overlay{
		Version:     3,
		GeneratedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Experiments: []OverlayExperiment{
			{
				ExperimentID: "exp",
				Variants: []OverlayVariant{
					{VariantID: "v1", Weight: 7, Score: 1.5, Runs: 40, ResolvedRuns: 38},
				},
			},
		},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
		panic("unreachable")
	}
	got, ok, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
		panic("unreachable")
	}
	if !ok {
		t.Fatalf("Load() ok = false, want true after Save")
	}
	if got.Version != want.Version || !got.GeneratedAt.Equal(want.GeneratedAt) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
	if len(got.Experiments) != 1 || got.Experiments[0].ExperimentID != "exp" {
		t.Fatalf("Load().Experiments = %+v, want one exp experiment", got.Experiments)
	}
	if w, ok := got.WeightAt("exp", "v1"); !ok || w != 7 {
		t.Fatalf("WeightAt(exp, v1) = (%d, %v), want (7, true)", w, ok)
	}
}

func TestStore_SaveOverwritesPriorGeneration(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
		panic("unreachable")
	}
	if err := store.Save(Overlay{Version: 1}); err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if err := store.Save(Overlay{Version: 2}); err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	got, ok, err := store.Load()
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
		panic("unreachable")
	}
	if got.Version != 2 {
		t.Fatalf("Load().Version = %d, want 2", got.Version)
	}
}
