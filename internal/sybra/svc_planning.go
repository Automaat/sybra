package sybra

import (
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

// TriageTask starts the triage workflow for the given task.
func (s *PlanningService) TriageTask(id string) error {
	return s.engine.StartWorkflow(id, "simple-task")
}

// PlanTask starts a workflow for the given task if none is active.
// If a workflow is already running, this is a no-op — the engine drives
// plan steps via transitions.
func (s *PlanningService) PlanTask(id string) error {
	t, err := s.tasks.Get(id)
	if err != nil {
		return err
	}
	if t.Workflow != nil && t.Workflow.State != "" {
		return nil
	}
	return s.engine.StartWorkflow(id, "simple-task")
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

// ApproveTestPlan approves the manual test plan via the workflow engine.
// Workflow-engine-level same as ApprovePlan; named separately so the frontend
// binding stays explicit about which review page is acting.
func (s *PlanningService) ApproveTestPlan(id string) (task.Task, error) {
	return s.approve(id)
}

// RejectTestPlan rejects the manual test plan with optional feedback.
func (s *PlanningService) RejectTestPlan(id, feedback string) (task.Task, error) {
	return s.reject(id, feedback)
}

// SendTestPlanMessage sends a message to a live interactive test-plan agent.
func (s *PlanningService) SendTestPlanMessage(id, message string) error {
	return s.sendMessage(id, message, agent.RoleTestPlan)
}

// HasLiveTestPlanAgent reports whether a live test-plan agent exists for the task.
func (s *PlanningService) HasLiveTestPlanAgent(id string) bool {
	return s.agents.FindRunningAgentForTask(id, agent.RoleTestPlan) != nil
}

func (s *PlanningService) approve(id string) (task.Task, error) {
	if err := s.engine.HandleHumanAction(id, "approve", nil); err != nil {
		return task.Task{}, err
	}
	return s.tasks.Get(id)
}

// reject forwards an optional human-typed feedback plus any unresolved
// inline review comments to the workflow engine as the "reject" action.
// Shared by the plan-review and test-plan-review flows.
func (s *PlanningService) reject(id, feedback string) (task.Task, error) {
	data := map[string]string{}
	combined := s.assembleFeedback(id, feedback)
	if combined != "" {
		data["feedback"] = combined
	}

	if err := s.engine.HandleHumanAction(id, "reject", data); err != nil {
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
