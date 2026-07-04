package sybra

import (
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestFixRenovateCI_WorktreePrepareFailureEscalates is the regression test
// for issue #1454's "no human-required flip" symptom: when preparing the fix
// worktree fails for a reason PrepareForFix itself cannot recover from (here,
// an unregistered project — the same code path a genuinely missing bare clone
// would hit), the task must not be left silently stranded in its initial
// status. It must flip to human-required with a reason, so it surfaces on
// the board instead of only a log line.
func TestFixRenovateCI_WorktreePrepareFailureEscalates(t *testing.T) {
	taskStore, err := task.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(taskStore, nil)

	projectStore, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("project.NewStore: %v", err)
	}

	logger := slog.New(slog.DiscardHandler)
	worktrees := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Projects:     projectStore,
		Tasks:        tasks,
		Logger:       logger,
	})

	svc := &IntegrationService{
		tasks:     tasks,
		worktrees: worktrees,
		logger:    logger,
	}

	// "owner/does-not-exist" is never registered with projectStore, so
	// PrepareForFix's project lookup fails deterministically without needing
	// a real git clone — mirroring any worktree-prepare failure that isn't
	// itself a setup-command failure (those are now non-gating, see
	// internal/worktree/setup.go's runSetupNonGating).
	err = svc.FixRenovateCI("owner/does-not-exist", 42, "some-branch", "bump a dependency")
	if err == nil {
		t.Fatal("expected FixRenovateCI to return an error when worktree prepare fails")
	}

	tasksList, err := tasks.List()
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(tasksList) != 1 {
		t.Fatalf("expected exactly 1 task created, got %d", len(tasksList))
	}
	got := tasksList[0]
	if got.Status != task.StatusHumanRequired {
		t.Errorf("task status = %q, want %q — task must not be silently stranded", got.Status, task.StatusHumanRequired)
	}
	if got.StatusReason == "" {
		t.Error("expected a non-empty status reason explaining the escalation")
	}
}
