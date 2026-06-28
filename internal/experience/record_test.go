package experience

import (
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
)

func TestFromTaskDeterministic(t *testing.T) {
	closed := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	tk := task.Task{
		ID:        "task-1",
		Title:     "Ship feature",
		Tags:      []string{"backend"},
		TaskType:  task.TaskTypeNormal,
		AgentMode: task.AgentModeHeadless,
		ProjectID: "owner/repo",
		Outcome:   "merged",
		ClosedAt:  &closed,
		PlanBrief: "Use the narrow path.",
		AgentRuns: []task.AgentRun{
			{Provider: "claude", TestOutcome: "product_bug", TestFailureFingerprint: "abc"},
			{Provider: "codex"},
		},
	}
	proj := project.Project{
		ID:    "owner/repo",
		Owner: "owner",
		Repo:  "repo",
		Type:  project.ProjectTypePet,
		Checks: &project.ChecksConfig{
			Verify: []string{"go test ./..."},
		},
	}

	got := FromTask(tk, proj)
	if got.TaskID != "task-1" || got.ProjectID != "owner/repo" || got.ProjectType != "pet" {
		t.Fatalf("unexpected identity fields: %+v", got)
	}
	if !got.CreatedAt.Equal(closed) {
		t.Fatalf("CreatedAt = %s, want %s", got.CreatedAt, closed)
	}
	if got.Provider != "codex" || got.Attempts != 2 {
		t.Fatalf("provider/attempts = %q/%d, want codex/2", got.Provider, got.Attempts)
	}
	if len(got.VerifyCommands) != 1 || got.VerifyCommands[0] != "go test ./..." {
		t.Fatalf("VerifyCommands = %+v", got.VerifyCommands)
	}
	if len(got.FailureModes) != 2 {
		t.Fatalf("FailureModes = %+v, want test outcome and fingerprint", got.FailureModes)
	}
	tk.Tags[0] = "mutated"
	if got.Tags[0] != "backend" {
		t.Fatal("FromTask aliased task tags")
	}
}

func TestFormatForPrompt(t *testing.T) {
	if got := FormatForPrompt(nil); got != "" {
		t.Fatalf("FormatForPrompt(nil) = %q, want empty", got)
	}
	got := FormatForPrompt([]Record{{
		TaskID:         "task-1",
		ProjectType:    "pet",
		Title:          "Ship feature",
		Outcome:        "merged",
		Tags:           []string{"backend"},
		Strategy:       "Run tests\nthen inspect.",
		VerifyCommands: []string{"go test ./..."},
		Caution:        "Keep it scoped.",
	}})
	for _, want := range []string{"Verified Experience Memory", "Advisory only", "task-1", "Ship feature", "go test ./...", "Run tests then inspect."} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted prompt missing %q:\n%s", want, got)
		}
	}
}
