package task

import (
	"time"

	"github.com/Automaat/sybra/internal/workflow"
)

// taskFrontmatter is the on-disk YAML schema for a task file. Keep persistence
// field names here so Task can evolve as the domain/API model without carrying
// serialization tags.
type taskFrontmatter struct {
	ID                     string              `yaml:"id"`
	Slug                   string              `yaml:"slug,omitempty"`
	Title                  string              `yaml:"title"`
	Status                 Status              `yaml:"status"`
	TaskType               TaskType            `yaml:"task_type,omitempty"`
	AgentMode              string              `yaml:"agent_mode"`
	AllowedTools           []string            `yaml:"allowed_tools"`
	Tags                   []string            `yaml:"tags"`
	ProjectID              string              `yaml:"project_id,omitempty"`
	Branch                 string              `yaml:"branch,omitempty"`
	WorktreeDir            string              `yaml:"worktree_dir,omitempty"`
	PRNumber               int                 `yaml:"pr_number,omitempty"`
	Issue                  string              `yaml:"issue,omitempty"`
	RefIssue               string              `yaml:"ref_issue,omitempty"`
	StatusReason           string              `yaml:"status_reason,omitempty"`
	HandoffSourceProvider  string              `yaml:"handoff_source_provider,omitempty"`
	BlockedByIssue         string              `yaml:"blocked_by_issue,omitempty"`
	UmbrellaIssue          string              `yaml:"umbrella_issue,omitempty"`
	DependsOn              []string            `yaml:"depends_on,omitempty"`
	Reviewed               bool                `yaml:"reviewed,omitempty"`
	RunRole                string              `yaml:"run_role,omitempty"`
	SupervisorSteer        string              `yaml:"supervisor_steer,omitempty"`
	ReviewPhase            string              `yaml:"review_phase,omitempty"`
	PRPhase                string              `yaml:"pr_phase,omitempty"`
	TodoistID              string              `yaml:"todoist_id,omitempty"`
	Priority               Priority            `yaml:"priority,omitempty"`
	DueDate                *time.Time          `yaml:"due_date,omitempty"`
	ClosedAt               *time.Time          `yaml:"closed_at,omitempty"`
	Outcome                string              `yaml:"outcome,omitempty"`
	MergeCommit            string              `yaml:"merge_commit,omitempty"`
	MaxTurns               int                 `yaml:"max_turns,omitempty"`
	RequirePermissions     *bool               `yaml:"require_permissions,omitempty"`
	HeadlessPermissionMode string              `yaml:"headless_permission_mode,omitempty"`
	ForkSubagent           bool                `yaml:"fork_subagent,omitempty"`
	Sandbox                *bool               `yaml:"sandbox,omitempty"`
	ReasoningEffort        string              `yaml:"reasoning_effort,omitempty"`
	TestingCycleStartedAt  *time.Time          `yaml:"testing_cycle_started_at,omitempty"`
	AgentRuns              []agentRunRecord    `yaml:"agent_runs,omitempty"`
	Workflow               *workflow.Execution `yaml:"workflow,omitempty"`
	CreatedAt              time.Time           `yaml:"created_at"`
	UpdatedAt              time.Time           `yaml:"updated_at"`
	StatusChangedAt        time.Time           `yaml:"status_changed_at,omitempty"`
	AssignedNode           string              `yaml:"assigned_node,omitempty"`
	NodeOverride           string              `yaml:"node_override,omitempty"`
	MirrorRev              int64               `yaml:"mirror_rev,omitempty"`
	MirrorUpdatedAt        *time.Time          `yaml:"mirror_updated_at,omitempty"`
}

type agentRunRecord struct {
	AgentID                 string    `yaml:"agent_id"`
	Role                    string    `yaml:"role,omitempty"`
	Mode                    string    `yaml:"mode"`
	Provider                string    `yaml:"provider,omitempty"`
	Model                   string    `yaml:"model,omitempty"`
	ExperimentID            string    `yaml:"experiment_id,omitempty"`
	VariantID               string    `yaml:"variant_id,omitempty"`
	AssignmentUnit          string    `yaml:"assignment_unit,omitempty"`
	AssignmentKey           string    `yaml:"assignment_key,omitempty"`
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
	PremiumRequests         float64   `yaml:"premium_requests,omitempty"`
	Prompt                  string    `yaml:"prompt,omitempty"`
	Result                  string    `yaml:"result,omitempty"`
	OneShot                 bool      `yaml:"one_shot,omitempty"`
	Verdict                 string    `yaml:"verdict,omitempty"`
	VerdictRendered         bool      `yaml:"verdict_rendered,omitempty"`
	LogFile                 string    `yaml:"log_file,omitempty"`
	SessionID               string    `yaml:"session_id,omitempty"`
	ProtocolViolation       string    `yaml:"protocol_violation,omitempty"`
	TestOutcome             string    `yaml:"test_outcome,omitempty"`
	TestFailureFingerprint  string    `yaml:"test_failure_fingerprint,omitempty"`
	HeadSHA                 string    `yaml:"head_sha,omitempty"`
	SubagentCallCount       int       `yaml:"subagent_call_count,omitempty"`
}

// taskFromFrontmatter rebuilds the persisted task fields. Store loading
// populates sidecar fields such as Plan, CodeReview, PlanDrafts, and FilePath.
func taskFromFrontmatter(fm taskFrontmatter, body string) Task {
	t := Task{
		ID:                     fm.ID,
		Slug:                   fm.Slug,
		Title:                  fm.Title,
		Status:                 fm.Status,
		TaskType:               fm.TaskType,
		AgentMode:              fm.AgentMode,
		AllowedTools:           fm.AllowedTools,
		Tags:                   fm.Tags,
		ProjectID:              fm.ProjectID,
		Branch:                 fm.Branch,
		WorktreeDir:            fm.WorktreeDir,
		PRNumber:               fm.PRNumber,
		Issue:                  fm.Issue,
		RefIssue:               fm.RefIssue,
		StatusReason:           fm.StatusReason,
		HandoffSourceProvider:  fm.HandoffSourceProvider,
		BlockedByIssue:         fm.BlockedByIssue,
		UmbrellaIssue:          fm.UmbrellaIssue,
		DependsOn:              fm.DependsOn,
		Reviewed:               fm.Reviewed,
		RunRole:                fm.RunRole,
		SupervisorSteer:        fm.SupervisorSteer,
		ReviewPhase:            fm.ReviewPhase,
		PRPhase:                fm.PRPhase,
		TodoistID:              fm.TodoistID,
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
		ReasoningEffort:        fm.ReasoningEffort,
		TestingCycleStartedAt:  fm.TestingCycleStartedAt,
		Workflow:               fm.Workflow,
		CreatedAt:              fm.CreatedAt,
		UpdatedAt:              fm.UpdatedAt,
		StatusChangedAt:        fm.StatusChangedAt,
		AssignedNode:           fm.AssignedNode,
		NodeOverride:           fm.NodeOverride,
		MirrorRev:              fm.MirrorRev,
		MirrorUpdatedAt:        fm.MirrorUpdatedAt,
		Body:                   body,
	}
	if t.TaskType == "" {
		t.TaskType = TaskTypeNormal
	}
	t.AgentRuns = agentRunsFromRecords(fm.AgentRuns)
	if t.AgentRuns == nil {
		t.AgentRuns = []AgentRun{}
	}
	t.TamperFlagged = isTamperFlagged(t.Status, t.StatusReason)
	return t
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
		RefIssue:               t.RefIssue,
		StatusReason:           t.StatusReason,
		HandoffSourceProvider:  t.HandoffSourceProvider,
		BlockedByIssue:         t.BlockedByIssue,
		UmbrellaIssue:          t.UmbrellaIssue,
		DependsOn:              t.DependsOn,
		Reviewed:               t.Reviewed,
		RunRole:                t.RunRole,
		SupervisorSteer:        t.SupervisorSteer,
		ReviewPhase:            t.ReviewPhase,
		PRPhase:                t.PRPhase,
		TodoistID:              t.TodoistID,
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
		ReasoningEffort:        t.ReasoningEffort,
		TestingCycleStartedAt:  t.TestingCycleStartedAt,
		AgentRuns:              agentRunRecordsFromRuns(t.AgentRuns),
		Workflow:               t.Workflow,
		CreatedAt:              t.CreatedAt,
		UpdatedAt:              t.UpdatedAt,
		StatusChangedAt:        t.StatusChangedAt,
		AssignedNode:           t.AssignedNode,
		NodeOverride:           t.NodeOverride,
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
