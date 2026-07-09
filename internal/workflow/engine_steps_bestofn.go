package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// bestOfNAttemptSep separates a best_of_n parent step ID from a synthetic
// attempt ID (e.g. "implement_n::attempt_2") in the compound stepID stashed
// in agentSteps for an attempt agent. Attempts have no corresponding YAML
// Step (unlike `parallel` children), so they cannot be looked up via
// Definition.StepByID — loadAdvanceContext instead detects this separator and
// routes to the best-of-N attempt path. "::" cannot collide with a real
// step ID validated elsewhere (step IDs are plain identifiers).
const bestOfNAttemptSep = "::"

func bestOfNAttemptID(n int) string { return fmt.Sprintf("attempt_%d", n) }

func bestOfNAttemptStepKey(parentID, attemptID string) string {
	return parentID + bestOfNAttemptSep + attemptID
}

// splitBestOfNAttemptStepKey reverses bestOfNAttemptStepKey.
func splitBestOfNAttemptStepKey(key string) (parentID, attemptID string, ok bool) {
	parent, attempt, found := strings.Cut(key, bestOfNAttemptSep)
	if !found {
		return "", "", false
	}
	return parent, attempt, true
}

func attemptProviderFor(providers []string, n int) string {
	if len(providers) == 0 {
		return ""
	}
	return providers[(n-1)%len(providers)]
}

func bestOfNManifestVar(stepID string) string   { return "bestofn." + stepID + ".manifest" }
func bestOfNSuccessfulVar(stepID string) string { return "bestofn." + stepID + ".successful" }

// execBestOfN fans out step.Config.Attempts implementation agents, each into
// its OWN isolated worktree (internal/worktree.Manager.PrepareAttempt) rather
// than the task's shared canonical worktree that `parallel` children use.
// Attempts dispatch through the EXISTING AgentLauncher.StartAgent with a
// pre-staged attempt dir and needsWorktree=false — no new launcher method is
// added; this routes through the direct-dispatch branch (agentAdapter.
// StartAgent), which does not itself enforce the cumulative task cost
// budget, so this preflights it explicitly and fails closed on
// ErrTaskCostExceeded.
func (e *Engine) execBestOfN(taskID string, def *Definition, step *Step, wfExec *Execution, ctx TemplateContext) (*CompletionInfo, error) {
	if e.costBudget != nil {
		if err := e.costBudget.CheckTaskCostBudget(taskID); err != nil {
			if errors.Is(err, ErrTaskCostExceeded) {
				return e.failStepClosed(taskID, def, step, wfExec,
					"best-of-n: cost budget exceeded before attempts started: "+err.Error())
			}
			return nil, err
		}
	}
	if e.attemptWorktrees == nil {
		return e.failStepClosed(taskID, def, step, wfExec,
			"best-of-n: no attempt worktree manager configured")
	}

	n := step.Config.Attempts

	if wfExec.BestOfNInflight == nil {
		wfExec.BestOfNInflight = make(map[string]*BestOfNInflight)
	}
	rec, exists := wfExec.BestOfNInflight[step.ID]
	if !exists || rec == nil {
		rec = &BestOfNInflight{
			ParentStepID: step.ID,
			StartedAt:    time.Now().UTC(),
			Attempts:     make(map[string]*AttemptStatus, n),
		}
		for i := 1; i <= n; i++ {
			id := bestOfNAttemptID(i)
			rec.Attempts[id] = &AttemptStatus{AttemptID: id, Provider: attemptProviderFor(step.Config.AttemptProviders, i), Status: "pending"}
		}
		wfExec.BestOfNInflight[step.ID] = rec
	}

	// Stop stale agents from prior steps once before spawning the fan-out
	// (mirrors execParallel).
	e.agents.StopAgentsForTask(taskID, "")
	e.clearAgentStepsForTask(taskID)

	wfExec.State = ExecWaiting
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return nil, err
	}

	for i := 1; i <= n; i++ {
		id := bestOfNAttemptID(i)
		status := rec.Attempts[id]
		if status == nil {
			status = &AttemptStatus{AttemptID: id, Provider: attemptProviderFor(step.Config.AttemptProviders, i)}
			rec.Attempts[id] = status
		}
		// Resume-safe: skip attempts that already terminated in a prior run.
		// A "pending" attempt found here means the engine restarted before
		// its agent's completion was routed (execBestOfN is only re-entered by
		// ResumeStalled when no agent is running for the task at all) — the
		// original agent is orphaned, so this respawns it.
		if status.Status == "completed" || status.Status == "failed" {
			continue
		}
		if err := e.spawnBestOfNAttempt(taskID, step, wfExec, ctx, id, status); err != nil {
			e.logger.Error("workflow.best-of-n.spawn", "task_id", taskID, "parent", step.ID, "attempt", id, "err", err)
			if transientAgentStartError(err) {
				status.Status = "pending"
				status.Output = "spawn blocked: " + err.Error()
				e.surfaceStartFailure(taskID, ctx.Task.Status, err, wfExec, bestOfNAttemptStepKey(step.ID, id))
				continue
			}
			status.Status = "failed"
			status.Output = "spawn failed: " + err.Error()
		}
	}

	if rec.AllAttemptsDone() {
		return e.finalizeBestOfNParent(taskID, def, step, wfExec)
	}
	return nil, e.tasks.SetWorkflow(taskID, wfExec)
}

// spawnBestOfNAttempt prepares one attempt's isolated worktree and dispatches
// its implementation agent. Mirrors spawnParallelChild's dispatch shape but
// pre-stages a per-attempt dir (via AttemptWorktreeManager) instead of
// sharing the parent's WorkflowVarDir, and stamps AgentAssignment so the
// AgentRun this produces is durably attributed to this attempt
// (VariantID=attempt_N, AssignmentUnit=bestofn-attempt) independent of the
// trimmable Execution state above.
func (e *Engine) spawnBestOfNAttempt(taskID string, step *Step, wfExec *Execution, parentCtx TemplateContext, attemptID string, status *AttemptStatus) error {
	dir, branch, err := e.attemptWorktrees.PrepareAttempt(taskID, attemptID)
	if err != nil {
		return fmt.Errorf("prepare attempt worktree: %w", err)
	}
	status.Dir = dir
	status.Branch = branch

	attemptCtx := parentCtx
	mode := step.Config.Mode
	if mode == "" || mode == "interactive" {
		// Attempts must terminate on their own so the parent can advance —
		// same rationale as spawnParallelChild's oneShot=false headless-only
		// requirement.
		mode = "headless"
	}
	model := step.Config.Model
	if model == "" {
		model = "sonnet"
	}
	provider := status.Provider
	if provider == "" {
		provider = resolveProvider(step.Config.Provider, wfExec, e.agents.DefaultProvider(), attemptCtx.Task)
	}
	if provider != "" && !providerAvailable(provider) {
		e.logger.Warn("workflow.best-of-n.provider-unavailable", "wanted", provider, "attempt", attemptID)
		provider = ""
	}
	status.Provider = provider
	status.Model = model

	prompt, err := RenderTemplate(step.Config.Prompt, attemptCtx)
	if err != nil {
		return fmt.Errorf("render prompt: %w", err)
	}

	assignment := AgentAssignment{
		VariantID:      attemptID,
		AssignmentUnit: "bestofn-attempt",
		Provider:       provider,
		Model:          model,
	}

	// Hold e.mu across StartAgent so a fast-exiting agent's completion cannot
	// race past the agentSteps registration below (mirrors spawnParallelChild).
	e.mu.Lock()
	agentID, _, _, startErr := e.agents.StartAgent(taskID, step.Config.Role, mode, model, provider, prompt, dir, step.Config.AllowedTools, false, false, step.Config.OutputSchema, "", assignment)
	if startErr != nil {
		e.mu.Unlock()
		return fmt.Errorf("start agent: %w", startErr)
	}
	e.agentSteps[agentID] = agentEntry{taskID: taskID, stepID: bestOfNAttemptStepKey(step.ID, attemptID)}
	e.mu.Unlock()

	status.AgentID = agentID
	status.Status = "pending"
	e.logger.Info("workflow.best-of-n.spawn",
		"task_id", taskID, "parent", step.ID, "attempt", attemptID,
		"agent_id", agentID, "provider", provider, "dir", dir)
	return nil
}

// advanceBestOfNAttempt records one attempt's completion. Per-attempt retry:
// a failed attempt is re-spawned (in the SAME isolated worktree — PrepareAttempt
// resumes rather than recreates a healthy checkout) up to step.Config.MaxRetries
// times before terminating with status=failed. The parent's StepRecord and
// Next-evaluation only fire once every attempt has terminated.
func (e *Engine) advanceBestOfNAttempt(taskID string, def *Definition, parent *Step, attemptID string, wfExec *Execution, output StepOutput) (comp *CompletionInfo, err error) {
	rec := wfExec.BestOfNInflight[parent.ID]
	if rec == nil {
		e.logger.Debug("workflow.best-of-n.stale", "task_id", taskID, "parent", parent.ID, "attempt", attemptID)
		return
	}
	status := rec.Attempts[attemptID]
	if status == nil {
		status = &AttemptStatus{AttemptID: attemptID}
		rec.Attempts[attemptID] = status
	}

	if output.Status == "failed" && parent.Config.MaxRetries > 0 && status.Retries < parent.Config.MaxRetries {
		status.Retries++
		status.Status = "pending"
		status.Output = output.Output
		if sErr := e.tasks.SetWorkflow(taskID, wfExec); sErr != nil {
			return nil, sErr
		}
		t, gErr := e.tasks.GetTask(taskID)
		if gErr != nil {
			return nil, gErr
		}
		t = e.withManualTestConfig(t)
		ctx := TemplateContext{Task: t, Step: *parent, Vars: wfExec.Variables, Workflow: wfExec}
		if spawnErr := e.spawnBestOfNAttempt(taskID, parent, wfExec, ctx, attemptID, status); spawnErr != nil {
			status.Status = "failed"
			status.Output = "respawn failed: " + spawnErr.Error()
			e.logger.Error("workflow.best-of-n.respawn", "task_id", taskID, "parent", parent.ID, "attempt", attemptID, "err", spawnErr)
		}
		return nil, e.tasks.SetWorkflow(taskID, wfExec)
	}

	status.AgentID = output.AgentID
	status.Provider = output.Provider
	status.Status = output.Status
	status.Output = truncate(output.Output, 4000)

	if !rec.AllAttemptsDone() {
		e.logger.Debug("workflow.best-of-n.attempt-done",
			"task_id", taskID, "parent", parent.ID, "attempt", attemptID, "status", status.Status)
		return nil, e.tasks.SetWorkflow(taskID, wfExec)
	}

	return e.finalizeBestOfNParent(taskID, def, parent, wfExec)
}

// finalizeBestOfNParent collapses a best_of_n block whose attempts have all
// reached terminal status. Zero or exactly one successful attempt each fail
// closed to human-required with a DISTINCT reason (never a shared generic
// message) rather than proceeding to a judge that has nothing (or nothing
// meaningful) to compare. Two or more successes persist a manifest artifact
// and advance to the workflow's next step (the judge run_agent step).
func (e *Engine) finalizeBestOfNParent(taskID string, def *Definition, parent *Step, wfExec *Execution) (comp *CompletionInfo, err error) {
	rec := wfExec.BestOfNInflight[parent.ID]
	if rec == nil {
		// Stale/duplicate finalize — already collapsed by a prior call.
		return
	}

	successes := rec.SuccessfulAttemptIDs()
	failedIDs := rec.FailedAttemptIDs()

	if len(successes) == 0 {
		delete(wfExec.BestOfNInflight, parent.ID)
		e.cleanupAllBestOfNAttempts(taskID, rec)
		return e.failStepClosed(taskID, def, parent, wfExec,
			"best-of-n: all attempts failed to start or complete")
	}
	if len(successes) == 1 {
		delete(wfExec.BestOfNInflight, parent.ID)
		e.cleanupAllBestOfNAttempts(taskID, rec)
		return e.failStepClosed(taskID, def, parent, wfExec,
			"best-of-n: fewer than 2 successful attempts, cannot judge")
	}

	manifest := buildBestOfNManifest(parent.ID, rec)
	if e.recorder != nil {
		if rErr := e.recorder.PutGeneric(taskID, "best-of-n-manifest-"+parent.ID+".json", parent.ID, manifest); rErr != nil {
			e.logger.Warn("artifact.record.failed", "kind", "best_of_n_manifest", "task_id", taskID, "step", parent.ID, "err", rErr)
		}
	}
	wfExec.SetVar(bestOfNManifestVar(parent.ID), manifest)
	wfExec.SetVar(bestOfNSuccessfulVar(parent.ID), strings.Join(successes, ","))

	parentOutput := fmt.Sprintf("successful=%s failed=%s", strings.Join(successes, ","), strings.Join(failedIDs, ","))
	now := time.Now().UTC()
	wfExec.RecordStep(StepRecord{
		StepID:    parent.ID,
		Status:    "completed",
		Output:    truncate(parentOutput, 4000),
		StartedAt: rec.StartedAt,
		EndedAt:   now,
	})
	wfExec.SetVar("step."+parent.ID+".output", truncate(parentOutput, 2000))

	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	t.Workflow = wfExec
	nextStep, comp, err := e.resolveNext(taskID, def, parent, wfExec, t)
	if err != nil {
		return nil, err
	}
	if nextStep == nil {
		return comp, nil
	}
	e.logger.Info("workflow.best-of-n.advance", "task_id", taskID, "from", parent.ID, "to", nextStep.ID)
	return e.executeSteps(taskID, def, nextStep, wfExec)
}

// cleanupAllBestOfNAttempts best-effort removes every attempt worktree dir
// once a best_of_n block terminates WITHOUT a promotion (all failed, or too
// few successes to judge) — mirrors the loser cleanup execPromoteBestOfN does
// on the success path, so a block that never reaches promotion doesn't leak
// attempt directories on disk indefinitely.
func (e *Engine) cleanupAllBestOfNAttempts(taskID string, rec *BestOfNInflight) {
	if e.attemptWorktrees == nil || rec == nil {
		return
	}
	ids := make([]string, 0, len(rec.Attempts))
	for id := range rec.Attempts {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	e.attemptWorktrees.CleanupAttempts(taskID, ids)
}

// preflightRunAgentBudget enforces the cumulative task cost budget BEFORE a
// direct-dispatch run_agent step (e.g. the best-of-N judge) actually spawns.
// The judge passes a pre-staged `dir`, so it routes through
// agentAdapter.StartAgent, which — unlike StartAgentWithAssignment — never
// checks the budget itself; without this preflight Sybra would spend on the
// (most expensive) judge run and only fail closed later at promotion.
//
// Returns handled=false (dispatch proceeds normally) unless the step opts in
// via BudgetPreflight AND the budget is already exceeded, in which case it
// flips the task to human-required and ends the workflow via the same
// declarative Next path as every other mechanical gate.
func (e *Engine) preflightRunAgentBudget(taskID string, def *Definition, step *Step, wfExec *Execution) (comp *CompletionInfo, handled bool, err error) {
	if !step.Config.BudgetPreflight || e.costBudget == nil {
		return nil, false, nil
	}
	cErr := e.costBudget.CheckTaskCostBudget(taskID)
	if cErr == nil {
		return nil, false, nil
	}
	if !errors.Is(cErr, ErrTaskCostExceeded) {
		return nil, true, cErr
	}
	comp, err = e.failStepClosed(taskID, def, step, wfExec,
		"best-of-n: cost budget exceeded before judge dispatch: "+cErr.Error())
	return comp, true, err
}

// failStepClosed flips the task to human-required with reason and drives
// the step's own Next transitions (typically
// `when task.status == human-required goto ""`) so the workflow ends via the
// same declarative path as every other mechanical gate (verify_commits,
// detect_tampering, ...), rather than force-ending the execution directly.
func (e *Engine) failStepClosed(taskID string, def *Definition, step *Step, wfExec *Execution, reason string) (*CompletionInfo, error) {
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	wfExec.RecordStep(StepRecord{StepID: step.ID, Status: "failed", Output: reason, StartedAt: now, EndedAt: now})
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	t.Workflow = wfExec
	nextStep, comp, err := e.resolveNext(taskID, def, step, wfExec, t)
	if err != nil {
		return nil, err
	}
	if nextStep == nil {
		return comp, nil
	}
	return e.executeSteps(taskID, def, nextStep, wfExec)
}

// bestOfNManifestAttempt is one attempt's entry in the JSON manifest artifact
// judge prompts read and engine tests assert against. Local-only: never
// surfaced to a public destination without scrub (see CLAUDE.md's Work-Data
// Confidentiality rule) — callers must route through the same
// work-project-scrub context as every other artifact.
type bestOfNManifestAttempt struct {
	AttemptID string `json:"attempt_id"`
	Status    string `json:"status"`
	Provider  string `json:"provider,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Output    string `json:"output,omitempty"`
}

type bestOfNManifest struct {
	ParentStep string                   `json:"parent_step"`
	Attempts   []bestOfNManifestAttempt `json:"attempts"`
}

func buildBestOfNManifest(stepID string, rec *BestOfNInflight) string {
	ids := make([]string, 0, len(rec.Attempts))
	for id := range rec.Attempts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	m := bestOfNManifest{ParentStep: stepID}
	for _, id := range ids {
		a := rec.Attempts[id]
		m.Attempts = append(m.Attempts, bestOfNManifestAttempt{
			AttemptID: a.AttemptID,
			Status:    a.Status,
			Provider:  a.Provider,
			Branch:    a.Branch,
			Dir:       a.Dir,
			Output:    truncate(a.Output, 500),
		})
	}
	b, mErr := json.Marshal(m)
	if mErr != nil {
		return `{"parent_step":"` + stepID + `","error":"manifest marshal failed"}`
	}
	return string(b)
}

// judgeVerdict is the mechanically-validated JSON contract the judge
// run_agent step must emit: `{"winner_attempt_id": "...", "scores": [...],
// "rationale": "..."}`. Extra/unknown fields are ignored.
type judgeVerdict struct {
	WinnerAttemptID string       `json:"winner_attempt_id"`
	Scores          []judgeScore `json:"scores"`
	Rationale       string       `json:"rationale"`
}

type judgeScore struct {
	AttemptID string  `json:"attempt_id"`
	Score     float64 `json:"score"`
}

// extractJudgeJSON parses a judgeVerdict out of a judge agent's raw output,
// tolerating surrounding prose by falling back to the outermost {...} span
// when the whole trimmed output isn't valid JSON on its own.
func extractJudgeJSON(output string) (judgeVerdict, error) {
	trimmed := strings.TrimSpace(output)
	var v judgeVerdict
	if err := json.Unmarshal([]byte(trimmed), &v); err == nil {
		return v, nil
	}
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return judgeVerdict{}, fmt.Errorf("no JSON object found in judge output")
	}
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &v); err != nil {
		return judgeVerdict{}, fmt.Errorf("malformed judge JSON: %w", err)
	}
	return v, nil
}

// humanRequiredStepOutput flips the task to human-required with reason and
// returns a StepOutput{Status: "failed"} so the caller's normal sync-step
// Next-evaluation (`when task.status == human-required goto ""`) ends the
// workflow — the same mechanical pattern as verify_commits/detect_tampering.
func (e *Engine) humanRequiredStepOutput(taskID string, step *Step, reason string) (StepOutput, error) {
	if err := e.tasks.UpdateTaskStatus(taskID, "human-required", reason); err != nil {
		return StepOutput{}, err
	}
	return StepOutput{StepID: step.ID, Status: "failed", Output: reason}, nil
}

// execPromoteBestOfN is the mechanical (no-LLM) step that reads the
// mandatory JudgeStep's completed output, mechanically validates the winner
// JSON, and fast-forwards the canonical task branch/worktree onto the
// winning attempt via AttemptWorktreeManager.PromoteAttempt. Malformed JSON,
// an unknown/ambiguous winner id, a judge step that errored or never
// completed, and an unsafe promotion (diverged branch / existing PR / dirty
// worktree — see worktree.Manager.PromoteAttempt) each fail closed to
// human-required with a DISTINCT reason string.
func (e *Engine) execPromoteBestOfN(taskID string, step *Step) (StepOutput, error) {
	// Read the freshest execution off the task rather than a stale ctx copy —
	// this sync step runs immediately after the judge's AdvanceStep already
	// persisted wfExec.
	t, err := e.tasks.GetTask(taskID)
	if err != nil {
		return StepOutput{}, err
	}
	wfExec := t.Workflow
	if wfExec == nil {
		return StepOutput{}, fmt.Errorf("task %s has no active workflow", taskID)
	}

	judgeRec := wfExec.RecordForStep(step.Config.JudgeStep)
	if judgeRec == nil || judgeRec.Status != "completed" {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: judge step errored or timed out")
	}

	rec := wfExec.BestOfNInflight[step.Config.BestOfNStep]
	if rec == nil {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: no attempt records found for best_of_n_step")
	}

	verdict, extractErr := extractJudgeJSON(judgeRec.Output)
	if extractErr != nil {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: malformed judge output: "+extractErr.Error())
	}
	winnerID := strings.TrimSpace(verdict.WinnerAttemptID)
	if winnerID == "" {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: judge output missing winner_attempt_id")
	}
	if strings.ContainsAny(winnerID, ", \t\n") {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: judge output names multiple or ambiguous winners")
	}
	winner := rec.Attempts[winnerID]
	if winner == nil {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: judge named unknown attempt id "+winnerID)
	}
	if winner.Status != "completed" {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: judge named a non-successful attempt "+winnerID)
	}

	if e.costBudget != nil {
		if cErr := e.costBudget.CheckTaskCostBudget(taskID); cErr != nil {
			if errors.Is(cErr, ErrTaskCostExceeded) {
				return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: cost budget exceeded before promotion: "+cErr.Error())
			}
			return StepOutput{}, cErr
		}
	}

	if e.attemptWorktrees == nil {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion: no attempt worktree manager configured")
	}

	if e.recorder != nil {
		if rErr := e.recorder.PutGeneric(taskID, "best-of-n-judge-report-"+step.Config.BestOfNStep+".json", step.ID, judgeRec.Output); rErr != nil {
			e.logger.Warn("artifact.record.failed", "kind", "best_of_n_judge_report", "task_id", taskID, "step", step.ID, "err", rErr)
		}
	}

	canonicalDir, promErr := e.attemptWorktrees.PromoteAttempt(taskID, winner.Dir, winner.Branch)
	if promErr != nil {
		return e.humanRequiredStepOutput(taskID, step, "best-of-n promotion refused: "+promErr.Error())
	}
	// Downstream steps (e.g. a shell step pushing the now-canonical branch)
	// resolve the worktree via the reserved WorkflowVarDir var, same as every
	// other run_agent-produced dir — see execRunAgent's wfExec.SetVar(WorkflowVarDir, ...).
	if canonicalDir != "" {
		wfExec.SetVar(WorkflowVarDir, canonicalDir)
	}

	// Clean up every attempt dir, including the winner's: PromoteAttempt
	// fast-forwards the canonical branch and materializes a SEPARATE
	// canonical worktree (PathFor, not PathForAttempt) — the winner's own
	// attempt dir is now a redundant duplicate checkout, not the worktree
	// downstream steps operate in.
	allAttempts := make([]string, 0, len(rec.Attempts))
	for id := range rec.Attempts {
		allAttempts = append(allAttempts, id)
	}
	sort.Strings(allAttempts)
	if len(allAttempts) > 0 {
		e.attemptWorktrees.CleanupAttempts(taskID, allAttempts)
	}

	// Persist the loser cleanup + inflight teardown against the CURRENT
	// wfExec (fetched fresh at the top of this step) so we don't clobber any
	// variables the judge step itself set.
	delete(wfExec.BestOfNInflight, step.Config.BestOfNStep)
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return StepOutput{}, err
	}

	e.logger.Info("workflow.best-of-n.promoted", "task_id", taskID, "step", step.ID, "winner", winnerID)
	return StepOutput{StepID: step.ID, Status: "completed", Output: "promoted " + winnerID}, nil
}
