package workflow

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
// Call once at startup, after agent reattach and before dispatch is armed.
// Reattach matters: a survive-restart agent outlives the engine that spawned
// it, and its step's claim is exactly the kind this would otherwise reclaim.
// Releasing it would let a second agent start for a step that is still running,
// so any task with a live agent is skipped and left to the TTL.
func (e *Engine) ReclaimOrphanedEffectLeases() int {
	if e == nil || e.tasks == nil {
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

func (e *Engine) reclaimTaskEffectLeases(t *TaskInfo) int {
	if t.Workflow == nil || len(t.Workflow.EffectLog) == 0 {
		return 0
	}
	if e.agents != nil && (e.agents.HasRunningAgent(t.ID) || e.agents.IsDispatching(t.ID)) {
		return 0
	}
	next := t.Workflow.Clone()
	if next == nil {
		return 0
	}
	orphans := 0
	for i := range next.EffectLog {
		rec := &next.EffectLog[i]
		if rec.CompletedAt != nil || rec.LeaseExpiresAt == nil {
			continue
		}
		if rec.Owner == "" || rec.Owner == e.ownerID {
			continue
		}
		e.logger.Info("workflow.effect.reclaim.orphan",
			"task_id", t.ID, "step", rec.ID.StepID, "effect", rec.ID.String(), "stale_owner", rec.Owner)
		rec.LeaseExpiresAt = nil
		orphans++
	}
	if orphans == 0 {
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
	if !applied {
		e.logger.Info("workflow.effect.reclaim.stale", "task_id", t.ID)
		return 0
	}
	t.Workflow = next
	return orphans
}
