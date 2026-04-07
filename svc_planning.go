package main

import (
	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
)

// PlanningService exposes task planning operations as Wails-bound methods.
type PlanningService struct {
	workflow *TaskWorkflow
	agents   *agent.Manager
}

// TriageTask runs the triage agent for the given task.
func (s *PlanningService) TriageTask(id string) error {
	return s.workflow.TriageTask(id)
}

// PlanTask runs the planning agent for the given task.
func (s *PlanningService) PlanTask(id string) error {
	return s.workflow.PlanTask(id)
}

// ApprovePlan approves the plan and moves the task to in-progress.
func (s *PlanningService) ApprovePlan(id string) (task.Task, error) {
	return s.workflow.ApprovePlan(id)
}

// RejectPlan rejects the plan with optional feedback.
func (s *PlanningService) RejectPlan(id, feedback string) (task.Task, error) {
	return s.workflow.RejectPlan(id, feedback)
}

// SendPlanMessage sends a message to a live interactive plan agent.
func (s *PlanningService) SendPlanMessage(id, message string) error {
	return s.workflow.SendPlanMessage(id, message)
}

// HasLivePlanAgent reports whether a live plan agent exists for the task.
func (s *PlanningService) HasLivePlanAgent(id string) bool {
	return s.agents.FindRunningAgentForTask(id, agent.RolePlan) != nil
}

// EvaluateTask runs the eval agent to assess the task agent's result.
func (s *PlanningService) EvaluateTask(taskID, agentResult string) error {
	return s.workflow.EvaluateTask(taskID, agentResult)
}
