package sybra

import (
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/reconcile"
	"github.com/Automaat/sybra/internal/sybra/completion"
	"github.com/Automaat/sybra/internal/sybra/reconciliation"
)

func (a *App) postRunReconciliation() *reconciliation.Reconciler {
	if a == nil || a.tasks == nil {
		return nil
	}
	if a.postRunReconciler == nil {
		a.postRunReconciler = reconciliation.New(reconciliation.Config{
			Tasks: a.tasks, Projects: a.projects, Worktrees: a.worktrees, Logger: a.logger, Evidence: a.evidenceStore,
			Audit: func(req reconcile.Request, plan reconcile.Plan) {
				taskID, projectScope, confidential := a.reconciliationAuditIdentity(req.TaskID)
				a.logAudit(audit.EventReconciliationDecided, taskID, "", map[string]any{
					"run_id": req.RunID, "intent": string(req.Intent), "action": string(plan.Action),
					"decision_code": "reconcile." + string(plan.Action),
					"project_scope": projectScope, "confidential": confidential,
				})
			},
		})
		if a.worktrees != nil {
			a.worktrees.SetCleanupGate(a.postRunReconciler.CanCleanup)
		}
	}
	return a.postRunReconciler
}

func (a *App) reconciliationAuditIdentity(taskID string) (safeTaskID, projectScope string, confidential bool) {
	safeTaskID, projectScope = taskID, "fleet"
	t, err := a.tasks.Get(taskID)
	if err != nil {
		if taskID != "" {
			return experience.WorkRecordID(taskID), "work-unknown", true
		}
		return safeTaskID, projectScope, false
	}
	if t.ProjectID == "" {
		return safeTaskID, projectScope, false
	}
	// A missing project store is a degraded classification path. Fail closed:
	// retain useful typed audit evidence without exposing the task identity.
	if a.projects == nil {
		return experience.WorkRecordID(taskID), "work-unknown", true
	}
	p, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return experience.WorkRecordID(taskID), "work-unknown", true
	}
	projectScope = experience.ProjectKey(p)
	confidential = a.workScrubContextForTask(t.ProjectID) != nil
	if confidential {
		safeTaskID = experience.WorkRecordID(taskID)
	}
	return safeTaskID, projectScope, confidential
}

// newAgentCompletionHandler constructs the handler with every dependency
// the App holds. Called from wireServices once all subsystems are
// initialized — by then loopSched, humanReview, workflowEngine, and stats
// are either populated or intentionally nil (degraded init), and the
// handler's nil-checks at call time handle the latter.
func (a *App) newAgentCompletionHandler(emit func(string, any)) *completion.Handler {
	cfg := completion.Config{
		Logger:     a.logger,
		Audit:      a.audit,
		Emit:       emit,
		Tasks:      a.tasks,
		Worktrees:  a.worktrees,
		Sandboxes:  a.sandboxes,
		Stats:      a.stats,
		Limits:     a.limits,
		LoopSched:  a.loopSched,
		PRTracker:  a.prTracker,
		Cfg:        a.cfg,
		Artifacts:  a.artifacts,
		Reconciler: a.postRunReconciliation(),
		WorkScrub: func(projectID string) *completion.WorkScrubContext {
			ctx := a.workScrubContextForTask(projectID)
			if ctx == nil {
				return nil
			}
			return &completion.WorkScrubContext{Blocklist: ctx.Blocklist}
		},
		// a.humanReview may be nil (feature disabled) — the method value
		// still binds cleanly and onComplete guards for a nil receiver.
		HumanReviewComplete: a.humanReview.onComplete,
		RunResultPersisted:  a.remoteResultPersisted,
	}
	// a.workflowEngine is declared *workflow.Engine, so a nil pointer would
	// become a non-nil CompletionWorkflow interface (typed-nil gotcha) and
	// defeat the handler's `workflowEngine == nil` guards. Only populate the
	// interface field (and the engine-backed ConflictRecovery hook) when the
	// concrete engine actually initialized — SYBRA_DISABLE_WORKFLOWS=1 or a
	// workflow-store init failure leaves it nil (degraded init).
	if a.workflowEngine != nil {
		cfg.WorkflowEngine = a.workflowEngine
		// Routed through workflowEngine.TryConflictRecovery, not
		// reviewer.RecoverStaleBranchConflict directly: completion can fire
		// while a same-task StartWorkflow call is still active elsewhere, and
		// TryConflictRecovery queues the retry instead of re-entering into
		// ErrWorkflowAlreadyActive.
		cfg.ConflictRecovery = a.workflowEngine.TryConflictRecovery
	}
	return completion.New(cfg)
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
