package review

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/autonomy"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/taskstatus"
	"github.com/Automaat/sybra/internal/workflow"
)

var (
	errWorkflowEffectNoPersist             = errors.New("workflow effect claim requires no persistence")
	errWorkflowStatusReasonNoLongerMatches = errors.New("workflow status reason no longer matches")
	errWorkflowWriteFenceMismatch          = errors.New("workflow write fence mismatch")
)

// taskAdapter and agentAdapter are minimal test-only stand-ins for the real
// bridges (internal/sybra's private taskAdapter/agentAdapter) that let a
// mechanical test workflow (no run_agent step) drive a real workflow.Engine
// without depending on package sybra. agentAdapter.StartAgent is never
// exercised by these tests — every other method proxies directly to
// agent.Manager.

type taskAdapter struct {
	tasks    *task.Manager
	projects *project.Store
}

func (a *taskAdapter) GetTask(id string) (workflow.TaskInfo, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return workflow.TaskInfo{}, err
	}
	info := taskToInfo(t)
	if t.ProjectID != "" && a.projects != nil {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			info.ProjectType = string(p.Type)
		}
	}
	return info, nil
}

func (a *taskAdapter) ListTasks() ([]workflow.TaskInfo, error) {
	tasks, err := a.tasks.List()
	if err != nil {
		return nil, err
	}
	infos := make([]workflow.TaskInfo, 0, len(tasks))
	for i := range tasks {
		infos = append(infos, taskToInfo(tasks[i]))
	}
	return infos, nil
}

func (a *taskAdapter) UpdateTaskStatus(id string, status taskstatus.Status, reason string) error {
	st, err := task.ValidateStatus(string(status))
	if err != nil {
		return err
	}
	_, err = a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		target := st
		u := task.Update{}
		if reason != "" {
			u.StatusReason = &reason
		} else if cur.Status != st && (st != task.StatusHumanRequired || cur.Status != task.StatusBlocked) {
			u.ClearStatusReason = task.Ptr(true)
		}
		if st == task.StatusHumanRequired {
			target = task.StatusBlocked
			u.Escalation = task.MachineFailure("workflow.untyped_escalation", reason)
			u.AutonomyOutcome = task.QuarantinedOutcome()
		}
		return task.TransitionIntent{
			ToStatus: target,
			Actor:    "workflow.engine.update_status",
			Extra:    u,
		}, nil
	})
	return err
}

func (a *taskAdapter) ClearTaskStatusReasonIf(id string, expectedStatus taskstatus.Status, expectedReason string) (bool, error) {
	cleared := false
	_, err := a.tasks.UpdateFn(id, func(cur task.Task) (task.Update, error) {
		if cur.Status != expectedStatus || cur.StatusReason != expectedReason {
			return task.Update{}, errWorkflowStatusReasonNoLongerMatches
		}
		empty := ""
		cleared = true
		return task.Update{StatusReason: &empty}, nil
	})
	if errors.Is(err, errWorkflowStatusReasonNoLongerMatches) {
		return false, nil
	}
	return cleared, err
}

func (a *taskAdapter) ClearTaskStatusReasonAndSetWorkflowIf(id, expectedStatus, expectedReason string, wf *workflow.Execution) (bool, error) {
	cleared := false
	_, err := a.tasks.UpdateFn(id, func(cur task.Task) (task.Update, error) {
		if string(cur.Status) != expectedStatus || cur.StatusReason != expectedReason {
			return task.Update{}, errWorkflowStatusReasonNoLongerMatches
		}
		empty := ""
		cleared = true
		return task.Update{StatusReason: &empty, Workflow: &wf}, nil
	})
	if errors.Is(err, errWorkflowStatusReasonNoLongerMatches) {
		return false, nil
	}
	return cleared, err
}
func (a *taskAdapter) UpdateTaskBlocker(id string, status taskstatus.Status, reason string, state blocker.State) error {
	st, err := task.ValidateStatus(string(status))
	if err != nil {
		return err
	}
	u := task.Update{Status: &st, Blocker: &state}
	if reason != "" {
		u.StatusReason = &reason
	}
	u.Escalation, u.AutonomyOutcome = testTypedBlockerEscalation(st, state, reason)
	if st == task.StatusHumanRequired && !blocker.AllowsHumanRequired(state.Kind) {
		st = task.StatusBlocked
		u.Status = &st
	}
	_, err = a.tasks.Update(id, u)
	return err
}

func (a *taskAdapter) UpdateTaskPR(id string, prNumber int) error {
	_, err := a.tasks.Update(id, task.Update{PRNumber: &prNumber})
	return err
}

func (a *taskAdapter) MarkTaskReviewed(id string) error {
	reviewed := true
	_, err := a.tasks.Update(id, task.Update{Reviewed: &reviewed})
	return err
}

func (a *taskAdapter) SetCodeReviewVerdict(id, verdict string) error {
	_, err := a.tasks.Update(id, task.Update{CodeReviewVerdict: &verdict})
	return err
}

func (a *taskAdapter) MarkAgentRunProtocolViolation(taskID, agentID, violation string) error {
	return a.tasks.UpdateRun(taskID, agentID, task.RunPatch{ProtocolViolation: task.Ptr(violation)})
}

func (a *taskAdapter) MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error {
	patch := task.RunPatch{TestOutcome: task.Ptr(outcome)}
	if fingerprint != "" {
		patch.TestFailureFingerprint = task.Ptr(fingerprint)
	}
	return a.tasks.UpdateRun(taskID, agentID, patch)
}

func (a *taskAdapter) MarkAgentRunIncomplete(taskID, agentID string) error {
	return a.tasks.UpdateRun(taskID, agentID, task.RunPatch{Outcome: task.Ptr(task.RunOutcomeIncomplete)})
}

func (a *taskAdapter) RecordAgentRunFinalCommit(taskID, agentID, headSHA, source string) error {
	patch := task.RunPatch{}
	if headSHA != "" {
		patch.HeadSHA = task.Ptr(headSHA)
	}
	if source != "" {
		patch.FinalCommitSource = task.Ptr(source)
	}
	return a.tasks.UpdateRun(taskID, agentID, patch)
}

func (a *taskAdapter) AppendTaskBody(id, content string) error {
	_, err := a.tasks.AppendBody(id, content)
	return err
}

func (a *taskAdapter) ReplaceTaskBody(id, body string) error {
	_, err := a.tasks.Update(id, task.Update{Body: &body})
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.set_workflow",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	return err
}

func (a *taskAdapter) SetStatusAndWorkflow(id, status, reason string, wf *workflow.Execution) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	extra := task.Update{Workflow: &wf}
	if reason != "" {
		extra.StatusReason = &reason
	}
	if st == task.StatusHumanRequired {
		st = task.StatusBlocked
		extra.Escalation = task.MachineFailure("workflow.untyped_escalation", reason)
		extra.AutonomyOutcome = task.QuarantinedOutcome()
	}
	_, err = a.tasks.Apply(task.TransitionIntent{
		TaskID:   id,
		ToStatus: st,
		Actor:    "workflow.engine.set_status_and_workflow",
		Extra:    extra,
	})
	return err
}

func (a *taskAdapter) SetEscalationAndWorkflow(id, status, reason string, escalation autonomy.EscalationReason, outcome autonomy.Outcome, wf *workflow.Execution) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	extra := task.Update{Workflow: &wf, Escalation: &escalation, AutonomyOutcome: &outcome}
	if reason != "" {
		extra.StatusReason = &reason
	}
	_, err = a.tasks.Apply(task.TransitionIntent{
		TaskID:   id,
		ToStatus: st,
		Actor:    "workflow.engine.set_escalation_and_workflow",
		Extra:    extra,
	})
	return err
}

func (a *taskAdapter) SetBlockerAndWorkflow(id, status, reason string, state blocker.State, wf *workflow.Execution) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	u := task.Update{Status: &st, Blocker: &state, Workflow: &wf}
	if reason != "" {
		u.StatusReason = &reason
	}
	u.Escalation, u.AutonomyOutcome = testTypedBlockerEscalation(st, state, reason)
	if st == task.StatusHumanRequired && !blocker.AllowsHumanRequired(state.Kind) {
		st = task.StatusBlocked
		u.Status = &st
	}
	_, err = a.tasks.Update(id, u)
	return err
}

func testTypedBlockerEscalation(status task.Status, state blocker.State, reason string) (*autonomy.EscalationReason, *autonomy.Outcome) {
	if status != task.StatusHumanRequired {
		return nil, nil
	}
	owner := blocker.FailureOwner(state.Kind)
	if owner == autonomy.FailureOwnerUnknown {
		owner = autonomy.FailureOwnerMachine
	}
	code := "workflow.blocker." + string(state.Kind)
	if state.Kind == "" {
		code = "workflow.blocker.unknown"
	}
	if owner.AllowsHumanRequired() {
		return task.ControlPlaneFailure(code, owner, reason), task.HumanRequiredOutcome()
	}
	return task.ControlPlaneFailure(code, owner, reason), task.QuarantinedOutcome()
}
func (a *taskAdapter) SetWorkflowIf(id string, fence workflow.WorkflowWriteFence, wf *workflow.Execution) (bool, error) {
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Generation != fence.Generation || cur.Status != fence.Status ||
			cur.StatusReason != fence.StatusReason || cur.Workflow == nil ||
			cur.Workflow.WorkflowID != fence.WorkflowID || cur.Workflow.CurrentStep != fence.CurrentStep ||
			cur.Workflow.State != fence.State {
			return task.TransitionIntent{}, errWorkflowWriteFenceMismatch
		}
		expectedStatus := cur.Status
		return task.TransitionIntent{
			TaskID: id, ToStatus: cur.Status, Actor: "workflow.engine.set_workflow_if",
			ExpectedGeneration: &fence.Generation, ExpectedStatus: &expectedStatus,
			Extra: task.Update{Workflow: &wf},
		}, nil
	})
	if errors.Is(err, errWorkflowWriteFenceMismatch) {
		return false, nil
	}
	return err == nil, err
}

func (a *taskAdapter) SetStatusAndWorkflowIf(id string, fence workflow.WorkflowWriteFence, status taskstatus.Status, reason string, wf *workflow.Execution) (bool, error) {
	st, err := task.ValidateStatus(string(status))
	if err != nil {
		return false, err
	}
	_, err = a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Generation != fence.Generation || cur.Status != fence.Status ||
			cur.StatusReason != fence.StatusReason || cur.Workflow == nil ||
			cur.Workflow.WorkflowID != fence.WorkflowID || cur.Workflow.CurrentStep != fence.CurrentStep ||
			cur.Workflow.State != fence.State {
			return task.TransitionIntent{}, errWorkflowWriteFenceMismatch
		}
		expectedStatus := cur.Status
		return task.TransitionIntent{
			TaskID: id, ToStatus: st, Actor: "workflow.engine.set_status_and_workflow_if",
			ExpectedGeneration: &fence.Generation, ExpectedStatus: &expectedStatus,
			Extra: task.Update{StatusReason: task.Ptr(reason), Workflow: &wf},
		}, nil
	})
	if errors.Is(err, errWorkflowWriteFenceMismatch) {
		return false, nil
	}
	return err == nil, err
}

func (a *taskAdapter) ClaimWorkflowEffect(id string, claim workflow.EffectClaim) (workflow.EffectClaimResult, error) {
	var result workflow.EffectClaimResult
	var fenceErr error
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Workflow == nil {
			return task.TransitionIntent{}, fmt.Errorf("task %s has no workflow", id)
		}
		wf := cur.Workflow.Clone()
		result.Workflow = wf
		claimResult, claimErr := wf.ClaimEffect(claim)
		claimResult.Workflow = wf
		result = claimResult
		if claimErr != nil {
			if errors.Is(claimErr, workflow.ErrEffectClaimConflict) || errors.Is(claimErr, workflow.ErrEffectAlreadyComplete) {
				fenceErr = claimErr
				return task.TransitionIntent{}, errWorkflowEffectNoPersist
			}
			return task.TransitionIntent{}, claimErr
		}
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.claim_effect",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowEffectNoPersist) {
			return result, fenceErr
		}
		return workflow.EffectClaimResult{}, err
	}
	return result, nil
}

func (a *taskAdapter) CompleteWorkflowEffect(id string, claim workflow.EffectClaim) (workflow.EffectClaimResult, error) {
	var result workflow.EffectClaimResult
	var fenceErr error
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Workflow == nil {
			return task.TransitionIntent{}, fmt.Errorf("task %s has no workflow", id)
		}
		wf := cur.Workflow.Clone()
		result.Workflow = wf
		claimResult, claimErr := wf.CompleteEffect(claim)
		claimResult.Workflow = wf
		result = claimResult
		if claimErr != nil {
			if errors.Is(claimErr, workflow.ErrEffectClaimLost) || errors.Is(claimErr, workflow.ErrEffectAlreadyComplete) {
				fenceErr = claimErr
				return task.TransitionIntent{}, errWorkflowEffectNoPersist
			}
			return task.TransitionIntent{}, claimErr
		}
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.complete_effect",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowEffectNoPersist) {
			return result, fenceErr
		}
		return workflow.EffectClaimResult{}, err
	}
	return result, nil
}

func (a *taskAdapter) ReleaseWorkflowEffect(id string, claim workflow.EffectClaim) (workflow.EffectClaimResult, error) {
	var result workflow.EffectClaimResult
	var fenceErr error
	_, err := a.tasks.ApplyFn(id, func(cur task.Task) (task.TransitionIntent, error) {
		if cur.Workflow == nil {
			return task.TransitionIntent{}, fmt.Errorf("task %s has no workflow", id)
		}
		wf := cur.Workflow.Clone()
		result.Workflow = wf
		claimResult, claimErr := wf.ReleaseEffect(claim)
		claimResult.Workflow = wf
		result = claimResult
		if claimErr != nil {
			if errors.Is(claimErr, workflow.ErrEffectClaimLost) || errors.Is(claimErr, workflow.ErrEffectAlreadyComplete) {
				fenceErr = claimErr
				return task.TransitionIntent{}, errWorkflowEffectNoPersist
			}
			return task.TransitionIntent{}, claimErr
		}
		return task.TransitionIntent{
			ToStatus: cur.Status,
			Actor:    "workflow.engine.release_effect",
			Extra:    task.Update{Workflow: &wf},
		}, nil
	})
	if err != nil {
		if errors.Is(err, errWorkflowEffectNoPersist) {
			return result, fenceErr
		}
		return workflow.EffectClaimResult{}, err
	}
	return result, nil
}

func (a *taskAdapter) ConsumeSupervisorSteer(taskID, prompt string) (string, error) {
	return agentorch.PrependSupervisorSteer(a.tasks, taskID, prompt)
}

func (a *taskAdapter) WriteSidecar(id, kind, content string) error {
	if name, ok := strings.CutPrefix(kind, "plan_draft."); ok {
		return a.tasks.PlanDrafts().Write(id, name, content)
	}
	var u task.Update
	switch kind {
	case "plan":
		u.Plan = &content
	case "plan_contract":
		u.PlanContract = &content
	case "code_review":
		u.CodeReview = &content
	case "plan_critique":
		u.PlanCritique = &content
	case "plan_research":
		u.PlanResearch = &content
	case "plan_decisions":
		u.PlanDecisions = &content
	case "plan_brief":
		u.PlanBrief = &content
	case "current_test_failures":
		u.CurrentTestFailures = &content
	case "acceptance_ledger":
		u.AcceptanceLedger = &content
	case "spec_decision":
		u.SpecDecision = &content
	default:
		return fmt.Errorf("unknown sidecar kind %q (want plan|plan_contract|code_review|plan_critique|plan_research|plan_decisions|plan_brief|current_test_failures|acceptance_ledger|spec_decision|plan_draft.<name>)", kind)
	}
	_, err := a.tasks.Update(id, u)
	return err
}

func taskToInfo(t task.Task) workflow.TaskInfo {
	return workflow.TaskInfo{
		ID:                    t.ID,
		Title:                 t.Title,
		Generation:            t.Generation,
		Status:                t.Status,
		StatusReason:          t.StatusReason,
		Role:                  t.RunRole,
		Tags:                  t.Tags,
		AgentMode:             t.AgentMode,
		ProjectID:             t.ProjectID,
		HandoffSourceProvider: t.HandoffSourceProvider,
		PRNumber:              t.PRNumber,
		Branch:                t.Branch,
		Body:                  t.Body,
		Plan:                  t.Plan,
		PlanContract:          t.PlanContract,
		PlanCritique:          t.PlanCritique,
		PlanResearch:          t.PlanResearch,
		PlanDecisions:         t.PlanDecisions,
		PlanBrief:             t.PlanBrief,
		CodeReview:            t.CodeReview,
		CurrentTestFailures:   t.CurrentTestFailures,
		AcceptanceLedger:      t.AcceptanceLedger,
		SpecDecision:          t.SpecDecision,
		CodeReviewVerdict:     t.CodeReviewVerdict,
		PlanDrafts:            t.PlanDrafts,
		Issue:                 t.Issue,
		Reviewed:              t.Reviewed,
		Workflow:              t.Workflow,
		AgentRuns:             toRunInfos(t.AgentRuns),
		TestingCycleStartedAt: t.TestingCycleStartedAt,
	}
}

func TestTaskToInfo_PreservesRunRole(t *testing.T) {
	info := taskToInfo(task.Task{ID: "t1", RunRole: "review"})
	if info.Role != "review" {
		t.Fatalf("Role = %q, want review", info.Role)
	}
}

func toRunInfos(runs []task.AgentRun) []workflow.AgentRunInfo {
	if len(runs) == 0 {
		return nil
	}
	out := make([]workflow.AgentRunInfo, len(runs))
	for i := range runs {
		out[i] = workflow.AgentRunInfo{
			AgentID:                runs[i].AgentID,
			Role:                   runs[i].Role,
			Provider:               runs[i].Provider,
			RequestedSkill:         runs[i].RequestedSkill,
			SkillExecutionMode:     runs[i].SkillExecutionMode,
			SkillConformance:       runs[i].SkillConformance,
			StartedAt:              runs[i].StartedAt,
			ProtocolViolation:      runs[i].ProtocolViolation,
			TestOutcome:            runs[i].TestOutcome,
			TestFailureFingerprint: runs[i].TestFailureFingerprint,
			HeadSHA:                runs[i].HeadSHA,
		}
	}
	return out
}

// agentAdapter is a minimal AgentLauncher stand-in: every method but
// StartAgent proxies to agent.Manager. StartAgent is never exercised by the
// mechanical (no run_agent step) test workflows that use this adapter.
type agentAdapter struct {
	agents *agent.Manager
	tasks  *task.Manager
}

func (a *agentAdapter) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	return "", "", "", fmt.Errorf("agentAdapter.StartAgent: not implemented in this test double (task %s)", taskID)
}

func (a *agentAdapter) HasRunningAgent(taskID string) bool {
	return a.agents.HasRunningAgentForTask(taskID)
}

func (a *agentAdapter) HasOtherRunningAgentForTask(taskID, exceptAgentID string) bool {
	return a.agents.HasOtherRunningAgentForTask(taskID, exceptAgentID)
}

func (a *agentAdapter) FindRunningAgentForRole(taskID, role string) (string, bool) {
	r := agent.Role(role)
	ag := a.agents.FindRunningAgentForTask(taskID, r)
	if ag == nil {
		return "", false
	}
	return ag.ID, true
}

func (a *agentAdapter) StopAgentsForTask(taskID, role string) {
	r := agent.Role(role)
	for _, ag := range a.agents.FindAllRunningAgentsForTask(taskID, r) {
		_ = a.agents.StopAgent(ag.ID)
	}
}

func (a *agentAdapter) SendPrompt(agentID, message string) error {
	return a.agents.SendPromptToAgent(agentID, message)
}

func (a *agentAdapter) DefaultProvider() string {
	return a.agents.DefaultProvider()
}

func (a *agentAdapter) TryClaimDispatch(taskID string) (workflow.DispatchClaim, bool) {
	return a.agents.TryClaimDispatch(taskID)
}

func (a *agentAdapter) ProviderRateLimited(provider string) bool {
	return a.agents.ProviderRateLimited(provider)
}

func (a *agentAdapter) ProviderCanFailover(provider string) bool {
	return a.agents.ProviderCanFailover(provider)
}

func (a *agentAdapter) ProviderHealthy(provider string) bool {
	return a.agents.ProviderHealthy(provider)
}

func (a *agentAdapter) IsDispatching(taskID string) bool {
	return a.agents.IsDispatching(taskID)
}

func (a *agentAdapter) AdmitDispatch(string, string, string) (admit bool, reason string) {
	return true, ""
}
