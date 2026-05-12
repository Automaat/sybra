package worktree

import (
	"testing"

	"github.com/Automaat/sybra/internal/task"
)

func TestBranchNameForTask_UsesConventionalTitlePrefix(t *testing.T) {
	t.Parallel()
	tk := task.Task{
		ID:    "fa6919fc",
		Slug:  "add-auth",
		Title: "feat(api): add auth endpoint",
	}

	got := branchNameForTask(tk)
	want := "feat/add-auth-fa6919fc"
	if got != want {
		t.Fatalf("branchNameForTask = %q, want %q", got, want)
	}
}

func TestBranchNameForTask_PreservesExistingBranch(t *testing.T) {
	t.Parallel()
	tk := task.Task{
		ID:     "fa6919fc",
		Slug:   "add-auth",
		Title:  "feat(api): add auth endpoint",
		Branch: "sybra/add-auth-fa6919fc",
	}

	got := branchNameForTask(tk)
	want := "sybra/add-auth-fa6919fc"
	if got != want {
		t.Fatalf("branchNameForTask = %q, want %q", got, want)
	}
}

func TestBranchPrefixForTask_Fallbacks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		task task.Task
		want string
	}{
		{
			name: "debug",
			task: task.Task{TaskType: task.TaskTypeDebug, Title: "crash on start"},
			want: "fix",
		},
		{
			name: "research",
			task: task.Task{TaskType: task.TaskTypeResearch, Title: "compare providers"},
			want: "chore",
		},
		{
			name: "normal",
			task: task.Task{TaskType: task.TaskTypeNormal, Title: "update copy"},
			want: "chore",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := branchPrefixForTask(tt.task); got != tt.want {
				t.Fatalf("branchPrefixForTask = %q, want %q", got, tt.want)
			}
		})
	}
}
