package loopagent

import (
	"strings"
	"sync"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		la      LoopAgent
		wantErr string
	}{
		{
			name: "valid",
			la:   LoopAgent{Name: "x", Prompt: "/foo", IntervalSec: 60, Provider: "claude"},
		},
		{
			name:    "missing name",
			la:      LoopAgent{Prompt: "/foo", IntervalSec: 60},
			wantErr: "name is required",
		},
		{
			name:    "missing prompt",
			la:      LoopAgent{Name: "x", IntervalSec: 60},
			wantErr: "prompt is required",
		},
		{
			name:    "interval below minimum",
			la:      LoopAgent{Name: "x", Prompt: "/foo", IntervalSec: 30},
			wantErr: "interval_sec must be >= 60",
		},
		{
			name:    "invalid provider",
			la:      LoopAgent{Name: "x", Prompt: "/foo", IntervalSec: 60, Provider: "gpt4"},
			wantErr: "provider must be one of claude, codex, copilot",
		},
		{
			name: "codex provider accepted",
			la:   LoopAgent{Name: "x", Prompt: "/foo", IntervalSec: 60, Provider: "codex"},
		},
		{
			name: "copilot provider accepted",
			la:   LoopAgent{Name: "x", Prompt: "/foo", IntervalSec: 60, Provider: "copilot"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.la.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestStoreCRUD(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	// Empty list
	got, err := store.List()
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d", len(got))
	}

	// Create — provider defaults to claude when blank
	created, err := store.Create(LoopAgent{Name: "self-monitor", Prompt: "/sybra-self-monitor", IntervalSec: 3600})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created ID is empty")
	}
	if created.Provider != "claude" {
		t.Fatalf("expected provider claude, got %q", created.Provider)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set")
	}

	// Get round-trip
	fetched, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.Name != "self-monitor" || fetched.IntervalSec != 3600 {
		t.Fatalf("round-trip mismatch: %+v", fetched)
	}

	// FindByName
	if _, ok := store.FindByName("self-monitor"); !ok {
		t.Fatal("FindByName failed")
	}
	if _, ok := store.FindByName("nope"); ok {
		t.Fatal("FindByName found nonexistent")
	}

	// Create rejects invalid
	if _, err := store.Create(LoopAgent{Name: "x", Prompt: "/y", IntervalSec: 10}); err == nil {
		t.Fatal("expected interval validation error")
	}

	// Update preserves CreatedAt and ID
	created.IntervalSec = 7200
	created.Enabled = true
	updated, err := store.Update(created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("update changed ID: %s vs %s", updated.ID, created.ID)
	}
	if !updated.CreatedAt.Equal(fetched.CreatedAt) {
		t.Fatalf("update overwrote CreatedAt")
	}
	if updated.IntervalSec != 7200 || !updated.Enabled {
		t.Fatalf("update did not persist mutations: %+v", updated)
	}
	if !updated.UpdatedAt.After(fetched.UpdatedAt) && !updated.UpdatedAt.Equal(fetched.UpdatedAt) {
		// Allow equal in case the test runs within the same nanosecond resolution.
		t.Fatalf("UpdatedAt not advanced")
	}

	// List sorted by name
	if _, err := store.Create(LoopAgent{Name: "alpha", Prompt: "/a", IntervalSec: 60}); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 || listed[0].Name != "alpha" || listed[1].Name != "self-monitor" {
		t.Fatalf("list not sorted by name: %+v", listed)
	}

	// Delete
	if err := store.Delete(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(created.ID); err == nil {
		t.Fatal("expected error after delete")
	}

	// Delete missing is not an error
	if err := store.Delete("nonexistent"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

// TestStoreConcurrentUpdatePreservesCreatedAt exercises the Get-then-write
// critical section inside Update under real concurrency. Both goroutines
// start from the same on-disk CreatedAt; without flocking that section, one
// Update can read the record while the other is mid-write and persist a
// half-applied CreatedAt/UpdatedAt pairing. Because Update fully replaces
// the caller-supplied fields (there is no field-level merge), the two
// concurrent writers race for whichever mutation lands last — this test
// only pins the invariant Update itself owns: the final record on disk is
// always a complete, validly-parsed record whose CreatedAt matches the
// original.
func TestStoreConcurrentUpdatePreservesCreatedAt(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	created, err := store.Create(LoopAgent{Name: "race", Prompt: "/race", IntervalSec: 60})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		la := created
		la.Enabled = i%2 == 0
		wg.Go(func() {
			if _, err := store.Update(la); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent update: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get after concurrent updates: %v", err)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v — dropped by a concurrent write racing the read-modify-write", got.CreatedAt, created.CreatedAt)
	}
	if got.Name != "race" || got.IntervalSec != 60 {
		t.Errorf("record corrupted by concurrent writes: %+v", got)
	}
}
