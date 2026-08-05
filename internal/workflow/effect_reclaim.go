package workflow

// reclaimFenceRetries bounds re-reads when a concurrent boot write invalidates
// the fence. Reattach and restart-stale keep writing tasks while this sweep
// runs, and a lost fence would otherwise restore the full-TTL stall until the
// next process start — this sweep is boot-only, nothing retries it later.
const reclaimFenceRetries = 2

// ReclaimOrphanedEffectLeases releases in-flight effect claims left behind by a
// previous engine instance.
//
// An effect lease has no heartbeat, so its TTL is deliberately generous
// (defaultEffectLeaseTTL) to avoid reclaiming a step that is merely slow. That
// reasoning only holds while the owner is alive. A restart mints a fresh owner
// id, so every claim the previous instance held becomes unreclaimable by the
// live engine for the remainder of its TTL: `ClaimEffect` takes the
// owner-mismatch branch and `resume-stalled` re-fences the same step every tick
// until the lease lapses. A process that no longer exists cannot be mid-step,
// so at boot those claims can be expired outright.
//
// Call once at startup, after agent reattach and before effect replay.
// Reattach matters: a survive-restart agent outlives the engine that spawned
// it, and its step's claim is exactly the kind this would otherwise reclaim.
// Releasing it would let a second agent start for a step that is still running,
// so any task with a live or dispatching agent is skipped and left to the TTL.
func (e *Engine) ReclaimOrphanedEffectLeases() int {
	if e == nil || e.tasks == nil {
		return 0
	}
	// On a leader mirroring a follower's execution, "owner is not mine" means
	// the peer still running the step, not an orphan.
	if e.dispatchDisabled.Load() {
		return 0
	}
	tasks, err := e.tasks.ListTasks()
	if err != nil {
		e.logger.Error("workflow.effect.reclaim.list", "err", err)
		return 0
	}
	reclaimed := 0
	for i := range tasks {
		reclaimed += e.reclaimTaskEffectLeases(&tasks[i])
	}
	if reclaimed > 0 {
		e.logger.Info("workflow.effect.reclaim", "effects", reclaimed, "owner", e.ownerID)
	}
	return reclaimed
}

// reclaimEligible reports whether this task is one this instance may rewrite at
// all. Tasks homed on a remote follower, terminal workflows, and tasks with a
// live agent are all left alone.
func (e *Engine) reclaimEligible(t *TaskInfo) bool {
	if t.Workflow == nil || len(t.Workflow.EffectLog) == 0 {
		return false
	}
	if t.Workflow.State == ExecCompleted || t.Workflow.State == ExecFailed {
		return false
	}
	if e.dispatchGate != nil && !e.dispatchGate(*t) {
		return false
	}
	if e.agents != nil && (e.agents.HasRunningAgent(t.ID) || e.agents.IsDispatching(t.ID)) {
		return false
	}
	return true
}

// orphanedLeaseSteps nils the lease on every record this engine may reclaim and
// returns their step ids for logging. A lapsed lease already fences nobody, so
// rewriting it would bump the task generation for no gain — and a generation
// bump re-opens status-effect dedupe, which keys on exactly one generation back.
func (e *Engine) orphanedLeaseSteps(wf *Execution) []string {
	now := e.now()
	var steps []string
	for i := range wf.EffectLog {
		rec := &wf.EffectLog[i]
		switch {
		case rec.CompletedAt != nil,
			rec.Owner == "",
			rec.Owner == e.ownerID,
			!rec.leaseActiveAt(now):
			continue
		}
		rec.LeaseExpiresAt = nil
		steps = append(steps, rec.ID.StepID+"="+rec.Owner)
	}
	return steps
}

func (e *Engine) reclaimTaskEffectLeases(t *TaskInfo) int {
	for attempt := range reclaimFenceRetries {
		if !e.reclaimEligible(t) {
			return 0
		}
		next := t.Workflow.Clone()
		if next == nil {
			return 0
		}
		steps := e.orphanedLeaseSteps(next)
		if len(steps) == 0 {
			return 0
		}
		applied, err := e.tasks.SetWorkflowIf(t.ID, WorkflowWriteFence{
			Generation:   t.Generation,
			Status:       t.Status,
			StatusReason: t.StatusReason,
			WorkflowID:   t.Workflow.WorkflowID,
			CurrentStep:  t.Workflow.CurrentStep,
			State:        t.Workflow.State,
		}, next)
		if err != nil {
			e.logger.Error("workflow.effect.reclaim.persist", "task_id", t.ID, "err", err)
			return 0
		}
		if applied {
			t.Workflow = next
			e.logger.Info("workflow.effect.reclaim.orphan",
				"task_id", t.ID, "effects", len(steps), "stale", steps)
			return len(steps)
		}
		fresh, freshErr := e.tasks.GetTask(t.ID)
		if freshErr != nil || attempt == reclaimFenceRetries-1 {
			e.logger.Info("workflow.effect.reclaim.stale", "task_id", t.ID, "attempts", attempt+1)
			return 0
		}
		*t = fresh
	}
	return 0
}
