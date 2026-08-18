package task

import (
	"fmt"
	"time"

	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/fsutil"
)

// AddRun appends run to taskID's AgentRuns without changing its status.
func (s *Store) AddRun(taskID string, run AgentRun) error {
	return s.addRun(taskID, run, nil)
}

// AddRunWithStatus appends run to taskID's AgentRuns and atomically sets its
// status to *status. Use this instead of AddRun+Update when the status
// transition must be recorded alongside the run that caused it.
func (s *Store) AddRunWithStatus(taskID string, run AgentRun, status *Status) error {
	return s.addRun(taskID, run, status)
}

func (s *Store) addRun(taskID string, run AgentRun, status *Status) error {
	unlock, err := s.lockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	t, err := s.read(taskID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if status != nil {
		oldStatus := t.Status
		if *status == StatusHumanRequired && oldStatus != StatusHumanRequired {
			return fmt.Errorf("add run with status: transition to human-required requires typed escalation evidence")
		}
		t.Status = *status
		if oldStatus != t.Status {
			t.StatusChangedAt = now
			t.StatusReason = ""
			t.Blocker = blocker.State{}
			t.Escalation = autonomy.EscalationReason{}
			t.AutonomyOutcome = ""
		} else {
			backfillStatusChangedAt(&t, now)
		}
		wasTerminal := IsTerminalStatus(oldStatus)
		isTerminal := IsTerminalStatus(t.Status)
		if !wasTerminal && isTerminal {
			t.ClosedAt = &now
		} else if wasTerminal && !isTerminal {
			t.ClosedAt = nil
		}
	} else {
		backfillStatusChangedAt(&t, now)
	}
	t.AgentRuns = append(t.AgentRuns, run)
	t.UpdatedAt = now
	d, err := marshalTask(t, false)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(t.FilePath, d); err != nil {
		return err
	}
	s.storeTaskCache(t)
	return nil
}

func backfillStatusChangedAt(t *Task, fallback time.Time) {
	if !t.StatusChangedAt.IsZero() {
		return
	}
	t.StatusChangedAt = statusChangedAtBackfill(*t, fallback)
}

func statusChangedAtBackfill(t Task, fallback time.Time) time.Time {
	switch {
	case !t.UpdatedAt.IsZero():
		return t.UpdatedAt
	case !t.CreatedAt.IsZero():
		return t.CreatedAt
	default:
		return fallback
	}
}

// RunPatch describes a partial update to an AgentRun. Every field is a
// pointer: nil means "leave unchanged". Fields that carried an implicit
// non-empty/true guard in the old map[string]any path keep that guard here
// (see applyRunLifecycle/applyRunVerdict/applyRunTestOutcome/applyRunIdentity):
// HeadSHA, FinalCommitSource, Outcome, EscalationReason, and string verdict/
// test/session values ignore empty strings, and verdict/recovery booleans are
// latches that only ever flip true.
type RunPatch struct {
	// Lifecycle
	State                 *string
	Outcome               *string
	EscalationReason      *string
	Result                *string
	LogFile               *string
	HeadSHA               *string
	FinalCommitSource     *string
	ResumeZeroOutputStall *bool

	// Cost/tokens
	CostUSD         *float64
	PremiumRequests *float64
	ToolFailures    *int

	// Verdict
	Verdict                *string
	VerdictRendered        *bool
	RecoveryReplayRejected *bool

	// Test outcome
	TestOutcome            *string
	TestFailureFingerprint *string
	ProtocolViolation      *string

	// Identity
	Provider                *string
	Model                   *string
	ExperimentID            *string
	VariantID               *string
	AssignmentUnit          *string
	AssignmentKey           *string
	ReasoningEffort         *string
	RequestedSkill          *string
	SkillExecutionMode      *string
	ResolvedSkillSourceHash *string
	SkillConformance        *string
	SessionID               *string
	SubagentCallCount       *int
	TurnCount               *int
}

func applyRunLifecycle(run *AgentRun, p RunPatch) {
	if p.State != nil {
		run.State = *p.State
	}
	if p.Outcome != nil && *p.Outcome != "" {
		run.Outcome = *p.Outcome
	}
	if p.EscalationReason != nil && *p.EscalationReason != "" {
		run.EscalationReason = *p.EscalationReason
	}
	if p.Result != nil {
		run.Result = *p.Result
	}
	if p.LogFile != nil {
		run.LogFile = *p.LogFile
	}
	if p.HeadSHA != nil && *p.HeadSHA != "" {
		run.HeadSHA = *p.HeadSHA
	}
	if p.FinalCommitSource != nil && *p.FinalCommitSource != "" {
		run.FinalCommitSource = *p.FinalCommitSource
	}
	if p.ResumeZeroOutputStall != nil && *p.ResumeZeroOutputStall {
		run.ResumeZeroOutputStall = true
	}
}

func applyRunCostTokens(run *AgentRun, p RunPatch) {
	if p.CostUSD != nil {
		run.CostUSD = *p.CostUSD
	}
	if p.PremiumRequests != nil {
		run.PremiumRequests = *p.PremiumRequests
	}
	if p.ToolFailures != nil {
		run.ToolFailures = *p.ToolFailures
	}
}

func applyRunVerdict(run *AgentRun, p RunPatch) {
	if p.Verdict != nil && *p.Verdict != "" {
		run.Verdict = *p.Verdict
	}
	if p.VerdictRendered != nil && *p.VerdictRendered {
		run.VerdictRendered = true
	}
	if p.RecoveryReplayRejected != nil && *p.RecoveryReplayRejected {
		run.RecoveryReplayRejected = true
	}
}

func applyRunTestOutcome(run *AgentRun, p RunPatch) {
	if p.TestOutcome != nil && *p.TestOutcome != "" {
		run.TestOutcome = *p.TestOutcome
	}
	if p.TestFailureFingerprint != nil && *p.TestFailureFingerprint != "" {
		run.TestFailureFingerprint = *p.TestFailureFingerprint
	}
	if p.ProtocolViolation != nil && *p.ProtocolViolation != "" {
		run.ProtocolViolation = *p.ProtocolViolation
	}
}

func applyRunIdentity(run *AgentRun, p RunPatch) {
	if p.Provider != nil {
		run.Provider = *p.Provider
	}
	if p.Model != nil {
		run.Model = *p.Model
	}
	if p.ExperimentID != nil {
		run.ExperimentID = *p.ExperimentID
	}
	if p.VariantID != nil {
		run.VariantID = *p.VariantID
	}
	if p.AssignmentUnit != nil {
		run.AssignmentUnit = *p.AssignmentUnit
	}
	if p.AssignmentKey != nil {
		run.AssignmentKey = *p.AssignmentKey
	}
	if p.ReasoningEffort != nil {
		run.ReasoningEffort = *p.ReasoningEffort
	}
	if p.RequestedSkill != nil {
		run.RequestedSkill = *p.RequestedSkill
	}
	if p.SkillExecutionMode != nil {
		run.SkillExecutionMode = *p.SkillExecutionMode
	}
	if p.ResolvedSkillSourceHash != nil {
		run.ResolvedSkillSourceHash = *p.ResolvedSkillSourceHash
	}
	if p.SkillConformance != nil {
		run.SkillConformance = *p.SkillConformance
	}
	if p.SessionID != nil && *p.SessionID != "" {
		run.SessionID = *p.SessionID
	}
	if p.SubagentCallCount != nil {
		run.SubagentCallCount = *p.SubagentCallCount
	}
	if p.TurnCount != nil {
		run.TurnCount = *p.TurnCount
	}
}

func applyRunPatch(run *AgentRun, p RunPatch) {
	applyRunLifecycle(run, p)
	applyRunCostTokens(run, p)
	applyRunVerdict(run, p)
	applyRunTestOutcome(run, p)
	applyRunIdentity(run, p)
}

// UpdateRun applies patch to the AgentRun matching agentID within taskID's
// AgentRuns. Returns an error if the task or the run within it is not
// found.
func (s *Store) UpdateRun(taskID, agentID string, patch RunPatch) error {
	unlock, err := s.lockTask(taskID)
	if err != nil {
		return err
	}
	defer unlock()

	t, err := s.read(taskID)
	if err != nil {
		return err
	}
	found := false
	for i := range t.AgentRuns {
		if t.AgentRuns[i].AgentID != agentID {
			continue
		}
		found = true
		applyRunPatch(&t.AgentRuns[i], patch)
		break
	}
	if !found {
		return fmt.Errorf("agent run %s not found for task %s", agentID, taskID)
	}
	now := time.Now().UTC()
	backfillStatusChangedAt(&t, now)
	t.UpdatedAt = now
	d, err := marshalTask(t, false)
	if err != nil {
		return err
	}
	if err := fsutil.AtomicWrite(t.FilePath, d); err != nil {
		return err
	}
	s.storeTaskCache(t)
	return nil
}
