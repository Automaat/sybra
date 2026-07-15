package workflow

import (
	"context"
	"encoding/json"
	"fmt"
)

// Sync outcome strings recorded by execSyncBranch. Mirror the SyncResult
// values in internal/worktree — kept as string literals here (not a shared
// type) because internal/workflow cannot import internal/worktree without
// creating an import cycle (internal/worktree -> internal/task ->
// internal/workflow). The BranchSyncer interface crosses that boundary as a
// plain string for the same reason.
const (
	syncResultSkipped = "skipped"
	syncResultFailed  = "failed"
)

// syncBranchReport is the structured result, stored as a generic artifact so
// conflict-at-PR and failure rates can be measured across tasks.
type syncBranchReport struct {
	Result string `json:"result"`
	Err    string `json:"err,omitempty"`
}

// execSyncBranch runs a proactive, best-effort sync of the task's worktree
// branch against the project's default branch. It never blocks workflow
// advancement: a nil BranchSyncer, a missing worktree, a sync error, or even
// a panic inside the syncer all resolve to a completed step with the outcome
// recorded in the step output and (when a recorder is wired) a generic
// artifact — never an error return, and never a task status change.
func (e *Engine) execSyncBranch(taskID string, step *Step) (out StepOutput, err error) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Warn("workflow.sync-branch.panic", "task_id", taskID, "recovered", r)
			out, err = e.recordSyncBranch(taskID, step, syncResultFailed, fmt.Sprintf("panic: %v", r))
		}
	}()

	if e.branchSyncer == nil {
		return e.recordSyncBranch(taskID, step, syncResultSkipped, "no branch syncer configured")
	}

	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	result, syncErr := e.branchSyncer.SyncTaskBranch(ctx, taskID)
	if syncErr != nil {
		e.logger.Warn("workflow.sync-branch.result", "task_id", taskID, "result", result, "err", syncErr)
		return e.recordSyncBranch(taskID, step, result, syncErr.Error())
	}
	e.logger.Info("workflow.sync-branch.result", "task_id", taskID, "result", result)
	return e.recordSyncBranch(taskID, step, result, "")
}

// recordSyncBranch stores the sync outcome as a generic artifact (best-effort)
// and returns a completed StepOutput carrying the same outcome, so the result
// is observable via structured log, step output, and artifact.
func (e *Engine) recordSyncBranch(taskID string, step *Step, result, detail string) (StepOutput, error) {
	report := syncBranchReport{Result: result, Err: detail}
	if e.recorder != nil {
		if data, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
			if recErr := e.recorder.PutGeneric(taskID, "sync-branch.json", step.ID, string(data)); recErr != nil {
				e.logger.Warn("workflow.sync-branch.artifact", "task_id", taskID, "err", recErr)
			}
		}
	}
	return stepDone(step, result)
}
