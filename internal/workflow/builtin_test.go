package workflow

import "testing"

func TestBuiltinDefinitions(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	if len(defs) == 0 {
		t.Fatal("expected non-empty builtin definitions")
	}
	for _, d := range defs {
		if d.ID == "" {
			t.Errorf("builtin definition has empty ID: %+v", d)
		}
		if !d.Builtin {
			t.Errorf("builtin definition %q has Builtin=false", d.ID)
		}
	}
}

func TestBuiltinDefinitions_Valid(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	for _, d := range defs {
		t.Run(d.ID, func(t *testing.T) {
			t.Parallel()
			if err := d.Validate(); err != nil {
				t.Errorf("Validate() error for %q: %v", d.ID, err)
			}
		})
	}
}

func TestBuiltinDefinitions_NoDuplicateIDs(t *testing.T) {
	t.Parallel()
	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	seen := make(map[string]bool)
	for _, d := range defs {
		if seen[d.ID] {
			t.Errorf("duplicate builtin ID: %q", d.ID)
		}
		seen[d.ID] = true
	}
}

func TestSyncBuiltins(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != len(defs) {
		t.Errorf("store has %d workflows, want %d", len(listed), len(defs))
	}
}

func TestSyncBuiltins_NoOverwrite(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", err, len(defs))
	}

	// Save a modified version of the first builtin.
	modified := defs[0]
	modified.Name = "user-modified"
	modified.Builtin = false // simulate user edit
	if err := store.Save(modified); err != nil {
		t.Fatalf("Save modified: %v", err)
	}

	// SyncBuiltins must not overwrite.
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	got, err := store.Get(modified.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "user-modified" {
		t.Errorf("SyncBuiltins overwrote user modification: got %q, want %q", got.Name, "user-modified")
	}
}

// TestSyncBuiltins_OverwriteStaleBuiltin locks the drift-repair behavior:
// a stored workflow still marked Builtin=true must get replaced when its
// content diverges from the embedded copy. Matches the historical pr-fix
// drift that left `operator: contains` on a scalar enum field and silently
// broke all auto-fix dispatch.
func TestSyncBuiltins_OverwriteStaleBuiltin(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v (len=%d)", err, len(defs))
	}

	// Simulate drift: save with Builtin=true but a stale name.
	stale := defs[0]
	stale.Name = "stale-drifted-name"
	stale.Builtin = true
	if err := store.Save(stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}

	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}

	got, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != defs[0].Name {
		t.Errorf("SyncBuiltins did not repair stale builtin: got %q, want %q",
			got.Name, defs[0].Name)
	}
}

// TestSyncBuiltins_IdempotentOnClean verifies the no-op path: calling
// SyncBuiltins twice in a row on a freshly seeded store must not bounce
// the UpdatedAt timestamp on every startup.
func TestSyncBuiltins_IdempotentOnClean(t *testing.T) {
	t.Parallel()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("first SyncBuiltins: %v", err)
	}
	defs, err := BuiltinDefinitions()
	if err != nil || len(defs) == 0 {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	before, err := store.Get(defs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := SyncBuiltins(store); err != nil {
		t.Fatalf("second SyncBuiltins: %v", err)
	}
	after, err := store.Get(defs[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("idempotent sync rewrote file: UpdatedAt before=%v after=%v",
			before.UpdatedAt, after.UpdatedAt)
	}
}
