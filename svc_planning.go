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

// PlanTask starts a planning workflow step for the given task.
func (s *PlanningService) PlanTask(id string) error {
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
func (s *PlanningService) RejectPlan(id, feedback string) (task.Task, error) {
	data := map[string]string{}
	if feedback != "" {
		data["feedback"] = feedback
	}

	// Append unresolved inline comments to feedback.
	comments, _ := s.tasks.Comments().List(id)
	var unresolvedLines []string
	for _, c := range comments {
		if !c.Resolved {
			unresolvedLines = append(unresolvedLines, fmt.Sprintf("- Line %d: %s", c.Line, c.Body))
		}
	}
	if len(unresolvedLines) > 0 {
		commentSection := "Unresolved review comments:\n" + strings.Join(unresolvedLines, "\n")
		if data["feedback"] != "" {
			data["feedback"] += "\n\n" + commentSection
		} else {
			data["feedback"] = commentSection
		}
	}

	if err := s.engine.HandleHumanAction(id, "reject", data); err != nil {
		return task.Task{}, err
	}
	return s.tasks.Get(id)
}

// SendPlanMessage sends a message to a live interactive plan agent.
func (s *PlanningService) SendPlanMessage(id, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("message is empty")
	}
	planAg := s.agents.FindRunningAgentForTask(id, agent.RolePlan)
	if planAg == nil || planAg.Mode != "interactive" {
		return fmt.Errorf("no live interactive plan agent for task %s", id)
	}
	return s.agents.SendPromptToAgent(planAg.ID, message)
}

// HasLivePlanAgent reports whether a live plan agent exists for the task.
func (s *PlanningService) HasLivePlanAgent(id string) bool {
	return s.agents.FindRunningAgentForTask(id, agent.RolePlan) != nil
}

// EvaluateTask is no longer needed — the workflow engine handles evaluation
// as a step in the workflow. Kept as a no-op for API compatibility.
func (s *PlanningService) EvaluateTask(_, _ string) error {
	return nil
}
