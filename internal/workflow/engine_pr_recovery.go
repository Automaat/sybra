package workflow

import (
	"context"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/taskstatus"
)

const readyPRRecoveryReason = "manual verification requires a live PR and the branch is already pushed — routing to PR flow instead of parking human-required"
const alreadyFixedOnMainRecoveryReason = "requested change already satisfies origin base and the task branch has no remaining diff — closing duplicate task as done"

// maybeRecoverHumanRequiredAlreadyFixedOnMain rewrites a narrow duplicate-task
// class: an implementation step parked the task at human-required after
// determining the requested change was already on main, and the task branch is
// still clean with no commits ahead of the origin base. In that case Sybra has
// enough deterministic proof to close the task itself instead of parking it.
//
// Agent text is only a trigger hint here. On agent-completion recovery the hint
// must come from the current run's output, not a stale task status_reason from
// an older human-required park. The close decision itself still requires
// repo-state proof: no commits ahead, no uncommitted diff, and HEAD reachable
// from the origin base.
func (e *Engine) maybeRecoverHumanRequiredAlreadyFixedOnMain(taskID string, currentStep *Step, wfExec *Execution, t TaskInfo, output StepOutput, duplicateSignal string) (*CompletionInfo, bool, error) {
	if currentStep == nil || currentStep.Type != StepRunAgent || currentStep.Config.Role != "implementation" || output.Status != "completed" {
		return nil, false, nil
	}
	if t.Status != taskstatus.HumanRequired || t.PRNumber != 0 || t.ProjectID == "" {
		return nil, false, nil
	}
	if wfExec != nil && wfExec.LastAgentStepFailed() {
		return nil, false, nil
	}
	alreadyFixed, err := declaresAlreadyFixedOnMain(duplicateSignal)
	if err != nil {
		// Stay parked: guessing from the prose of a run that already failed to declare is the behaviour this replaces.
		e.logger.Warn("workflow.human-required.duplicate-recovery.unreadable-verdict", "task_id", taskID, "err", err)
		return nil, false, nil
	}
	if !alreadyFixed {
		return nil, false, nil
	}
	if e.worktrees == nil {
		return nil, false, nil
	}
	wtPath, ok := e.worktrees.GetWorktreePath(taskID)
	if !ok || wtPath == "" {
		return nil, false, nil
	}
	clean, err := e.branchAlreadySatisfiedOnMain(taskID, wtPath, t)
	if err != nil {
		e.logger.Warn("workflow.human-required.duplicate-recovery.inspect", "task_id", taskID, "err", err)
		return nil, false, nil
	}
	if !clean {
		return nil, false, nil
	}
	if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.Done, alreadyFixedOnMainRecoveryReason); err != nil {
		return nil, false, err
	}
	e.logger.Info("workflow.human-required.duplicate-recovery", "task_id", taskID)
	comp, err := e.completeWorkflowForStatusRedirect(taskID, wfExec)
	return comp, true, err
}

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
	if t.Status != taskstatus.HumanRequired || t.PRNumber != 0 || t.ProjectID == "" {
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

	localSHA, err := project.CurrentCommit(ctx, wtPath)
	if err != nil {
		e.logger.Warn("workflow.human-required.pr-recovery.local-head", "task_id", taskID, "err", err)
		return nil, false, nil
	}

	status := taskstatus.ReadyPR
	reason := readyPRRecoveryReason
	// Only take the link-existing-PR -> in-review shortcut when the local HEAD
	// being recovered is the exact commit already on the remote branch. If new
	// local commits are ahead of (or diverged from) the pushed branch, an
	// existing PR still points at the stale head; linking it and moving to
	// in-review would skip the push that the PR flow performs, so the new HEAD
	// would never reach the PR. Fall through to ready-pr in that case so
	// simple-task-pr pushes the current HEAD before linking.
	if remoteSHA == localSHA {
		if existing, found := e.findExistingPRForBranch(t.ProjectID, headArg); found {
			if err := e.linkTaskPR(taskID, t, existing); err != nil {
				return nil, false, err
			}
			status = taskstatus.InReview
			reason = ""
		}
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

func (e *Engine) branchAlreadySatisfiedOnMain(taskID, wtPath string, t TaskInfo) (bool, error) {
	output, err := e.gitLogAheadOfBaseWithRetry(taskID, wtPath, t)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(output)) != "" {
		return false, nil
	}
	if diagnoseWorktreeState(e.ctx, wtPath) != "clean tree, no commits ahead" {
		return false, nil
	}
	return branchMergedIntoBase(e.ctx, wtPath), nil
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
