package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sidecarCase names one sidecar by the accessor that exposes it, the suffix it
// occupies on disk, and the Update/Task field pair it round-trips through.
type sidecarCase struct {
	name   string
	suffix string
	label  string
	store  func(*Store) *PlanningSidecarStore
	set    func(*Update, string)
	get    func(Task) string
}

func sidecarCases() []sidecarCase {
	return []sidecarCase{
		{
			name:   "plan",
			label:  "plan",
			suffix: ".plan.md",
			store:  (*Store).Plans,
			set:    func(u *Update, v string) { u.Plan = Ptr(v) },
			get:    func(t Task) string { return t.Plan },
		},
		{
			name:   "plan contract",
			label:  "plan contract",
			suffix: ".plan-contract.json",
			store:  (*Store).PlanContracts,
			set:    func(u *Update, v string) { u.PlanContract = Ptr(v) },
			get:    func(t Task) string { return t.PlanContract },
		},
		{
			name:   "plan critique",
			label:  "plan critique",
			suffix: ".plan-critique.md",
			store:  (*Store).PlanCritiques,
			set:    func(u *Update, v string) { u.PlanCritique = Ptr(v) },
			get:    func(t Task) string { return t.PlanCritique },
		},
		{
			name:   "plan research",
			label:  "plan research",
			suffix: ".plan-research.md",
			store:  (*Store).PlanResearch,
			set:    func(u *Update, v string) { u.PlanResearch = Ptr(v) },
			get:    func(t Task) string { return t.PlanResearch },
		},
		{
			name:   "plan decisions",
			label:  "plan decisions",
			suffix: ".plan-decisions.md",
			store:  (*Store).PlanDecisions,
			set:    func(u *Update, v string) { u.PlanDecisions = Ptr(v) },
			get:    func(t Task) string { return t.PlanDecisions },
		},
		{
			name:   "plan brief",
			label:  "plan brief",
			suffix: ".plan-brief.md",
			store:  (*Store).PlanBrief,
			set:    func(u *Update, v string) { u.PlanBrief = Ptr(v) },
			get:    func(t Task) string { return t.PlanBrief },
		},
		{
			name:   "code review",
			label:  "code review",
			suffix: ".review.md",
			store:  (*Store).CodeReviews,
			set:    func(u *Update, v string) { u.CodeReview = Ptr(v) },
			get:    func(t Task) string { return t.CodeReview },
		},
	}
}

func TestPlanningSidecarStoreReadWriteDelete(t *testing.T) {
	t.Parallel()
	for _, tc := range sidecarCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			sidecar := tc.store(store)

			if content, err := sidecar.Read("nonexistent"); err != nil || content != "" {
				t.Fatalf("Read missing = (%q, %v), want (%q, nil)", content, err, "")
			}

			want := "# " + tc.name + "\n\ncontent"
			if err := sidecar.Write("task-abc", want); err != nil {
				t.Fatalf("Write: %v", err)
			}
			got, err := sidecar.Read("task-abc")
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got != want {
				t.Errorf("Read = %q, want %q", got, want)
			}
			if path := filepath.Join(dir, "task-abc"+tc.suffix); !fileExists(path) {
				t.Errorf("sidecar %s not written", path)
			}

			if err := sidecar.Delete("task-abc"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if content, err := sidecar.Read("task-abc"); err != nil || content != "" {
				t.Fatalf("Read after delete = (%q, %v), want (%q, nil)", content, err, "")
			}
			if err := sidecar.Delete("task-abc"); err != nil {
				t.Fatalf("Delete missing should not error, got %v", err)
			}

			if err := sidecar.Write("task-clear", "initial"); err != nil {
				t.Fatal(err)
			}
			if err := sidecar.Write("task-clear", ""); err != nil {
				t.Fatalf("Write empty: %v", err)
			}
			if path := filepath.Join(dir, "task-clear"+tc.suffix); fileExists(path) {
				t.Errorf("sidecar %s should be removed by an empty write", path)
			}
		})
	}
}

// Callers such as clusterlead mirroring surface these errors unwrapped, so the
// store's own label is the only thing naming which of the seven sidecars failed.
func TestPlanningSidecarStoreErrorsNameTheSidecar(t *testing.T) {
	t.Parallel()
	for _, tc := range sidecarCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			sidecar := tc.store(store)

			// A non-empty directory where the sidecar file belongs fails
			// all three operations from one fixture: EISDIR, ENOTEMPTY, and a rename collision.
			blocked := filepath.Join(dir, "task-err"+tc.suffix)
			if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o755); err != nil {
				t.Fatal(err)
			}

			_, readErr := sidecar.Read("task-err")
			assertErrPrefix(t, "Read", readErr, "read "+tc.label)
			assertErrPrefix(t, "Write", sidecar.Write("task-err", "content"), "write "+tc.label)
			assertErrPrefix(t, "Delete", sidecar.Delete("task-err"), "delete "+tc.label)
		})
	}
}

func assertErrPrefix(t *testing.T, op string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want an error naming %q, got nil", op, want)
	}
	if !strings.HasPrefix(err.Error(), want+": ") {
		t.Errorf("%s error = %q, want prefix %q", op, err, want+": ")
	}
}

func TestStoreRoundTripsEachSidecar(t *testing.T) {
	t.Parallel()
	for _, tc := range sidecarCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store, err := NewStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			created, err := store.Create("Sidecar task", "body", "headless")
			if err != nil {
				t.Fatal(err)
			}

			want := "# " + tc.name + "\n\nfindings"
			var update Update
			tc.set(&update, want)
			updated, err := store.Update(created.ID, update)
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got := tc.get(updated); got != want {
				t.Errorf("Update returned %q, want %q", got, want)
			}

			path := filepath.Join(dir, created.ID+tc.suffix)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("sidecar not written: %v", err)
			}
			if string(data) != want {
				t.Errorf("sidecar file = %q, want %q", string(data), want)
			}

			got, err := store.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if v := tc.get(got); v != want {
				t.Errorf("Get = %q, want %q", v, want)
			}

			listed, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 {
				t.Fatalf("List length = %d, want 1", len(listed))
			}
			if v := tc.get(listed[0]); v != want {
				t.Errorf("List = %q, want %q", v, want)
			}

			if err := store.Delete(created.ID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if fileExists(path) {
				t.Errorf("sidecar %s should be removed with the task", path)
			}
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

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
	contract := `{"task_id":"task-abc","verification":[{"command":"go test ./...","expected":"passes"}]}`

	updated, err := store.Update(created.ID, Update{
		PlanResearch:  Ptr(research),
		PlanDecisions: Ptr(decisions),
		PlanBrief:     Ptr(brief),
		PlanContract:  Ptr(contract),
	})
	if err != nil {
		t.Fatalf("Update planning sidecars: %v", err)
	}
	if updated.PlanResearch != research || updated.PlanDecisions != decisions || updated.PlanBrief != brief || updated.PlanContract != contract {
		t.Fatalf("updated sidecars = (%q, %q, %q, %q)", updated.PlanResearch, updated.PlanDecisions, updated.PlanBrief, updated.PlanContract)
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
	if got.PlanContract != contract {
		t.Errorf("Get PlanContract = %q, want %q", got.PlanContract, contract)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List length = %d, want 1", len(listed))
	}
	if listed[0].PlanResearch != research || listed[0].PlanDecisions != decisions || listed[0].PlanBrief != brief || listed[0].PlanContract != contract {
		t.Fatalf("listed sidecars = (%q, %q, %q, %q)", listed[0].PlanResearch, listed[0].PlanDecisions, listed[0].PlanBrief, listed[0].PlanContract)
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
	contract := `{"task_id":"warm-cache","verification":[{"command":"go test ./...","expected":"passes"}]}`
	for _, update := range []Update{
		{PlanResearch: Ptr(research)},
		{PlanDecisions: Ptr(decisions)},
		{Plan: Ptr(plan)},
		{PlanBrief: Ptr(brief)},
		{PlanContract: Ptr(contract)},
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
	if got.PlanResearch != research || got.PlanDecisions != decisions || got.Plan != plan || got.PlanBrief != brief || got.PlanContract != contract {
		t.Fatalf("cached sidecars = plan:%q research:%q decisions:%q brief:%q contract:%q", got.Plan, got.PlanResearch, got.PlanDecisions, got.PlanBrief, got.PlanContract)
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
		PlanContract:  Ptr(`{"task_id":"delete-sidecars"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(created.ID); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".plan-research.md", ".plan-decisions.md", ".plan-brief.md", ".plan-contract.json"} {
		path := filepath.Join(dir, created.ID+suffix)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("sidecar %s still exists or stat failed: %v", path, err)
		}
	}
}
