package sybra

import "github.com/Automaat/sybra/internal/sybra/completion"

// newAgentCompletionHandler constructs the handler with every dependency
// the App holds. Called from wireServices once all subsystems are
// initialized — by then loopSched, humanReview, workflowEngine, and stats
// are either populated or intentionally nil (degraded init), and the
// handler's nil-checks at call time handle the latter.
func (a *App) newAgentCompletionHandler(emit func(string, any)) *completion.Handler {
	return completion.New(completion.Config{
		Logger:         a.logger,
		Audit:          a.audit,
		Emit:           emit,
		Tasks:          a.tasks,
		Worktrees:      a.worktrees,
		Sandboxes:      a.sandboxes,
		WorkflowEngine: a.workflowEngine,
		Stats:          a.stats,
		Limits:         a.limits,
		LoopSched:      a.loopSched,
		PRTracker:      a.prTracker,
		Cfg:            a.cfg,
		// a.humanReview may be nil (feature disabled) — the method value
		// still binds cleanly and onComplete guards for a nil receiver.
		HumanReviewComplete: a.humanReview.onComplete,
		// a.reviewer.RecoverStaleBranchConflict is nil-receiver-safe (see its
		// own guard), same pattern as agentOrch.SetConflictRecovery.
		ConflictRecovery: a.reviewer.RecoverStaleBranchConflict,
	})
}

func (a *App) wireCompletionHandlers(emit func(string, any)) {
	// Build completion handlers last: by this point every dependency they read
	// is wired up, and the manager's construction-time callback delegates to
	// a.agentCompletion once it is populated here.
	a.agentCompletion = a.newAgentCompletionHandler(emit)
	if a.workflowEngine != nil {
		a.workflowEngine.SetOnComplete(a.agentCompletion.OnWorkflowComplete)
	}
}
