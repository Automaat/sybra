package workflow

import (
	"testing"
)

// TestBuiltinSimpleTask_MaybeCritiqueReplanSkip locks the behavior that
// critique_plan runs only on the first plan pass. On replan (after a human
// reject), `vars.step.review_plan.output` exists, so maybe_critique must
// route directly to review_plan and spare the critic a second run on a
// plan the human already eyeballed once. Substring-checking tag "nocritic"
// is still honored as an opt-out on the first pass.
func TestBuiltinSimpleTask_MaybeCritiqueReplanSkip(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var simple *Definition
	for i := range defs {
		if defs[i].ID == "simple-task" {
			simple = &defs[i]
			break
		}
	}
	if simple == nil {
		t.Fatal("simple-task builtin definition not found")
	}
	step := simple.StepByID("maybe_critique")
	if step == nil {
		t.Fatal("maybe_critique step not found in simple-task")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name: "first_pass_runs_critique",
			fields: map[string]string{
				"task.tags": "backend,feature",
			},
			want: "critique_plan",
		},
		{
			name: "nocritic_tag_skips_critique",
			fields: map[string]string{
				"task.tags": "backend,nocritic",
			},
			want: "review_plan",
		},
		{
			name: "replan_skips_critique_even_without_nocritic",
			fields: map[string]string{
				"task.tags":                    "backend,feature",
				"vars.step.review_plan.output": "reject",
				"vars.human_action":            "reject",
			},
			want: "review_plan",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuiltinTestingTask_MaybeCritiqueReplanSkip is the testing-task
// mirror of the simple-task replan-skip guarantee above.
func TestBuiltinTestingTask_MaybeCritiqueReplanSkip(t *testing.T) {
	t.Parallel()

	defs, err := BuiltinDefinitions()
	if err != nil {
		t.Fatalf("BuiltinDefinitions: %v", err)
	}
	var testing_ *Definition
	for i := range defs {
		if defs[i].ID == "testing-task" {
			testing_ = &defs[i]
			break
		}
	}
	if testing_ == nil {
		t.Fatal("testing-task builtin definition not found")
	}
	step := testing_.StepByID("maybe_critique_test")
	if step == nil {
		t.Fatal("maybe_critique_test step not found in testing-task")
	}

	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "first_pass_runs_critique",
			fields: map[string]string{"task.tags": "testing"},
			want:   "critique_test_plan",
		},
		{
			name:   "nocritic_tag_skips_critique",
			fields: map[string]string{"task.tags": "testing,nocritic"},
			want:   "review_test_plan",
		},
		{
			name: "replan_skips_critique_even_without_nocritic",
			fields: map[string]string{
				"task.tags":                         "testing",
				"vars.step.review_test_plan.output": "reject",
			},
			want: "review_test_plan",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTransition(step.Next, tc.fields)
			if err != nil {
				t.Fatalf("ResolveTransition: %v", err)
			}
			if got != tc.want {
				t.Errorf("goto = %q, want %q", got, tc.want)
			}
		})
	}
}

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
