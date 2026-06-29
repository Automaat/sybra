package task

import (
	"reflect"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/workflow"
)

func TestTaskFrontmatterMappingRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	closedAt := now.Add(time.Hour)
	dueDate := now.Add(24 * time.Hour)
	requirePermissions := false
	testingCycleStartedAt := now.Add(2 * time.Hour)
	completedAt := now.Add(3 * time.Hour)
	original := Task{
		ID:                     "task1234",
		Slug:                   "task-slug",
		Title:                  "Task title",
		Status:                 StatusTesting,
		TaskType:               TaskTypeResearch,
		AgentMode:              AgentModeHeadless,
		AllowedTools:           []string{"Read", "Write"},
		Tags:                   []string{"backend", "refactor"},
		ProjectID:              "owner/repo",
		Branch:                 "feature/task",
		WorktreeDir:            "/tmp/worktree",
		PRNumber:               42,
		Issue:                  "https://github.com/owner/repo/issues/1",
		StatusReason:           "testing",
		HandoffSourceProvider:  "codex",
		BlockedByIssue:         "https://github.com/owner/repo/issues/2",
		UmbrellaIssue:          "owner/repo#3",
		DependsOn:              []string{"owner/repo#4"},
		Reviewed:               true,
		RunRole:                "pr-fix",
		SupervisorSteer:        "read the failure",
		ReviewPhase:            "awaiting-author",
		PRPhase:                "fixing",
		TodoistID:              "todoist-1",
		Priority:               PriorityHigh,
		DueDate:                &dueDate,
		ClosedAt:               &closedAt,
		Outcome:                "merged",
		MergeCommit:            "abc123",
		MaxTurns:               7,
		RequirePermissions:     &requirePermissions,
		HeadlessPermissionMode: "auto",
		ForkSubagent:           true,
		ReasoningEffort:        "xhigh",
		TestingCycleStartedAt:  &testingCycleStartedAt,
		AgentRuns: []AgentRun{{
			AgentID:                "agent-1",
			Role:                   "test-runner",
			Mode:                   AgentModeHeadless,
			Provider:               "claude",
			Model:                  "model-a",
			ExperimentID:           "exp",
			VariantID:              "variant",
			AssignmentUnit:         "task",
			AssignmentKey:          "task1234",
			ReasoningEffort:        "high",
			State:                  "done",
			StartedAt:              now,
			CostUSD:                1.25,
			PremiumRequests:        2.5,
			Prompt:                 "prompt",
			Result:                 "result",
			OneShot:                true,
			Verdict:                "human",
			VerdictRendered:        true,
			LogFile:                "/tmp/log",
			SessionID:              "session-1",
			ProtocolViolation:      "bad-report",
			TestOutcome:            "product_bug",
			TestFailureFingerprint: "fp",
			HeadSHA:                "def456",
		}},
		Workflow: &workflow.Execution{
			WorkflowID:  "workflow-1",
			CurrentStep: "step-1",
			State:       workflow.ExecRunning,
			StartedAt:   now,
			CompletedAt: &completedAt,
			Variables:   map[string]string{"k": "v"},
		},
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now,
		Body:      "body",
	}

	got := taskFromFrontmatter(frontmatterFromTask(original), original.Body)
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("frontmatter mapping mismatch\n got: %#v\nwant: %#v", got, original)
	}
}
