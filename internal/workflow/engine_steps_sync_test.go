package workflow

import (
	"context"
	"errors"
	"testing"
)

// fakeBranchSyncer is a scripted BranchSyncer for execSyncBranch tests.
type fakeBranchSyncer struct {
	result string
	err    error
	panics bool
}

func (f *fakeBranchSyncer) SyncTaskBranch(context.Context, string) (string, error) {
	if f.panics {
		panic("boom")
	}
	return f.result, f.err
}

func newSyncBranchStep() *Step { return &Step{ID: "sync_branch", Type: StepSyncBranch} }

func newSyncBranchEngine(t *testing.T) (*Engine, *memTasks) {
	t.Helper()
	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	return engine, engine.tasks.(*memTasks)
}

func TestExecSyncBranch_NoSyncerSkips(t *testing.T) {
	t.Parallel()
	engine, tasks := newSyncBranchEngine(t)
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})

	out, err := engine.execSyncBranch("t1", newSyncBranchStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if out.Output != "skipped" {
		t.Errorf("Output = %q, want skipped", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "ready-pr" {
		t.Errorf("status = %q, want unchanged", ti.Status)
	}
}

func TestExecSyncBranch_Success(t *testing.T) {
	t.Parallel()
	engine, tasks := newSyncBranchEngine(t)
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine.SetBranchSyncer(&fakeBranchSyncer{result: "synced"})

	out, err := engine.execSyncBranch("t1", newSyncBranchStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if out.Output != "synced" {
		t.Errorf("Output = %q, want synced", out.Output)
	}
}

func TestExecSyncBranch_NoopIsCompleted(t *testing.T) {
	t.Parallel()
	engine, tasks := newSyncBranchEngine(t)
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine.SetBranchSyncer(&fakeBranchSyncer{result: "noop"})

	out, err := engine.execSyncBranch("t1", newSyncBranchStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Output != "noop" {
		t.Errorf("Output = %q, want noop", out.Output)
	}
}

func TestExecSyncBranch_ConflictDoesNotBlock(t *testing.T) {
	t.Parallel()
	engine, tasks := newSyncBranchEngine(t)
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine.SetBranchSyncer(&fakeBranchSyncer{result: "conflict", err: errors.New("rebase conflict")})

	out, err := engine.execSyncBranch("t1", newSyncBranchStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed (never blocks)", out.Status)
	}
	if out.Output != "conflict" {
		t.Errorf("Output = %q, want conflict", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "ready-pr" {
		t.Errorf("status = %q, want unchanged — sync_branch must never escalate to human-required", ti.Status)
	}
}

func TestExecSyncBranch_FailedDoesNotBlock(t *testing.T) {
	t.Parallel()
	engine, tasks := newSyncBranchEngine(t)
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine.SetBranchSyncer(&fakeBranchSyncer{result: "failed", err: errors.New("network timeout")})

	out, err := engine.execSyncBranch("t1", newSyncBranchStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if out.Output != "failed" {
		t.Errorf("Output = %q, want failed", out.Output)
	}
}

func TestExecSyncBranch_PanicIsContained(t *testing.T) {
	t.Parallel()
	engine, tasks := newSyncBranchEngine(t)
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine.SetBranchSyncer(&fakeBranchSyncer{panics: true})

	out, err := engine.execSyncBranch("t1", newSyncBranchStep())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed — a syncer panic must not crash workflow advancement", out.Status)
	}
	if out.Output != "failed" {
		t.Errorf("Output = %q, want failed", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "ready-pr" {
		t.Errorf("status = %q, want unchanged", ti.Status)
	}
}
