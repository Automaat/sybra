package task

import (
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/workflow"
)

// taskFrontmatter is the on-disk YAML schema for a task file. Keep persistence
// field names here so Task can evolve as the domain/API model without carrying
// serialization tags (see TestTaskDomainTypesHaveNoYAMLTags). Field order
// mirrors Task's so the two are easy to diff side by side when a field is
// added.
type taskFrontmatter struct {
	ID              string                    `yaml:"id"`
	Slug            string                    `yaml:"slug,omitempty"`
	Title           string                    `yaml:"title"`
	Status          Status                    `yaml:"status"`
	TaskType        TaskType                  `yaml:"task_type,omitempty"`
	AgentMode       string                    `yaml:"agent_mode"`
	AllowedTools    []string                  `yaml:"allowed_tools"`
	Tags            []string                  `yaml:"tags"`
	ProjectID       string                    `yaml:"project_id,omitempty"`
	Branch          string                    `yaml:"branch,omitempty"`
	WorktreeDir     string                    `yaml:"worktree_dir,omitempty"`
	PRNumber        int                       `yaml:"pr_number,omitempty"`
	Issue           string                    `yaml:"issue,omitempty"`
	StatusReason    string                    `yaml:"status_reason,omitempty"`
	Escalation      autonomy.EscalationReason `yaml:"escalation,omitempty"`
	AutonomyOutcome autonomy.Outcome          `yaml:"autonomy_outcome,omitempty"`
	// Blocker matches Task's value type (not a pointer): blocker.State
	// implements IsZeroer, so yaml.v3's omitempty already skips a zero value
	// without needing pointer indirection to distinguish "unset" from "set".
	Blocker                blocker.State           `yaml:"blocker,omitempty"`
	HandoffSourceProvider  string                  `yaml:"handoff_source_provider,omitempty"`
	BlockedByIssue         string                  `yaml:"blocked_by_issue,omitempty"`
	UmbrellaIssue          string                  `yaml:"umbrella_issue,omitempty"`
	RefIssue               string                  `yaml:"ref_issue,omitempty"`
	DependsOn              []string                `yaml:"depends_on,omitempty"`
	DependsOnConditions    []DepCondition          `yaml:"depends_on_conditions,omitempty"`
	Reviewed               bool                    `yaml:"reviewed,omitempty"`
	CodeReviewVerdict      string                  `yaml:"code_review_verdict,omitempty"`
	RunRole                string                  `yaml:"run_role,omitempty"`
	SupervisorSteer        string                  `yaml:"supervisor_steer,omitempty"`
	ReviewPhase            string                  `yaml:"review_phase,omitempty"`
	ReviewedHeadSHA        string                  `yaml:"reviewed_head_sha,omitempty"`
	ReviewedHeadAttempts   int                     `yaml:"reviewed_head_attempts,omitempty"`
	ReconcileFailures      int                     `yaml:"reconcile_failures,omitempty"`
	PRPhase                string                  `yaml:"pr_phase,omitempty"`
	Priority               Priority                `yaml:"priority,omitempty"`
	DueDate                *time.Time              `yaml:"due_date,omitempty"`
	ClosedAt               *time.Time              `yaml:"closed_at,omitempty"`
	Outcome                string                  `yaml:"outcome,omitempty"`
	MergeCommit            string                  `yaml:"merge_commit,omitempty"`
	MaxTurns               int                     `yaml:"max_turns,omitempty"`
	RequirePermissions     *bool                   `yaml:"require_permissions,omitempty"`
	HeadlessPermissionMode string                  `yaml:"headless_permission_mode,omitempty"`
	ForkSubagent           bool                    `yaml:"fork_subagent,omitempty"`
	Sandbox                *bool                   `yaml:"sandbox,omitempty"`
	SandboxOffReason       string                  `yaml:"sandbox_off_reason,omitempty"`
	ReasoningEffort        string                  `yaml:"reasoning_effort,omitempty"`
	TestingCycleStartedAt  *time.Time              `yaml:"testing_cycle_started_at,omitempty"`
	Attachments            []Attachment            `yaml:"attachments,omitempty"`
	AgentRuns              []agentRunRecord        `yaml:"agent_runs,omitempty"`
	EffectLog              []workflow.EffectRecord `yaml:"effect_log,omitempty"`
	Workflow               *workflow.Execution     `yaml:"workflow,omitempty"`
	CreatedAt              time.Time               `yaml:"created_at"`
	UpdatedAt              time.Time               `yaml:"updated_at"`
	StatusChangedAt        time.Time               `yaml:"status_changed_at,omitempty"`
	AssignedNode           string                  `yaml:"assigned_node,omitempty"`
	NodeOverride           string                  `yaml:"node_override,omitempty"`
	AssignmentRev          int64                   `yaml:"assignment_rev,omitempty"`
	Generation             int64                   `yaml:"generation,omitempty"`
	MirrorRev              int64                   `yaml:"mirror_rev,omitempty"`
	MirrorUpdatedAt        *time.Time              `yaml:"mirror_updated_at,omitempty"`
}

type agentRunRecord struct {
	AgentID                 string    `yaml:"agent_id"`
	Role                    string    `yaml:"role,omitempty"`
	Mode                    string    `yaml:"mode"`
	Provider                string    `yaml:"provider,omitempty"`
	Model                   string    `yaml:"model,omitempty"`
	ExperimentID            string    `yaml:"experiment_id,omitempty"`
	VariantID               string    `yaml:"variant_id,omitempty"`
	RoutingReason           string    `yaml:"routing_reason,omitempty"`
	AssignmentUnit          string    `yaml:"assignment_unit,omitempty"`
	AssignmentKey           string    `yaml:"assignment_key,omitempty"`
	DecisionVersion         int       `yaml:"decision_version,omitempty"`
	ReasoningEffort         string    `yaml:"reasoning_effort,omitempty"`
	RequestedSkill          string    `yaml:"requested_skill,omitempty"`
	SkillExecutionMode      string    `yaml:"skill_execution_mode,omitempty"`
	ResolvedSkillSourceHash string    `yaml:"resolved_skill_source_hash,omitempty"`
	SkillConformance        string    `yaml:"skill_conformance,omitempty"`
	State                   string    `yaml:"state"`
	Outcome                 string    `yaml:"outcome,omitempty"`
	EscalationReason        string    `yaml:"escalation_reason,omitempty"`
	StartedAt               time.Time `yaml:"started_at"`
	CostUSD                 float64   `yaml:"cost_usd,omitempty"`
	ToolFailures            int       `yaml:"tool_failures,omitempty"`
	PremiumRequests         float64   `yaml:"premium_requests,omitempty"`
	Prompt                  string    `yaml:"prompt,omitempty"`
	Result                  string    `yaml:"result,omitempty"`
	OneShot                 bool      `yaml:"one_shot,omitempty"`
	Verdict                 string    `yaml:"verdict,omitempty"`
	VerdictRendered         bool      `yaml:"verdict_rendered,omitempty"`
	RecoveryReplayRejected  bool      `yaml:"recovery_replay_rejected,omitempty"`
	LogFile                 string    `yaml:"log_file,omitempty"`
	SessionID               string    `yaml:"session_id,omitempty"`
	ProtocolViolation       string    `yaml:"protocol_violation,omitempty"`
	TestOutcome             string    `yaml:"test_outcome,omitempty"`
	TestFailureFingerprint  string    `yaml:"test_failure_fingerprint,omitempty"`
	HeadSHA                 string    `yaml:"head_sha,omitempty"`
	FinalCommitSource       string    `yaml:"final_commit_source,omitempty"`
	SubagentCallCount       int       `yaml:"subagent_call_count,omitempty"`
	ResumeZeroOutputStall   bool      `yaml:"zero_output_stall,omitempty"`
	TurnCount               int       `yaml:"turn_count,omitempty"`
}

// taskFromFrontmatter rebuilds the persisted task fields. Store loading
// populates sidecar fields such as Plan, CodeReview, PlanDrafts, and FilePath.
func taskFromFrontmatter(fm taskFrontmatter, body string) Task {
	t := Task{
		ID:                     fm.ID,
		Slug:                   fm.Slug,
		Title:                  fm.Title,
		Status:                 fm.Status,
		TaskType:               normalizeTaskType(fm.TaskType),
		AgentMode:              fm.AgentMode,
		AllowedTools:           fm.AllowedTools,
		Tags:                   fm.Tags,
		ProjectID:              fm.ProjectID,
		Branch:                 fm.Branch,
		WorktreeDir:            fm.WorktreeDir,
		PRNumber:               fm.PRNumber,
		Issue:                  fm.Issue,
		StatusReason:           fm.StatusReason,
		Escalation:             fm.Escalation,
		AutonomyOutcome:        fm.AutonomyOutcome,
		Blocker:                fm.Blocker,
		HandoffSourceProvider:  fm.HandoffSourceProvider,
		BlockedByIssue:         fm.BlockedByIssue,
		UmbrellaIssue:          fm.UmbrellaIssue,
		RefIssue:               fm.RefIssue,
		DependsOn:              fm.DependsOn,
		DependsOnConditions:    fm.DependsOnConditions,
		Reviewed:               fm.Reviewed,
		CodeReviewVerdict:      fm.CodeReviewVerdict,
		RunRole:                fm.RunRole,
		SupervisorSteer:        fm.SupervisorSteer,
		ReviewPhase:            fm.ReviewPhase,
		ReviewedHeadSHA:        fm.ReviewedHeadSHA,
		ReviewedHeadAttempts:   fm.ReviewedHeadAttempts,
		ReconcileFailures:      fm.ReconcileFailures,
		PRPhase:                fm.PRPhase,
		Priority:               fm.Priority,
		DueDate:                fm.DueDate,
		ClosedAt:               fm.ClosedAt,
		Outcome:                fm.Outcome,
		MergeCommit:            fm.MergeCommit,
		MaxTurns:               fm.MaxTurns,
		RequirePermissions:     fm.RequirePermissions,
		HeadlessPermissionMode: fm.HeadlessPermissionMode,
		ForkSubagent:           fm.ForkSubagent,
		Sandbox:                fm.Sandbox,
		SandboxOffReason:       fm.SandboxOffReason,
		ReasoningEffort:        fm.ReasoningEffort,
		TestingCycleStartedAt:  fm.TestingCycleStartedAt,
		Attachments:            fm.Attachments,
		Workflow:               fm.Workflow,
		EffectLog:              fm.EffectLog,
		CreatedAt:              fm.CreatedAt,
		UpdatedAt:              fm.UpdatedAt,
		StatusChangedAt:        fm.StatusChangedAt,
		AssignedNode:           fm.AssignedNode,
		NodeOverride:           fm.NodeOverride,
		AssignmentRev:          fm.AssignmentRev,
		Generation:             fm.Generation,
		MirrorRev:              fm.MirrorRev,
		MirrorUpdatedAt:        fm.MirrorUpdatedAt,
		Body:                   body,
	}
	if t.Status == StatusHumanRequired && t.Escalation.IsZero() {
		t.Escalation = autonomy.LegacyReason(t.StatusReason)
	}
	if t.Status == StatusHumanRequired && t.Blocker.IsZero() && workflow.IsTamperFlaggedReason(t.StatusReason) {
		t.Blocker = blocker.State{
			Kind:       blocker.KindTamperDetected,
			Actor:      blocker.ActorWorkflow,
			NextAction: "bless_tampering",
		}
	}
	t.AgentRuns = agentRunsFromRecords(fm.AgentRuns)
	if t.AgentRuns == nil {
		t.AgentRuns = []AgentRun{}
	}
	if t.Attachments == nil {
		t.Attachments = []Attachment{}
	}
	t.TamperFlagged = isTamperFlagged(t.Status, t.Blocker)
	return t
}

func normalizeTaskType(tt TaskType) TaskType {
	if tt == TaskTypeUmbrella {
		return TaskTypeUmbrella
	}
	return ""
}

func frontmatterFromTask(t Task) taskFrontmatter {
	return taskFrontmatter{
		ID:                     t.ID,
		Slug:                   t.Slug,
		Title:                  t.Title,
		Status:                 t.Status,
		TaskType:               t.TaskType,
		AgentMode:              t.AgentMode,
		AllowedTools:           t.AllowedTools,
		Tags:                   t.Tags,
		ProjectID:              t.ProjectID,
		Branch:                 t.Branch,
		WorktreeDir:            t.WorktreeDir,
		PRNumber:               t.PRNumber,
		Issue:                  t.Issue,
		StatusReason:           t.StatusReason,
		Escalation:             t.Escalation,
		AutonomyOutcome:        t.AutonomyOutcome,
		Blocker:                t.Blocker,
		HandoffSourceProvider:  t.HandoffSourceProvider,
		BlockedByIssue:         t.BlockedByIssue,
		UmbrellaIssue:          t.UmbrellaIssue,
		RefIssue:               t.RefIssue,
		DependsOn:              t.DependsOn,
		DependsOnConditions:    t.DependsOnConditions,
		Reviewed:               t.Reviewed,
		CodeReviewVerdict:      t.CodeReviewVerdict,
		RunRole:                t.RunRole,
		SupervisorSteer:        t.SupervisorSteer,
		ReviewPhase:            t.ReviewPhase,
		ReviewedHeadSHA:        t.ReviewedHeadSHA,
		ReviewedHeadAttempts:   t.ReviewedHeadAttempts,
		ReconcileFailures:      t.ReconcileFailures,
		PRPhase:                t.PRPhase,
		Priority:               t.Priority,
		DueDate:                t.DueDate,
		ClosedAt:               t.ClosedAt,
		Outcome:                t.Outcome,
		MergeCommit:            t.MergeCommit,
		MaxTurns:               t.MaxTurns,
		RequirePermissions:     t.RequirePermissions,
		HeadlessPermissionMode: t.HeadlessPermissionMode,
		ForkSubagent:           t.ForkSubagent,
		Sandbox:                t.Sandbox,
		SandboxOffReason:       t.SandboxOffReason,
		ReasoningEffort:        t.ReasoningEffort,
		TestingCycleStartedAt:  t.TestingCycleStartedAt,
		Attachments:            t.Attachments,
		AgentRuns:              agentRunRecordsFromRuns(t.AgentRuns),
		EffectLog:              t.EffectLog,
		Workflow:               t.Workflow,
		CreatedAt:              t.CreatedAt,
		UpdatedAt:              t.UpdatedAt,
		StatusChangedAt:        t.StatusChangedAt,
		AssignedNode:           t.AssignedNode,
		NodeOverride:           t.NodeOverride,
		AssignmentRev:          t.AssignmentRev,
		Generation:             t.Generation,
		MirrorRev:              t.MirrorRev,
		MirrorUpdatedAt:        t.MirrorUpdatedAt,
	}
}

func agentRunsFromRecords(records []agentRunRecord) []AgentRun {
	if records == nil {
		return nil
	}
	runs := make([]AgentRun, len(records))
	for i := range records {
		runs[i] = AgentRun(records[i])
	}
	return runs
}

func agentRunRecordsFromRuns(runs []AgentRun) []agentRunRecord {
	if runs == nil {
		return nil
	}
	records := make([]agentRunRecord, len(runs))
	for i := range runs {
		records[i] = agentRunRecord(runs[i])
	}
	return records
}
