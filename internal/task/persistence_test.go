package task

import (
	"reflect"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
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
		TaskType:               TaskTypeUmbrella,
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
		DependsOnConditions:    []DepCondition{{Ref: "owner/repo#4", Kind: DepConditionKindNote, Value: "confirm permutation coverage"}},
		Reviewed:               true,
		RunRole:                "pr-fix",
		SupervisorSteer:        "read the failure",
		ReviewPhase:            "awaiting-author",
		ReviewedHeadSHA:        "e57e4b5db72c55ba7610140631a80946a7edddf0",
		ReviewedHeadAttempts:   2,
		ReconcileFailures:      3,
		PRPhase:                "fixing",
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
		SandboxOffReason:       "host mounts required for docker-in-docker e2e",
		ReasoningEffort:        "xhigh",
		TestingCycleStartedAt:  &testingCycleStartedAt,
		Attachments: []Attachment{{
			ID:          "att-1",
			FileName:    "evidence.txt",
			ContentType: "text/plain",
			SizeBytes:   5,
			Path:        "/tmp/attachments/task1234/att-1/evidence.txt",
			CreatedAt:   now.Add(-30 * time.Minute),
		}},
		AgentRuns: []AgentRun{{
			AgentID:                 "agent-1",
			Role:                    "test-runner",
			Mode:                    AgentModeHeadless,
			Provider:                "claude",
			Model:                   "model-a",
			ExperimentID:            "exp",
			VariantID:               "variant",
			AssignmentUnit:          "task",
			AssignmentKey:           "task1234",
			DecisionVersion:         7,
			ReasoningEffort:         "high",
			RequestedSkill:          "sybra-test",
			SkillExecutionMode:      "native",
			ResolvedSkillSourceHash: "deadbeefcafebabe",
			SkillConformance:        "exact",
			State:                   "done",
			EscalationReason:        "cost",
			StartedAt:               now,
			CostUSD:                 1.25,
			PremiumRequests:         2.5,
			Prompt:                  "prompt",
			Result:                  "result",
			OneShot:                 true,
			Verdict:                 "human",
			VerdictRendered:         true,
			LogFile:                 "/tmp/log",
			SessionID:               "session-1",
			ProtocolViolation:       "bad-report",
			TestOutcome:             "product_bug",
			TestFailureFingerprint:  "fp",
			HeadSHA:                 "def456",
			SubagentCallCount:       2,
		}},
		EffectLog: []workflow.EffectRecord{{
			ID: workflow.EffectID{
				Generation: 2,
				StepSeq:    4,
				StepID:     "external:review_pr_monitor:deadbeef",
				Pos:        0,
			},
			IntentAt: now.Add(-15 * time.Minute),
			CompletedAt: func() *time.Time {
				t := now.Add(-14 * time.Minute)
				return &t
			}(),
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
		AssignmentRev:   3,
		Generation:      2,
		MirrorRev:       7,
		MirrorUpdatedAt: &completedAt,
		Body:            "body",
	}

	got := taskFromFrontmatter(frontmatterFromTask(original), original.Body)
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("frontmatter mapping mismatch\n got: %#v\nwant: %#v", got, original)
	}
}

func TestTaskFrontmatterMappingBackfillsLegacyTamperBlocker(t *testing.T) {
	t.Parallel()

	got := taskFromFrontmatter(taskFrontmatter{
		ID:           "task-legacy-tamper",
		Title:        "Legacy tamper task",
		Status:       StatusHumanRequired,
		StatusReason: workflow.TamperFlaggedReasonPrefix + " internal/foo_test.go: added-skip",
	}, "")

	if got.Blocker.Kind != blocker.KindTamperDetected {
		t.Fatalf("Blocker.Kind = %q, want %q", got.Blocker.Kind, blocker.KindTamperDetected)
	}
	if got.Blocker.Actor != blocker.ActorWorkflow {
		t.Fatalf("Blocker.Actor = %q, want %q", got.Blocker.Actor, blocker.ActorWorkflow)
	}
	if got.Blocker.NextAction != "bless_tampering" {
		t.Fatalf("Blocker.NextAction = %q, want bless_tampering", got.Blocker.NextAction)
	}
	if !got.TamperFlagged {
		t.Fatal("TamperFlagged = false, want true")
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
	case "Body", "Plan", "PlanContract", "PlanCritique", "PlanResearch", "PlanDecisions", "PlanBrief", "CodeReview", "CurrentTestFailures", "AcceptanceLedger", "SpecDecision", "PlanDrafts", "FilePath", "TamperFlagged", "Degraded", "ParseError":
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
		task.TaskType = TaskTypeUmbrella
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
	case "Escalation":
		task.Escalation = autonomy.EscalationReason{
			Code:       "operator.decision",
			Owner:      autonomy.FailureOwnerOperatorDecision,
			Provenance: autonomy.ProvenanceOperator,
			ObservedAt: now,
			Message:    "choose an option",
		}
	case "AutonomyOutcome":
		task.AutonomyOutcome = autonomy.OutcomeHumanRequired
	case "Blocker":
		task.Blocker = blocker.State{
			Kind:       blocker.KindWorktreeRepair,
			Actor:      blocker.ActorWorkflow,
			Code:       "rebase_failed",
			NextAction: "repair_worktree",
			Exhausted:  true,
		}
	case "HandoffSourceProvider":
		task.HandoffSourceProvider = "codex"
	case "BlockedByIssue":
		task.BlockedByIssue = "owner/repo#456"
	case "UmbrellaIssue":
		task.UmbrellaIssue = "owner/repo#789"
	case "DependsOn":
		task.DependsOn = []string{"owner/repo#321"}
	case "DependsOnConditions":
		task.DependsOnConditions = []DepCondition{{Ref: "owner/repo#321", Kind: DepConditionKindLabel, Value: "scope-confirmed"}}
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
	case "ReconcileFailures":
		task.ReconcileFailures = 3
	case "PRPhase":
		task.PRPhase = "fixing"
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
	case "SandboxOffReason":
		task.SandboxOffReason = "host mounts required for docker-in-docker e2e"
	case "ReasoningEffort":
		task.ReasoningEffort = "xhigh"
	case "TestingCycleStartedAt":
		task.TestingCycleStartedAt = &later
	case "Attachments":
		task.Attachments = []Attachment{{
			ID:          "att-1",
			FileName:    "log.txt",
			ContentType: "text/plain",
			SizeBytes:   123,
			Path:        "/tmp/attachments/task-persist/att-1/log.txt",
			CreatedAt:   later,
		}}
	case "AgentRuns":
		task.AgentRuns = []AgentRun{{
			AgentID:                 "agent-1",
			Role:                    "test-runner",
			Mode:                    AgentModeHeadless,
			Provider:                "claude",
			Model:                   "model-a",
			ExperimentID:            "exp",
			VariantID:               "variant",
			AssignmentUnit:          "task",
			AssignmentKey:           "task-persist",
			ReasoningEffort:         "high",
			RequestedSkill:          "sybra-test",
			SkillExecutionMode:      "native",
			ResolvedSkillSourceHash: "deadbeefcafebabe",
			SkillConformance:        "exact",
			State:                   "done",
			EscalationReason:        "cost",
			StartedAt:               now,
			CostUSD:                 1.25,
			PremiumRequests:         2.5,
			Prompt:                  "prompt",
			Result:                  "result",
			OneShot:                 true,
			Verdict:                 "human",
			VerdictRendered:         true,
			LogFile:                 "/tmp/log",
			SessionID:               "session-1",
			ProtocolViolation:       "bad-report",
			TestOutcome:             "product_bug",
			TestFailureFingerprint:  "fp",
			HeadSHA:                 "def456",
			SubagentCallCount:       2,
		}}
	case "EffectLog":
		task.EffectLog = []workflow.EffectRecord{{
			ID: workflow.EffectID{
				Generation: 3,
				StepSeq:    9,
				StepID:     "external:test:deadbeef",
				Pos:        0,
			},
			IntentAt: now,
			CompletedAt: func() *time.Time {
				t := now.Add(time.Minute)
				return &t
			}(),
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
	case "AssignmentRev":
		task.AssignmentRev = 3
	case "Generation":
		task.Generation = 2
	case "MirrorRev":
		task.MirrorRev = 7
	case "MirrorUpdatedAt":
		task.MirrorUpdatedAt = &later
	default:
		t.Fatalf("no persistence test value for Task.%s", name)
	}
}
