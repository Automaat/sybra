package sybra

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// PlanningService exposes task planning operations as Wails-bound methods.
type PlanningService struct {
	engine *workflow.Engine
	tasks  *task.Manager
	agents *agent.Manager
}

// TriageTask starts the triage workflow for the given task. Idempotent:
// returns nil if a workflow is already running or being started concurrently
// (CreateTask spawns the same workflow in a goroutine).
func (s *PlanningService) TriageTask(id string) error {
	if err := s.engine.StartWorkflow(id, "simple-task-plan"); err != nil {
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			return nil
		}
		return err
	}
	return nil
}

// PlanTask starts a workflow for the given task if none is active.
// If a workflow is already running — or being started concurrently
// (CreateTask auto-spawns one in a goroutine) — this is a no-op so the
// UI Plan button stays idempotent.
func (s *PlanningService) PlanTask(id string) error {
	t, err := s.tasks.Get(id)
	if err != nil {
		return err
	}
	if t.Workflow != nil && t.Workflow.State != "" {
		return nil
	}
	if err := s.engine.StartWorkflow(id, "simple-task-plan"); err != nil {
		// Concurrent auto-start (from CreateTask) holds the per-task start
		// lock — that's also a no-op outcome from the user's perspective.
		if errors.Is(err, workflow.ErrWorkflowAlreadyActive) {
			return nil
		}
		return err
	}
	return nil
}

// ApprovePlan approves the plan via the workflow engine.
func (s *PlanningService) ApprovePlan(id string) (task.Task, error) {
	return s.approve(id)
}

// RejectPlan rejects the plan with optional feedback via the workflow engine.
func (s *PlanningService) RejectPlan(id, feedback string) (task.Task, error) {
	return s.reject(id, feedback)
}

// SendPlanMessage sends a message to a live interactive plan agent.
func (s *PlanningService) SendPlanMessage(id, message string) error {
	return s.sendMessage(id, message, agent.RolePlan)
}

// HasLivePlanAgent reports whether a live plan agent exists for the task.
func (s *PlanningService) HasLivePlanAgent(id string) bool {
	return s.agents.FindRunningAgentForTask(id, agent.RolePlan) != nil
}

func (s *PlanningService) approve(id string) (task.Task, error) {
	if err := s.handlePlanReviewAction(id, "approve", nil); err != nil {
		return task.Task{}, err
	}
	return s.tasks.Get(id)
}

func (s *PlanningService) handlePlanReviewAction(id, action string, data map[string]string) error {
	return s.engine.HandleHumanActionRecovering(id, action, data, recoverCompletedPlanReview)
}

func recoverCompletedPlanReview(t workflow.TaskInfo) (*workflow.Execution, bool, error) {
	if t.Status != string(task.StatusPlanReview) || t.Workflow == nil ||
		t.Workflow.WorkflowID != "simple-task-plan" ||
		t.Workflow.CurrentStep != "" ||
		t.Workflow.State != workflow.ExecCompleted {
		return nil, false, nil
	}
	if problems := completedPlanReviewRecoveryProblems(t); len(problems) > 0 {
		return nil, false, fmt.Errorf("cannot recover plan review workflow for task %s: %s", t.ID, strings.Join(problems, "; "))
	}

	wf := *t.Workflow
	wf.CurrentStep = "review_plan"
	wf.State = workflow.ExecWaiting
	wf.CompletedAt = nil
	wf.Variables = clearHumanActionVars(t.Workflow.Variables)
	return &wf, true, nil
}

func clearHumanActionVars(vars map[string]string) map[string]string {
	if len(vars) == 0 {
		return vars
	}
	cleared := make(map[string]string, len(vars))
	for k, v := range vars {
		if k == "human_action" || strings.HasPrefix(k, "human.") {
			continue
		}
		cleared[k] = v
	}
	return cleared
}

func completedPlanReviewRecoveryProblems(t workflow.TaskInfo) []string {
	var problems []string
	required := map[string]string{
		"plan":           t.Plan,
		"plan_research":  t.PlanResearch,
		"plan_decisions": t.PlanDecisions,
		"plan_brief":     t.PlanBrief,
	}
	for name, content := range required {
		if strings.TrimSpace(content) == "" {
			problems = append(problems, name+" is required")
		}
	}
	if strings.TrimSpace(t.PlanContract) != "" {
		for _, problem := range workflow.ValidatePlanContractForTask(t.PlanContract, t.ID, t.Body) {
			problems = append(problems, "plan_contract "+problem)
		}
	}
	if !hasCompletedStep(t.Workflow, "validate_plan_contract") &&
		!hasCompletedStep(t.Workflow, "validate_plan_contract_after_address") {
		problems = append(problems, "plan validation step did not complete")
	}
	return problems
}

func hasCompletedStep(wf *workflow.Execution, stepID string) bool {
	if wf == nil {
		return false
	}
	rec := wf.RecordForStep(stepID)
	return rec != nil && rec.Status == "completed"
}

// reject forwards an optional human-typed feedback plus any unresolved
// inline review comments to the workflow engine as the "reject" action.
// Used by the plan-review flow.
func (s *PlanningService) reject(id, feedback string) (task.Task, error) {
	data := map[string]string{}
	combined := s.assembleFeedback(id, feedback)
	if combined != "" {
		data["feedback"] = combined
	}

	if err := s.handlePlanReviewAction(id, "reject", data); err != nil {
		return task.Task{}, err
	}
	_ = s.tasks.Comments().ResolveAll(id)
	return s.tasks.Get(id)
}

// sendMessage delivers a follow-up message to a live interactive agent of
// the given role. Unresolved inline comments are merged into the message
// so the two review-page buttons (Reject / Send Message) behave
// consistently — the UI placeholder promises "comments are included
// automatically" and that promise applies to both.
func (s *PlanningService) sendMessage(id, message string, role agent.Role) error {
	combined := s.assembleFeedback(id, message)
	if strings.TrimSpace(combined) == "" {
		return fmt.Errorf("message is empty")
	}
	ag := s.agents.FindRunningAgentForTask(id, role)
	if ag == nil || ag.Mode != "interactive" {
		return fmt.Errorf("no live interactive %s agent for task %s", role, id)
	}
	return s.agents.SendPromptToAgent(ag.ID, combined)
}

// assembleFeedback merges free-text feedback with any unresolved inline
// review comments into a single prompt body. Returns "" if both inputs
// are empty. Shared by reject and sendMessage so the two code paths
// cannot drift in how they format comments.
func (s *PlanningService) assembleFeedback(taskID, feedback string) string {
	trimmed := strings.TrimSpace(feedback)

	comments, err := s.tasks.Comments().List(taskID)
	if err != nil {
		return trimmed
	}
	var unresolvedLines []string
	for _, c := range comments {
		if !c.Resolved {
			unresolvedLines = append(unresolvedLines, fmt.Sprintf("- Line %d: %s", c.Line, c.Body))
		}
	}
	if len(unresolvedLines) == 0 {
		return trimmed
	}

	commentSection := "Unresolved review comments:\n" + strings.Join(unresolvedLines, "\n")
	if trimmed == "" {
		return commentSection
	}
	return trimmed + "\n\n" + commentSection
}
