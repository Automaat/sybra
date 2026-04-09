package main

import (
	"fmt"
	"strings"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
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
	if err := s.engine.HandleHumanAction(id, "approve", nil); err != nil {
		return task.Task{}, err
	}
	return s.tasks.Get(id)
}

// RejectPlan rejects the plan with optional feedback via the workflow engine.
// Unresolved inline review comments are appended to the feedback
// automatically so the plan agent sees them on replan.
func (s *PlanningService) RejectPlan(id, feedback string) (task.Task, error) {
	data := map[string]string{}
	combined := s.assembleFeedback(id, feedback)
	if combined != "" {
		data["feedback"] = combined
	}

	if err := s.engine.HandleHumanAction(id, "reject", data); err != nil {
		return task.Task{}, err
	}
	return s.tasks.Get(id)
}

// SendPlanMessage sends a follow-up message to a live interactive plan
// agent. Unresolved inline comments are appended to the message so the two
// plan-review buttons behave consistently — the UI placeholder promises
// "comments are included automatically" and that promise applies to both.
func (s *PlanningService) SendPlanMessage(id, message string) error {
	combined := s.assembleFeedback(id, message)
	if strings.TrimSpace(combined) == "" {
		return fmt.Errorf("message is empty")
	}
	planAg := s.agents.FindRunningAgentForTask(id, agent.RolePlan)
	if planAg == nil || planAg.Mode != "interactive" {
		return fmt.Errorf("no live interactive plan agent for task %s", id)
	}
	return s.agents.SendPromptToAgent(planAg.ID, combined)
}

// assembleFeedback merges free-text feedback with any unresolved inline
// review comments into a single prompt body. Returns "" if both inputs are
// empty. Shared between RejectPlan and SendPlanMessage so the two code
// paths cannot drift in how they format comments.
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

// HasLivePlanAgent reports whether a live plan agent exists for the task.
func (s *PlanningService) HasLivePlanAgent(id string) bool {
	return s.agents.FindRunningAgentForTask(id, agent.RolePlan) != nil
}
