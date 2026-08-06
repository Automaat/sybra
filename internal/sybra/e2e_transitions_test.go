//go:build e2e

package sybra

import (
	"errors"
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

// TestE2E_TransitionTable_RejectsBackwardsMoves exercises the real
// filesystem-backed task.Manager (the same store/manager wiring production
// uses) to prove the two backwards moves named in #2751's acceptance
// criteria are rejected end to end: done -> in-progress and
// in-review -> todo. It also proves a legal forward move and the operator
// override path both still succeed, so the table isn't merely closed —
// it's closed exactly where intended and open everywhere else.
func TestE2E_TransitionTable_RejectsBackwardsMoves(t *testing.T) {
	env := setupE2E(t, "success")

	t.Run("done to in-progress rejected", func(t *testing.T) {
		created, err := env.tasks.Create("done task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusDone)}); err != nil {
			t.Fatal(err)
		}

		_, err = env.tasks.Apply(task.TransitionIntent{
			TaskID:   created.ID,
			ToStatus: task.StatusInProgress,
			Actor:    "test.automation",
		})
		var illegal *task.IllegalTransitionError
		if !errors.As(err, &illegal) {
			t.Fatalf("Apply err = %v, want *IllegalTransitionError", err)
		}
		if !errors.Is(err, task.ErrIllegalTransition) {
			t.Fatal("errors.Is(err, ErrIllegalTransition) = false, want true")
		}

		after, err := env.tasks.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != task.StatusDone {
			t.Fatalf("status after rejected transition = %q, want unchanged %q", after.Status, task.StatusDone)
		}
	})

	t.Run("in-review to todo rejected", func(t *testing.T) {
		created, err := env.tasks.Create("in-review task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
			t.Fatal(err)
		}

		_, err = env.tasks.Apply(task.TransitionIntent{
			TaskID:   created.ID,
			ToStatus: task.StatusTodo,
			Actor:    "test.automation",
		})
		if !errors.Is(err, task.ErrIllegalTransition) {
			t.Fatalf("Apply err = %v, want ErrIllegalTransition", err)
		}

		after, err := env.tasks.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status != task.StatusInReview {
			t.Fatalf("status after rejected transition = %q, want unchanged %q", after.Status, task.StatusInReview)
		}
	})

	t.Run("legal forward move succeeds", func(t *testing.T) {
		created, err := env.tasks.Create("legal move task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}

		result, err := env.tasks.Apply(task.TransitionIntent{
			TaskID:   created.ID,
			ToStatus: task.StatusInProgress,
			Actor:    "test.automation",
		})
		if err != nil {
			t.Fatalf("Apply legal move: %v", err)
		}
		if result.Task.Status != task.StatusInProgress {
			t.Fatalf("status = %q, want %q", result.Task.Status, task.StatusInProgress)
		}
	})

	t.Run("operator override reopens a terminal task", func(t *testing.T) {
		created, err := env.tasks.Create("reopen task", "", "headless")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.tasks.Update(created.ID, task.Update{Status: task.Ptr(task.StatusCancelled)}); err != nil {
			t.Fatal(err)
		}

		result, err := env.tasks.Apply(task.TransitionIntent{
			TaskID:           created.ID,
			ToStatus:         task.StatusTodo,
			Actor:            "cli.reopen",
			OperatorOverride: true,
		})
		if err != nil {
			t.Fatalf("Apply with OperatorOverride: %v", err)
		}
		if result.Task.Status != task.StatusTodo {
			t.Fatalf("status = %q, want %q", result.Task.Status, task.StatusTodo)
		}
	})
}
