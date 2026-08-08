package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/promptlab"
	"github.com/Automaat/sybra/internal/task"
)

// The CLI no longer classifies: it calls the server, which owns the classifier
// and the retryable stamp a failure leaves behind. That guarantee is pinned in
// internal/sybra's TestClassifyTask_StampsARetryableReasonOnFailure, which
// replaced the local reimplementation this file used to cover.

// TestCmdTriageClassifyRefusesNonNewTask locks in the single-id `triage
// classify <id>` entry point matching the status=new filter the --all path
// and the poll-based auto-triage handler already apply. Without this guard,
// classifying an arbitrary id can reclassify a task outside the triage
// pipeline's ownership — e.g. a human-required Prompt Lab proposal whose
// gating tags/status must survive untouched (see promptlab.ProposalTag).
func TestCmdTriageClassifyRefusesNonNewTask(t *testing.T) {
	dir := t.TempDir()
	store, err := task.NewStore(filepath.Join(dir, "tasks"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	mgr := task.NewManager(store, nil)
	created, err := mgr.Create("pending human decision", "body", task.AgentModeHeadless)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	humanRequired := task.StatusHumanRequired
	tags := []string{promptlab.ProposalTag, "requires-human"}
	created, err = mgr.Update(created.ID, task.Update{
		Status:          &humanRequired,
		Tags:            &tags,
		Escalation:      task.OperatorDecisionEvidence("test.fixture_human_required", "test fixture"),
		AutonomyOutcome: task.HumanRequiredOutcome(),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	t.Setenv("SYBRA_HOME", dir)
	cfg := config.DefaultConfig()

	// The board the CLI reads its status from, so the guard sees what the
	// owning instance holds rather than a second reader's copy.
	board := newAPITaskBoard(newTestBoardClient(t, dir))
	code, _ := captureStdout(t, func() int {
		return cmdTriageClassify(cfg, nil, board, []string{created.ID}, true)
	})
	if code == 0 {
		t.Fatalf("expected non-zero exit classifying a non-new task")
	}

	got, err := mgr.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %s, want unchanged human-required", got.Status)
	}
	if !slices.Equal(got.Tags, tags) {
		t.Errorf("tags = %v, want unchanged %v", got.Tags, tags)
	}
}
