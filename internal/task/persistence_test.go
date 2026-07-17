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
	sandbox := false
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
		RefIssue:               "https://github.com/owner/repo/issues/9",
		StatusReason:           "testing",
		HandoffSourceProvider:  "codex",
		BlockedByIssue:         "https://github.com/owner/repo/issues/2",
		UmbrellaIssue:          "owner/repo#3",
		DependsOn:              []string{"owner/repo#4"},
		Reviewed:               true,
		RunRole:                "pr-fix",
		SupervisorSteer:        "read the failure",
		ReviewPhase:            "awaiting-author",
		ReviewedHeadSHA:        "e57e4b5db72c55ba7610140631a80946a7edddf0",
		ReviewedHeadAttempts:   2,
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
		Sandbox:                &sandbox,
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
			EscalationReason:       "cost",
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
		CreatedAt:       now.Add(-time.Hour),
		UpdatedAt:       now,
		AssignedNode:    "pet-box",
		NodeOverride:    "gpu-box",
		MirrorRev:       7,
		MirrorUpdatedAt: &completedAt,
		Body:            "body",
	}

	got := taskFromFrontmatter(frontmatterFromTask(original), original.Body)
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("frontmatter mapping mismatch\n got: %#v\nwant: %#v", got, original)
	}
}

// persistedFields returns the leaf (non-anonymous) fields of a struct type,
// recursing into embedded fields so a Task built from feature-cluster
// sub-structs (e.g. `ReviewState`) is still checked field-by-field rather
// than by its container name. Unlike Type.Fields()/NumField(), which only
// walk the direct field list, reflect.VisibleFields flattens promoted fields
// from anonymous embeds.
func persistedFields(typ reflect.Type) []reflect.StructField {
	var fields []reflect.StructField
	for _, field := range reflect.VisibleFields(typ) {
		if field.Anonymous {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

func TestTaskFrontmatterMappingCoversPersistedFields(t *testing.T) {
	t.Parallel()
	taskType := reflect.TypeFor[Task]()
	frontmatterType := reflect.TypeFor[taskFrontmatter]()

	for _, field := range persistedFields(taskType) {
		if taskSidecarField(field.Name) {
			continue
		}
		if _, ok := frontmatterType.FieldByName(field.Name); !ok {
			t.Errorf("Task.%s is persisted but missing from taskFrontmatter", field.Name)
		}
	}
	for _, field := range persistedFields(frontmatterType) {
		if _, ok := taskType.FieldByName(field.Name); !ok {
			t.Errorf("taskFrontmatter.%s has no matching Task field", field.Name)
		}
	}
}

func TestTaskFrontmatterMappingPreservesEachPersistedField(t *testing.T) {
	t.Parallel()
	taskType := reflect.TypeFor[Task]()

	for _, field := range persistedFields(taskType) {
		if taskSidecarField(field.Name) {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			original := Task{}
			setTaskFieldForPersistenceTest(t, &original, field.Name)

			got := taskFromFrontmatter(frontmatterFromTask(original), "")
			if !reflect.DeepEqual(reflect.ValueOf(got).FieldByName(field.Name).Interface(), reflect.ValueOf(original).FieldByName(field.Name).Interface()) {
				t.Fatalf("Task.%s did not survive frontmatter mapping\n got: %#v\nwant: %#v", field.Name, reflect.ValueOf(got).FieldByName(field.Name).Interface(), reflect.ValueOf(original).FieldByName(field.Name).Interface())
			}
		})
	}
}

func TestPersistenceTypesHaveYAMLTags(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{reflect.TypeFor[taskFrontmatter](), reflect.TypeFor[agentRunRecord]()} {
		for field := range typ.Fields() {
			if tag := field.Tag.Get("yaml"); tag == "" {
				t.Errorf("%s.%s is missing a yaml tag", typ.Name(), field.Name)
			}
		}
	}
}

func taskSidecarField(name string) bool {
	switch name {
	case "Body", "Plan", "PlanContract", "PlanCritique", "PlanResearch", "PlanDecisions", "PlanBrief", "CodeReview", "PlanDrafts", "FilePath", "TamperFlagged":
		return true
	default:
		return false
	}
}

func setTaskFieldForPersistenceTest(t *testing.T, task *Task, name string) {
	t.Helper()
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	falseValue := false
	later := now.Add(time.Hour)
	completedAt := now.Add(2 * time.Hour)

	switch name {
	case "ID":
		task.ID = "task-persist"
	case "Slug":
		task.Slug = "task-persist-slug"
	case "Title":
		task.Title = "Task persistence title"
	case "Status":
		task.Status = StatusTesting
	case "TaskType":
		task.TaskType = TaskTypeResearch
	case "AgentMode":
		task.AgentMode = AgentModeHeadless
	case "AllowedTools":
		task.AllowedTools = []string{"Read", "Write"}
	case "Tags":
		task.Tags = []string{"backend", "review"}
	case "ProjectID":
		task.ProjectID = "owner/repo"
	case "Branch":
		task.Branch = "feature/task-persist"
	case "WorktreeDir":
		task.WorktreeDir = "/tmp/task-persist"
	case "PRNumber":
		task.PRNumber = 123
	case "Issue":
		task.Issue = "owner/repo#123"
	case "RefIssue":
		task.RefIssue = "owner/repo#999"
	case "StatusReason":
		task.StatusReason = "testing"
	case "HandoffSourceProvider":
		task.HandoffSourceProvider = "codex"
	case "BlockedByIssue":
		task.BlockedByIssue = "owner/repo#456"
	case "UmbrellaIssue":
		task.UmbrellaIssue = "owner/repo#789"
	case "DependsOn":
		task.DependsOn = []string{"owner/repo#321"}
	case "Reviewed":
		task.Reviewed = true
	case "RunRole":
		task.RunRole = "pr-fix"
	case "SupervisorSteer":
		task.SupervisorSteer = "read the failure"
	case "ReviewPhase":
		task.ReviewPhase = "awaiting-author"
	case "ReviewedHeadSHA":
		task.ReviewedHeadSHA = "e57e4b5db72c55ba7610140631a80946a7edddf0"
	case "ReviewedHeadAttempts":
		task.ReviewedHeadAttempts = 2
	case "PRPhase":
		task.PRPhase = "fixing"
	case "TodoistID":
		task.TodoistID = "todoist-123"
	case "Priority":
		task.Priority = PriorityHigh
	case "DueDate":
		task.DueDate = &later
	case "ClosedAt":
		task.ClosedAt = &later
	case "Outcome":
		task.Outcome = "merged"
	case "MergeCommit":
		task.MergeCommit = "abc123"
	case "MaxTurns":
		task.MaxTurns = 7
	case "RequirePermissions":
		task.RequirePermissions = &falseValue
	case "HeadlessPermissionMode":
		task.HeadlessPermissionMode = "auto"
	case "ForkSubagent":
		task.ForkSubagent = true
	case "Sandbox":
		task.Sandbox = &falseValue
	case "ReasoningEffort":
		task.ReasoningEffort = "xhigh"
	case "TestingCycleStartedAt":
		task.TestingCycleStartedAt = &later
	case "AgentRuns":
		task.AgentRuns = []AgentRun{{
			AgentID:                "agent-1",
			Role:                   "test-runner",
			Mode:                   AgentModeHeadless,
			Provider:               "claude",
			Model:                  "model-a",
			ExperimentID:           "exp",
			VariantID:              "variant",
			AssignmentUnit:         "task",
			AssignmentKey:          "task-persist",
			ReasoningEffort:        "high",
			State:                  "done",
			EscalationReason:       "cost",
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
		}}
	case "Workflow":
		task.Workflow = &workflow.Execution{
			WorkflowID:  "workflow-1",
			CurrentStep: "step-1",
			State:       workflow.ExecRunning,
			StartedAt:   now,
			CompletedAt: &completedAt,
			Variables:   map[string]string{"k": "v"},
		}
	case "CreatedAt":
		task.CreatedAt = now
	case "UpdatedAt":
		task.UpdatedAt = later
	case "StatusChangedAt":
		task.StatusChangedAt = later
	case "AssignedNode":
		task.AssignedNode = "pet-box"
	case "NodeOverride":
		task.NodeOverride = "gpu-box"
	case "MirrorRev":
		task.MirrorRev = 7
	case "MirrorUpdatedAt":
		task.MirrorUpdatedAt = &later
	default:
		t.Fatalf("no persistence test value for Task.%s", name)
	}
}
