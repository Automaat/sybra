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

// TestBranchNameForTask_NoCollisionAcrossTaskIDs guards the assumption
// PrepareForTask relies on: two tasks with identical titles/slugs (so
// identical branchPrefixForTask + DirName inputs modulo ID) must still
// resolve to distinct branches, since the task ID is the only differentiator
// once the branch name is derived rather than explicitly set. Task IDs come
// from a uuid[:8] (see task.Store.Create) — mustn't collide.
func TestBranchNameForTask_NoCollisionAcrossTaskIDs(t *testing.T) {
	t.Parallel()
	base := task.Task{Slug: "add-auth", Title: "feat(api): add auth endpoint"}

	seen := make(map[string]string)
	ids := []string{"fa6919fc", "0b6919fc", "fa6919fd", "aaaaaaaa", "bbbbbbbb"}
	for _, id := range ids {
		tk := base
		tk.ID = id
		branch := branchNameForTask(tk)
		if other, ok := seen[branch]; ok {
			t.Fatalf("branch %q derived for both task ID %q and %q", branch, other, id)
		}
		seen[branch] = id
	}
}

// TestBranchNameForTask_DeterministicPerTaskID ensures the derivation is
// stable across repeated calls (e.g. a reused worktree recomputing the branch
// on every PrepareForTask call must land back on the same branch, not drift).
func TestBranchNameForTask_DeterministicPerTaskID(t *testing.T) {
	t.Parallel()
	tk := task.Task{ID: "fa6919fc", Slug: "add-auth", Title: "feat(api): add auth endpoint"}

	first := branchNameForTask(tk)
	for range 5 {
		if got := branchNameForTask(tk); got != first {
			t.Fatalf("branchNameForTask is non-deterministic: got %q, want %q", got, first)
		}
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
