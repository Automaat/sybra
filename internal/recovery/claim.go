package recovery

import "sync"

// RecoveryClaim is a held per-task recovery claim returned by
// TryClaimRecovery. The holder MUST call Release exactly once when its
// recovery decision for the task is complete (success or failure) —
// typically via `defer claim.Release()`. Release is idempotent and nil-safe.
type RecoveryClaim struct {
	r        *Recovery
	taskID   string
	mu       sync.Mutex
	released bool
}

// TryClaimRecovery reserves the sole right to apply a recovery decision for
// taskID, returning ok=false when another recovery pass already holds the
// claim for it.
//
// Two independent entry points can race on the same task: the periodic
// RestartStaleInProgress sweep (the orchestrator's maintenance ticker) and a
// targeted RestartTaskIfStale call fired from the cluster monitor's
// lost-agent recovery, each running on its own goroutine. Without this claim
// both can read the same stale task snapshot (no running agent, an
// unprocessed workflow step) and independently apply the recovery decision —
// e.g. both firing WorkflowEngine.HandleAgentComplete for the same completed
// run — double-advancing the workflow.
//
// restartTaskIfStale is the sole caller: it claims right after the cheap,
// side-effect-free guards (task type, dispatch gate, status, running-agent,
// in-flight-dispatch) and defers Release, so the claim spans every
// synchronous decision branch (recoverCompletedHeadlessRun,
// recoverCancelledPRFix, handleTerminalWorkflow, the interactive-recovery
// path) of that task's recovery pass.
func (r *Recovery) TryClaimRecovery(taskID string) (claim *RecoveryClaim, ok bool) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()
	if r.recoveryClaims == nil {
		r.recoveryClaims = make(map[string]struct{})
	}
	if _, held := r.recoveryClaims[taskID]; held {
		return nil, false
	}
	r.recoveryClaims[taskID] = struct{}{}
	return &RecoveryClaim{r: r, taskID: taskID}, true
}

// Release frees the claim. Idempotent and nil-safe.
func (c *RecoveryClaim) Release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return
	}
	c.released = true
	c.mu.Unlock()
	c.r.recoveryMu.Lock()
	delete(c.r.recoveryClaims, c.taskID)
	c.r.recoveryMu.Unlock()
}
