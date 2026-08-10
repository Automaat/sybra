package agent

import (
	"testing"

	"github.com/Automaat/sybra/internal/providerid"
)

func TestAttemptIntentForRunUsesStableAdmissionWorktree(t *testing.T) {
	const canonical = "/tmp/worktrees/task"
	const disposable = "/tmp/verification/runs/first/source"
	intent := attemptIntentForRun(RunConfig{
		IntentID: "task:review:effect", TaskID: "task", Role: RoleReview,
		Dir: disposable, AdmissionWorktree: canonical,
	}, providerid.Codex)
	if intent.Worktree != canonical {
		t.Fatalf("Worktree = %q, want canonical admission worktree %q", intent.Worktree, canonical)
	}
}

func TestAttemptIntentFromRecordPreservesAdmissionWorktree(t *testing.T) {
	const canonical = "/tmp/worktrees/task"
	const disposable = "/tmp/verification/runs/first/source"
	record := Record{
		ID: "agent", TaskID: "task", Role: RoleReview, Provider: providerid.Codex,
		AttemptIntentID: "task:review:effect", AttemptTaskKey: "task",
		AttemptWorktree: canonical, CWD: disposable,
	}
	if got := attemptIntentFromRecord(record).Worktree; got != canonical {
		t.Fatalf("attemptIntentFromRecord Worktree = %q, want %q", got, canonical)
	}
	if got := fromRecord(record).attemptIntent.Worktree; got != canonical {
		t.Fatalf("fromRecord Worktree = %q, want %q", got, canonical)
	}
}
