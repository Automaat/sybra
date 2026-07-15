package sybra

import (
	"fmt"

	"github.com/Automaat/sybra/internal/workflow"
)

// WorkflowService exposes workflow operations as Wails-bound methods.
type WorkflowService struct {
	engine *workflow.Engine
	store  *workflow.Store
}

// ListWorkflows returns all workflow definitions.
func (s *WorkflowService) ListWorkflows() ([]workflow.Definition, error) {
	return s.store.List()
}

// GetWorkflow returns a workflow definition by ID.
func (s *WorkflowService) GetWorkflow(id string) (workflow.Definition, error) {
	return s.store.Get(id)
}

// SaveWorkflow creates or updates a workflow definition.
func (s *WorkflowService) SaveWorkflow(def workflow.Definition) error {
	return s.store.Save(def)
}

// DeleteWorkflow removes a workflow definition.
func (s *WorkflowService) DeleteWorkflow(id string) error {
	return s.store.Delete(id)
}

// ResetBuiltin resets a built-in workflow to its default definition.
func (s *WorkflowService) ResetBuiltin(id string) error {
	defs, err := workflow.BuiltinDefinitions()
	if err != nil {
		return err
	}
	for i := range defs {
		if defs[i].ID == id {
			return s.store.Save(defs[i])
		}
	}
	return fmt.Errorf("builtin workflow %s not found", id)
}

// StartWorkflow assigns and starts a workflow on a task.
func (s *WorkflowService) StartWorkflow(taskID, workflowID string) error {
	return s.engine.StartWorkflow(taskID, workflowID)
}

// HandleHumanAction processes approve/reject/input for a waiting workflow step.
func (s *WorkflowService) HandleHumanAction(taskID, action string, data map[string]string) error {
	return s.engine.HandleHumanAction(taskID, action, data)
}
