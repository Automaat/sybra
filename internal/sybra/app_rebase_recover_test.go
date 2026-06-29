package sybra

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestMarkRebaseBlocked_RecoversInsteadOfEscalating verifies that a
// worktree-prep rebase conflict is handed to the conflict-recovery callback
// (the conflict pr-fix) and, when that succeeds, the task is NOT stranded in
// human-required. This is the fix for PRs that have both a merge conflict and a
// failing check: the CI-fix rebase aborts on the conflict, but the conflict is
// recoverable autonomously.
func TestMarkRebaseBlocked_RecoversInsteadOfEscalating(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("rebase recover", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	recoverCalls := 0
	recoverFn := func(id string) bool {
		if id != tk.ID {
			t.Errorf("recover got id %q, want %q", id, tk.ID)
		}
		recoverCalls++
		return true
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(a.tasks, tk.ID, rebaseErr, a.logger, recoverFn) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}
	if recoverCalls != 1 {
		t.Fatalf("recover called %d times, want 1", recoverCalls)
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == task.StatusHumanRequired {
		t.Errorf("task escalated to human-required despite successful recovery")
	}
}

// TestMarkRebaseBlocked_EscalatesWhenRecoveryDeclines verifies the fallback:
// when there is no recoverable PR (callback returns false), the task is parked
// in human-required as before.
func TestMarkRebaseBlocked_EscalatesWhenRecoveryDeclines(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("rebase escalate", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(a.tasks, tk.ID, rebaseErr, a.logger, func(string) bool { return false }) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required when recovery declines", got.Status)
	}
}

// TestMarkRebaseBlocked_NilRecoverEscalates verifies that a nil recovery
// callback (call sites without a PR-monitor handle) keeps the old behaviour.
func TestMarkRebaseBlocked_NilRecoverEscalates(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("rebase nil recover", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	rebaseErr := fmt.Errorf("rebase x onto main: %w", worktree.ErrRebaseFailed)
	if !markRebaseBlocked(a.tasks, tk.ID, rebaseErr, a.logger, nil) {
		t.Fatal("markRebaseBlocked returned false for a rebase failure")
	}
	got, err := a.tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required with nil recovery", got.Status)
	}
}

// TestMarkRebaseBlocked_IgnoresNonRebaseError verifies non-rebase errors are
// not handled here (returns false so the caller's own error path runs) and the
// recovery callback is never consulted.
func TestMarkRebaseBlocked_IgnoresNonRebaseError(t *testing.T) {
	a := setupApp(t)
	tk, err := a.tasks.Create("non rebase", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	called := false
	if markRebaseBlocked(a.tasks, tk.ID, errors.New("boom"), a.logger, func(string) bool { called = true; return true }) {
		t.Error("markRebaseBlocked returned true for a non-rebase error")
	}
	if called {
		t.Error("recovery callback consulted for a non-rebase error")
	}
}
