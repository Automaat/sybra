package review

import (
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
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
		if tasks[i].TaskType == task.TaskTypeChat {
			continue
		}
		infos = append(infos, taskToInfo(tasks[i]))
	}
	return infos, nil
}

func (a *taskAdapter) UpdateTaskStatus(id, status, reason string) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	u := task.Update{Status: &st}
	if reason != "" {
		u.StatusReason = &reason
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

func (a *taskAdapter) AppendTaskBody(id, content string) error {
	_, err := a.tasks.AppendBody(id, content)
	return err
}

func (a *taskAdapter) ReplaceTaskBody(id, body string) error {
	_, err := a.tasks.Update(id, task.Update{Body: &body})
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.Update(id, task.Update{Workflow: &wf})
	return err
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
	default:
		return fmt.Errorf("unknown sidecar kind %q (want plan|plan_contract|code_review|plan_critique|plan_research|plan_decisions|plan_brief|plan_draft.<name>)", kind)
	}
	_, err := a.tasks.Update(id, u)
	return err
}

func taskToInfo(t task.Task) workflow.TaskInfo {
	return workflow.TaskInfo{
		ID:                    t.ID,
		Title:                 t.Title,
		Status:                string(t.Status),
		StatusReason:          t.StatusReason,
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
		PlanDrafts:            t.PlanDrafts,
		Issue:                 t.Issue,
		Reviewed:              t.Reviewed,
		Workflow:              t.Workflow,
		AgentRuns:             toRunInfos(t.AgentRuns),
		TestingCycleStartedAt: t.TestingCycleStartedAt,
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
