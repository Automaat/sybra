package workflow

import "github.com/Automaat/sybra/internal/taskstatus"

// These wrappers are the workflow engine's single choke point for task-visible
// mutations. Step handlers and workflow bookkeeping call them instead of
// reaching into TaskProvider directly so the claim-guarded effect paths remain
// mechanically auditable.

func (e *Engine) persistWorkflow(taskID string, wf *Execution) error {
	return e.tasks.SetWorkflow(taskID, wf)
}

func (e *Engine) persistStatus(taskID string, status taskstatus.Status, reason string) error {
	return e.tasks.UpdateTaskStatus(taskID, status, reason)
}

func (e *Engine) appendBodyNote(taskID, content string) error {
	return e.tasks.AppendTaskBody(taskID, content)
}

func (e *Engine) storeSidecar(taskID, kind, content string) error {
	return e.tasks.WriteSidecar(taskID, kind, content)
}

func (e *Engine) linkPRNumber(taskID string, prNumber int) error {
	return e.tasks.UpdateTaskPR(taskID, prNumber)
}

func (e *Engine) markReviewed(taskID string) error {
	return e.tasks.MarkTaskReviewed(taskID)
}
