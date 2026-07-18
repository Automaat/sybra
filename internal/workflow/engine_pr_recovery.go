package workflow

import (
	"context"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
)

const readyPRRecoveryReason = "manual verification requires a live PR and the branch is already pushed — routing to PR flow instead of parking human-required"

// maybeRecoverHumanRequiredByOpeningPR rewrites a narrow self-escalation class:
// a run_agent step parked the task at human-required only because live
// verification needed an open PR, while the branch itself is already pushed.
//
// In that case Sybra has enough information to continue autonomously:
//   - if a matching open PR already exists, link it and move to in-review
//   - otherwise flip to ready-pr so simple-task-pr opens or links it
//
// The current workflow is then completed immediately so its own next edge does
// not continue advancing under the recovered status.
func (e *Engine) maybeRecoverHumanRequiredByOpeningPR(taskID string, currentStep *Step, wfExec *Execution, t TaskInfo, output StepOutput) (*CompletionInfo, bool, error) {
	if currentStep == nil || currentStep.Type != StepRunAgent || output.Status != "completed" {
		return nil, false, nil
	}
	if t.Status != "human-required" || t.PRNumber != 0 || t.ProjectID == "" {
		return nil, false, nil
	}
	if !isMissingLivePRVerificationBlocker(t.StatusReason + "\n" + output.Output) {
		return nil, false, nil
	}
	if e.worktrees == nil {
		return nil, false, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok || wtPath == "" {
		return nil, false, nil
	}

	ctx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()
	branch, headArg, err := recoveryPRHead(ctx, wtPath, t.Branch)
	if err != nil {
		e.logger.Warn("workflow.human-required.pr-recovery.resolve-head", "task_id", taskID, "err", err)
		return nil, false, nil
	}
	remote := project.PushRemote(ctx, wtPath)
	remoteSHA, err := project.RemoteBranchHead(ctx, wtPath, remote, branch)
	if err != nil {
		e.logger.Warn("workflow.human-required.pr-recovery.remote-head", "task_id", taskID, "remote", remote, "branch", branch, "err", err)
		return nil, false, nil
	}
	if remoteSHA == "" {
		e.logger.Info("workflow.human-required.pr-recovery.remote-missing", "task_id", taskID, "remote", remote, "branch", branch)
		return nil, false, nil
	}

	status := "ready-pr"
	reason := readyPRRecoveryReason
	if existing, found := e.findExistingPRForBranch(t.ProjectID, headArg); found {
		if err := e.tasks.UpdateTaskPR(taskID, existing); err != nil {
			return nil, false, err
		}
		status = "in-review"
		reason = ""
	}
	if err := e.tasks.UpdateTaskStatus(taskID, status, reason); err != nil {
		return nil, false, err
	}
	e.logger.Info("workflow.human-required.pr-recovery", "task_id", taskID, "status", status, "branch", branch)
	comp, err := e.completeWorkflowForStatusRedirect(taskID, wfExec)
	return comp, true, err
}

func recoveryPRHead(ctx context.Context, wtPath, branch string) (resolvedBranch, headArg string, err error) {
	resolvedBranch = strings.TrimSpace(branch)
	if resolvedBranch == "" {
		resolvedBranch, err = project.CurrentBranch(ctx, wtPath)
		if err != nil {
			return "", "", err
		}
	}
	headArg, err = project.HeadArg(ctx, wtPath, resolvedBranch)
	if err != nil {
		return "", "", err
	}
	return resolvedBranch, headArg, nil
}

func isMissingLivePRVerificationBlocker(reason string) bool {
	r := strings.ToLower(strings.TrimSpace(reason))
	if r == "" || !strings.Contains(r, "manual verification blocker") {
		return false
	}
	if !strings.Contains(r, "open pr") {
		return false
	}
	return strings.Contains(r, "no current open pr") ||
		strings.Contains(r, "required live") ||
		strings.Contains(r, "live pr-monitor") ||
		strings.Contains(r, "live proof")
}

func (e *Engine) completeWorkflowForStatusRedirect(taskID string, wfExec *Execution) (*CompletionInfo, error) {
	now := time.Now().UTC()
	wfExec.State = ExecCompleted
	wfExec.CompletedAt = &now
	wfExec.CurrentStep = ""
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return nil, err
	}
	return &CompletionInfo{
		TaskID:     taskID,
		WorkflowID: wfExec.WorkflowID,
		Variables:  wfExec.Variables,
	}, nil
}
