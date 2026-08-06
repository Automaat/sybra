package review

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

// TestParkOrEscalateBranchFixFailure_BusyPathDoesNotCountAsFailure pins that a
// worktree refused only because another mutating operation owns the path never
// reaches the worktree-failure circuit breaker. Without this, wtFailureLimit
// polls against a task whose own preparation is still running escalate it to
// human-required for a condition that clears by itself.
func TestParkOrEscalateBranchFixFailure_BusyPathDoesNotCountAsFailure(t *testing.T) {
	for _, tt := range []struct {
		name  string
		wtErr error
	}{
		{"preparation in flight", fmt.Errorf("prepare: %w", worktree.ErrPreparationInFlight)},
		{"agent running", fmt.Errorf("prepare: %w", worktree.ErrAgentRunning)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tasks, _, _ := newRebaseRecoveryDeps(t)
			r := &Handler{
				logger:     slog.New(slog.DiscardHandler),
				tasks:      tasks,
				wtFailures: make(map[string]int),
			}
			tk, err := tasks.Create("busy wt", "", task.AgentModeHeadless)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
				t.Fatal(err)
			}

			for i := range wtFailureLimit + 2 {
				if parked := r.parkOrEscalateBranchFixFailure(tk.ID, tt.wtErr); !parked {
					t.Fatalf("attempt %d: parked = false, want true", i)
				}
			}

			if n := r.wtFailures[tk.ID]; n != 0 {
				t.Errorf("recorded worktree failures = %d, want 0", n)
			}
			got, err := tasks.Get(tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status == task.StatusHumanRequired {
				t.Errorf("task escalated to human-required on a self-clearing refusal (reason %q)", got.StatusReason)
			}
		})
	}
}
