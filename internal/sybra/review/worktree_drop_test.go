package review

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/worktree"
)

func TestDropTerminalWorktreeFailure(t *testing.T) {
	tests := []struct {
		name         string
		wtErr        error
		deleteTask   bool
		wantTerminal bool
		wantHumanReq bool
		wantSkipNext bool
	}{
		{
			name:         "missing task branch is terminal and escalates",
			wtErr:        worktree.ErrTaskBranchMissing,
			wantTerminal: true,
			wantHumanReq: true,
		},
		{
			name:         "deleted base ref is terminal",
			wtErr:        errors.New("create fix worktree: git worktree add x refs/remotes/origin/feat/x → exit 128: fatal: invalid reference"),
			wantTerminal: true,
			wantHumanReq: true,
		},
		{
			name:         "absent task record is dropped from the monitor set",
			wtErr:        errors.New("create fix worktree: boom"),
			deleteTask:   true,
			wantTerminal: true,
			wantSkipNext: true,
		},
		{
			name:         "transient failure is not terminal and keeps retrying",
			wtErr:        errors.New("worktree setup failed: temporary lock"),
			wantTerminal: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasks, _, _ := newRebaseRecoveryDeps(t)
			r := &Handler{
				logger:     slog.New(slog.DiscardHandler),
				tasks:      tasks,
				wtFailures: make(map[string]int),
			}
			tk, err := tasks.Create("terminal wt", "", task.AgentModeHeadless)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
				t.Fatal(err)
			}
			if tt.deleteTask {
				if err := tasks.Delete(tk.ID); err != nil {
					t.Fatal(err)
				}
			}

			if got := r.dropTerminalWorktreeFailure(tk.ID, tt.wtErr); got != tt.wantTerminal {
				t.Fatalf("dropTerminalWorktreeFailure = %v, want %v", got, tt.wantTerminal)
			}

			if tt.wantHumanReq {
				got, err := tasks.Get(tk.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != task.StatusHumanRequired {
					t.Fatalf("status = %q, want human-required", got.Status)
				}
				if n := r.wtFailures[tk.ID]; n != 0 {
					t.Fatalf("wtFailures = %d, want 0 for a terminal failure", n)
				}
			}

			if got := r.worktreeSkip(tk.ID); got != tt.wantSkipNext {
				t.Fatalf("worktreeSkip = %v, want %v", got, tt.wantSkipNext)
			}
		})
	}
}

func TestTransientWorktreeFailureBoundedByCircuitBreaker(t *testing.T) {
	tasks, _, _ := newRebaseRecoveryDeps(t)
	r := &Handler{
		logger:     slog.New(slog.DiscardHandler),
		tasks:      tasks,
		wtFailures: make(map[string]int),
	}
	tk, err := tasks.Create("transient wt", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInReview)}); err != nil {
		t.Fatal(err)
	}

	wtErr := errors.New("worktree setup failed: temporary lock")
	for i := 1; i < wtFailureLimit; i++ {
		if r.dropTerminalWorktreeFailure(tk.ID, wtErr) {
			t.Fatalf("attempt %d: transient failure must not be terminal", i)
		}
		if got := r.recordWorktreeFailure(tk.ID, wtErr); got != i {
			t.Fatalf("attempt %d: recordWorktreeFailure = %d, want %d", i, got, i)
		}
		got, err := tasks.Get(tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == task.StatusHumanRequired {
			t.Fatalf("attempt %d: escalated before the circuit limit", i)
		}
	}

	if r.dropTerminalWorktreeFailure(tk.ID, wtErr) {
		t.Fatal("circuit-limit attempt: transient failure must not be terminal")
	}
	r.recordWorktreeFailure(tk.ID, wtErr)
	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required after the circuit trips", got.Status)
	}
	if n := r.wtFailures[tk.ID]; n != 0 {
		t.Fatalf("wtFailures = %d, want 0 after the circuit trips", n)
	}
}
