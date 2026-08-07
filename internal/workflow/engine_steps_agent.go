package workflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/skillinvoke"
	"github.com/Automaat/sybra/internal/taskstatus"
)

// importSidecarIfConfigured reads the file the agent produced (template
// rendered from step.config.import_sidecar.from) and stores its content
// as the configured task sidecar. Called from HandleAgentComplete on
// success so the host — which can write anywhere — closes the gap when
// the agent's sandbox blocks ~/.sybra/tasks/. Errors are logged, not
// returned: the require_sidecar guard surfaces an empty sidecar by
// flipping the task to human-required, which is the correct UX.
func (e *Engine) importSidecarIfConfigured(taskID, stepID string, info TaskInfo) {
	if info.Workflow == nil {
		return
	}
	def, err := e.store.Get(info.Workflow.WorkflowID)
	if err != nil {
		return
	}
	e.importSidecarIfConfiguredFromDef(taskID, stepID, info, &def)
}

func (e *Engine) importSidecarIfConfiguredFromDef(taskID, stepID string, info TaskInfo, def *Definition) {
	if info.Workflow == nil || def == nil {
		return
	}
	step := def.StepByID(stepID)
	if step == nil || step.Type != StepRunAgent {
		return
	}
	for _, cfg := range step.Config.sidecarImports() {
		e.importOneSidecar(taskID, stepID, step, info, cfg)
	}
}

// adoptSidecarsFromFailedRun checks whether a run_agent step that just ended
// in failure (e.g. aborted_streaming, denied-tool noise) nonetheless left a
// complete, valid set of sidecar artifacts on disk — the case where the
// agent finished writing everything it was asked for and only died
// afterwards. A step's declared import_sidecars are treated as a single set:
// downstream guard steps (require_plan, require_sidecar, ...) already fail
// the task to human-required if any individual sidecar turns out missing, so
// adoption only pays off when every declared sidecar — not just the ones
// marked required at the import step — is present and non-empty, and a
// plan_contract among them validates. On success it adopts them exactly as a
// successful run would and reports true so the caller can treat the step as
// completed instead of burning a retry attempt re-doing already-finished
// work. Reports false (no adoption, no side effects) on any missing/empty
// artifact or invalid contract, leaving the ordinary failed/retry path
// untouched.
func (e *Engine) adoptSidecarsFromFailedRun(taskID, stepID string, info TaskInfo, def *Definition) bool {
	if info.Workflow == nil || def == nil {
		return false
	}
	step := def.StepByID(stepID)
	if step == nil || step.Type != StepRunAgent {
		return false
	}
	imports := step.Config.sidecarImports()
	if len(imports) == 0 {
		return false
	}
	var contract string
	hasContract := false
	for _, cfg := range imports {
		content, ok := e.probeSidecarContent(taskID, stepID, step, info, cfg)
		if !ok {
			return false
		}
		if cfg.Kind == "plan_contract" {
			contract, hasContract = content, true
		}
	}
	if hasContract {
		if problems := ValidatePlanContractForTask(contract, taskID, info.Body); len(problems) > 0 {
			e.logger.Info("workflow.adopt-sidecars.invalid-contract",
				"task_id", taskID, "step", stepID, "problems", strings.Join(problems, "; "))
			return false
		}
	}
	e.importSidecarIfConfiguredFromDef(taskID, stepID, info, def)
	e.logger.Info("workflow.adopt-sidecars.recovered", "task_id", taskID, "step", stepID)
	return true
}

// probeSidecarContent renders and reads a single sidecar import's source
// file without writing anything or mutating task status — used to check
// completeness before committing to adoptSidecarsFromFailedRun's write path.
func (e *Engine) probeSidecarContent(taskID, stepID string, step *Step, info TaskInfo, cfg ImportSidecar) (string, bool) {
	path, rErr := RenderTemplate(cfg.From, TemplateContext{
		Task:     info,
		Step:     *step,
		Vars:     info.Workflow.Variables,
		Workflow: info.Workflow,
	})
	if rErr != nil {
		return "", false
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		dirVarUnresolved := worktreeDirTemplatePattern.MatchString(cfg.From) && strings.TrimSpace(info.Workflow.Variables[WorkflowVarDir]) == ""
		if dirVarUnresolved {
			if _, recovered, ok := e.recoverSidecarFromTaskWorktree(taskID, stepID, step, info, cfg); ok {
				content, readErr = recovered, nil
			}
		}
		if readErr != nil {
			return "", false
		}
	}
	if strings.TrimSpace(string(content)) == "" {
		return "", false
	}
	return string(content), true
}

func (c StepConfig) sidecarImports() []ImportSidecar {
	var out []ImportSidecar
	if c.ImportSidecar != nil {
		out = append(out, *c.ImportSidecar)
	}
	out = append(out, c.ImportSidecars...)
	return out
}

var worktreeDirTemplatePattern = regexp.MustCompile(`\{\{\s*getvar\s+\.Vars\s+"` + WorkflowVarDir + `"\s*\}\}`)

func (e *Engine) importOneSidecar(taskID, stepID string, step *Step, info TaskInfo, cfg ImportSidecar) {
	path, rErr := RenderTemplate(cfg.From, TemplateContext{
		Task:     info,
		Step:     *step,
		Vars:     info.Workflow.Variables,
		Workflow: info.Workflow,
	})
	if rErr != nil {
		e.logger.Warn("workflow.import-sidecar.render", "task_id", taskID, "step", stepID, "err", rErr)
		return
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		dirVarUnresolved := worktreeDirTemplatePattern.MatchString(cfg.From) && strings.TrimSpace(info.Workflow.Variables[WorkflowVarDir]) == ""
		if dirVarUnresolved {
			// The engine lost track of the agent's working directory (e.g. a
			// restart/reattach reloaded workflow state without _dir), not
			// necessarily that the agent never produced the artifact — the
			// file may well exist in the worktree the agent actually ran in.
			// Before escalating, try to recover the real worktree dir from
			// task metadata (the same lookup verify_checks/tamper/etc. use)
			// and retry against it. See sybra#1988.
			if recoveredPath, recoveredContent, ok := e.recoverSidecarFromTaskWorktree(taskID, stepID, step, info, cfg); ok {
				path, content, readErr = recoveredPath, recoveredContent, nil
			}
		}
		if readErr != nil {
			e.logger.Warn("workflow.import-sidecar.read", "task_id", taskID, "step", stepID, "path", path, "err", readErr)
			if cfg.Required {
				// Surface the empty-dir-var case distinctly so it isn't
				// misread as "review missing" when investigating.
				if dirVarUnresolved {
					e.failRequiredImport(taskID, stepID, cfg.Kind, "unresolved: worktree dir variable was empty at render time")
				} else {
					e.failRequiredImport(taskID, stepID, cfg.Kind, "missing")
				}
			}
			return
		}
	}
	if cfg.Required && strings.TrimSpace(string(content)) == "" {
		e.failRequiredImport(taskID, stepID, cfg.Kind, "empty")
		return
	}
	// Convention: a bare "plan_draft" kind is auto-namespaced by the step
	// ID so a single workflow can fan out to N parallel planners without
	// each having to spell out a unique kind. The result lands in the
	// PlanDraftStore under name=<step ID>.
	kind := cfg.Kind
	if kind == "plan_draft" {
		kind = "plan_draft." + stepID
	}
	if writeErr := e.tasks.WriteSidecar(taskID, kind, string(content)); writeErr != nil {
		e.logger.Error("workflow.import-sidecar.write", "task_id", taskID, "step", stepID, "kind", kind, "err", writeErr)
		return
	}
	e.logger.Info("workflow.import-sidecar", "task_id", taskID, "step", stepID, "kind", kind, "path", path, "bytes", len(content))

	// Capture a plan artifact for plan-kind sidecars so the raw markdown is
	// available for later agent re-reading alongside its provenance metadata.
	if cfg.Kind == "plan" && e.recorder != nil {
		if recErr := e.recorder.PutPlanSnapshot(taskID, step.Config.Role, stepID, path, string(content)); recErr != nil {
			e.logger.Warn("artifact.record.failed", "kind", "plan", "task_id", taskID, "step", stepID, "err", recErr)
		}
	}
	if cfg.Kind == "plan_contract" && e.recorder != nil {
		name := "plan-contract.json"
		if stepID != "" {
			name = "plan-contract-" + stepID + ".json"
		}
		if recErr := e.recorder.PutGeneric(taskID, name, stepID, string(content)); recErr != nil {
			e.logger.Warn("artifact.record.failed", "kind", "plan_contract", "task_id", taskID, "step", stepID, "err", recErr)
		}
	}
}

// recoverSidecarFromTaskWorktree resolves the task's worktree dir from task
// metadata (the same WorktreeGetter lookup verify_checks/tamper/codegen use,
// independent of the workflow's own _dir variable) and retries rendering +
// reading cfg.From against it. On success it also persists the recovered dir
// back into the workflow's _dir variable so later sidecar imports and steps
// in the same run don't repeat the same lost-variable failure. Returns
// ok=false if no WorktreeGetter is wired, no worktree is found for the task,
// or the file still doesn't exist at the recovered path — callers fall
// through to the ordinary escalation path in that case.
func (e *Engine) recoverSidecarFromTaskWorktree(taskID, stepID string, step *Step, info TaskInfo, cfg ImportSidecar) (path string, content []byte, ok bool) {
	if e.worktrees == nil {
		return "", nil, false
	}
	wtPath, found := e.worktrees.GetWorktreePath(taskID)
	if !found || strings.TrimSpace(wtPath) == "" {
		return "", nil, false
	}
	vars := maps.Clone(info.Workflow.Variables)
	if vars == nil {
		vars = map[string]string{}
	}
	vars[WorkflowVarDir] = wtPath
	if sidecar := e.resolveSidecarDir(taskID); sidecar != "" {
		vars[WorkflowVarSidecarDir] = sidecar
	}
	recoveredPath, rErr := RenderTemplate(cfg.From, TemplateContext{
		Task:     info,
		Step:     *step,
		Vars:     vars,
		Workflow: info.Workflow,
	})
	if rErr != nil {
		return "", nil, false
	}
	data, readErr := os.ReadFile(recoveredPath)
	if readErr != nil {
		return "", nil, false
	}
	info.Workflow.SetVar(WorkflowVarDir, wtPath)
	if sidecar := e.resolveSidecarDir(taskID); sidecar != "" {
		info.Workflow.SetVar(WorkflowVarSidecarDir, sidecar)
	}
	if setErr := e.tasks.SetWorkflow(taskID, info.Workflow); setErr != nil {
		e.logger.Warn("workflow.import-sidecar.recover.persist", "task_id", taskID, "step", stepID, "err", setErr)
	}
	e.logger.Info("workflow.import-sidecar.recovered-dir", "task_id", taskID, "step", stepID, "dir", wtPath)
	return recoveredPath, data, true
}

func (e *Engine) failRequiredImport(taskID, stepID, kind, state string) {
	reason := fmt.Sprintf("required %s sidecar %s", strings.ReplaceAll(kind, "_", " "), state)
	if stepID != "" {
		reason += " after step " + stepID
	}
	if statusErr := e.tasks.UpdateTaskStatus(taskID, taskstatus.HumanRequired, reason); statusErr != nil {
		e.logger.Error("workflow.import-sidecar.required.status", "task_id", taskID, "step", stepID, "kind", kind, "err", statusErr)
	}
}

func resolveRunAgentDir(step *Step, wfExec *Execution, ctx TemplateContext) (string, error) {
	dir := wfExec.Variables[WorkflowVarDir]
	if step.Config.Dir == "" {
		return dir, nil
	}
	renderedDir, err := RenderTemplate(step.Config.Dir, ctx)
	if err != nil {
		return "", fmt.Errorf("render dir: %w", err)
	}
	if strings.TrimSpace(renderedDir) == "" {
		return "", errors.New("render dir: resolved to empty path")
	}
	return renderedDir, nil
}

func (e *Engine) execRunAgent(taskID string, step *Step, wfExec *Execution, ctx TemplateContext, effectIDs ...EffectID) (runErr error) {
	prepareTestVerdictAttemptVars(wfExec, step.ID, ctx.Task.Body)
	// Seed the sidecar dir before anything renders a template. Setting it only
	// after dispatch would leave the first run of a verifier role resolving
	// {{sidecardir .Vars}} to the worktree — which that role cannot write —
	// and the sandbox denial surfaces as an empty sidecar rather than an
	// error. ctx carries its own copy of the vars, so both must be updated.
	if sidecar := e.resolveSidecarDir(taskID); sidecar != "" {
		wfExec.SetVar(WorkflowVarSidecarDir, sidecar)
		if ctx.Vars == nil {
			ctx.Vars = map[string]string{}
		}
		ctx.Vars[WorkflowVarSidecarDir] = sidecar
	}
	unlockRoute := e.routeLocks.LockLocal(taskID)
	defer unlockRoute()

	claimedEffectID := EffectID{}
	if len(effectIDs) > 0 {
		claimedEffectID = effectIDs[0]
	}
	agentStarted := false
	defer func() {
		releaseClaim := func() {
			if claimedEffectID.IsZero() || agentStarted {
				return
			}
			if _, relErr := e.releaseClaimedEffect(taskID, claimedEffectID); relErr != nil {
				if effectClaimFence(relErr) {
					return
				}
				if runErr == nil {
					runErr = fmt.Errorf("release claimed effect: %w", relErr)
					return
				}
				runErr = errors.Join(runErr, fmt.Errorf("release claimed effect: %w", relErr))
			}
		}
		if recovered := recover(); recovered != nil {
			releaseClaim()
			panic(recovered)
		}
		if runErr != nil {
			releaseClaim()
		}
	}()

	mode := resolveRunAgentMode(step.Config.Mode, ctx)
	if admit, reason := e.agents.AdmitDispatch(taskID, step.Config.Role, mode); !admit {
		err := fmt.Errorf("%w: %s", ErrResourcePressure, reason)
		failure := ClassifyAgentStartFailure(err)
		wfExec.State = ExecWaiting
		e.logger.Info("workflow.run-agent.resource-pressure", "task_id", taskID, "step", step.ID, "reason", reason)
		return e.tasks.SetStatusAndWorkflow(taskID, string(ctx.Task.Status), failure.Reason, wfExec)
	}

	model := resolveRunAgentModel(step.Config.Model, ctx)

	provider, model, assignment, err := e.resolveAgentVariant(ctx.Task, step, wfExec, model, "workflow.cross-provider.fallback")
	if err != nil {
		return err
	}
	applySkillReceiptRecoveryAssignment(step.ID, wfExec, &assignment)

	prompt, err := e.renderAssignedPrompt(taskID, step, ctx, assignment, "workflow.consume-steer")
	if err != nil {
		return err
	}

	// Reuse a live agent if configured and one exists for this role.
	if step.Config.ReuseAgent {
		if agentID, found := e.agents.FindRunningAgentForRole(taskID, step.Config.Role); found {
			if sendErr := e.agents.SendPrompt(agentID, prompt); sendErr != nil {
				e.logger.Warn("workflow.reuse-agent.send-failed", "task_id", taskID, "agent_id", agentID, "err", sendErr)
				e.agents.StopAgentsForTask(taskID, step.Config.Role)
			} else {
				wfExec.State = ExecWaiting
				wfExec.SetVar(watchdogReaskDeliveredKey(step.ID), "1")
				e.logger.Info("workflow.reuse-agent", "task_id", taskID, "step", step.ID, "agent_id", agentID)
				return e.tasks.SetWorkflow(taskID, wfExec)
			}
		}
	}

	dir, err := resolveRunAgentDir(step, wfExec, ctx)
	if err != nil {
		return err
	}
	cleanRetryKey := watchdogHangCleanRetryKey(step.ID)
	cleanRetryRef := wfExec.Variables[cleanRetryKey]
	captureTamperDeletionAllowlist(wfExec, step.ID, step.Config.Role, ctx.Task)

	// Stop stale agents left over from earlier workflow steps (e.g. an
	// interactive plan agent with reuse_agent that outlived plan approval).
	// Empty role = stop all roles for this task.
	e.agents.StopAgentsForTask(taskID, "")
	// Drop the stopped agents' step mappings so a late/double completion from a
	// superseded agent (e.g. a stopped test-runner during a run_test retry) is
	// treated as untracked and dropped rather than counted against the step's
	// retry budget. The agent spawned just below becomes the only tracked one.
	e.clearAgentStepsForTask(taskID)
	wfExec.ClearAgentRoutes()

	// mode is coerced to headless in resolveRunAgentMode, so no run_agent step
	// dispatches an interactive one-shot anymore — a steerable headless run
	// finalizes on its first completed turn on its own (drainOrCloseHeadlessSteer).
	oneShot := false

	// The step-action effect intent is already persisted before execRunAgent is
	// entered, so an untracked completion that lands while StartAgent is
	// blocking still sees a durable "dispatch in progress" claim via
	// routeStepPending. Do not hold e.mu across StartAgent: review recovery can
	// legitimately try to queue another workflow while the launcher is blocked.
	agentID, startedDir, baselineRef, err := e.agents.StartAgent(taskID, step.Config.Role, mode, model, provider, prompt, dir, step.Config.AllowedTools, step.Config.NeedsWorktree, oneShot, step.Config.OutputSchema, cleanRetryRef, assignment)
	if err != nil {
		if parked, parkErr := e.parkRunAgentStartError(taskID, step.ID, wfExec, err); parked {
			return parkErr
		}
		return fmt.Errorf("start agent: %w", err)
	}
	agentStarted = true
	return e.persistStartedAgent(taskID, step, wfExec, agentID, provider, startedDir, baselineRef, cleanRetryKey, cleanRetryRef, dir)
}

func resolveRunAgentMode(mode string, ctx TemplateContext) string {
	if strings.Contains(mode, "{{") {
		rendered, err := RenderTemplate(mode, ctx)
		if err == nil {
			mode = rendered
		}
	}
	// A legacy task file can still carry agent_mode: interactive (kept as a
	// load-only value), which the implement step templates straight through
	// {{.Task.AgentMode}}. Interactive dispatch no longer exists — coerce it
	// to headless here so AdmitDispatch and StartAgent both see the real mode,
	// matching spawnBestOfNAttempt/spawnParallelChild.
	if mode == "" || mode == "interactive" {
		return "headless"
	}
	return mode
}

func resolveRunAgentModel(model string, ctx TemplateContext) string {
	if strings.Contains(model, "{{") {
		rendered, err := RenderTemplate(model, ctx)
		if err == nil {
			model = rendered
		}
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "sonnet"
	}
	return model
}

func (e *Engine) persistStartedAgent(taskID string, step *Step, wfExec *Execution, agentID, provider, startedDir, baselineRef, cleanRetryKey, cleanRetryRef, dir string) error {
	if startedDir != "" && (step.Config.NeedsWorktree || dir != "") {
		wfExec.SetVar(WorkflowVarDir, startedDir)
		if sidecar := e.resolveSidecarDir(taskID); sidecar != "" {
			wfExec.SetVar(WorkflowVarSidecarDir, sidecar)
		}
	}
	if baselineRef != "" {
		wfExec.SetVar(tamperBaselineVar(step.ID), baselineRef)
	}
	if cleanRetryRef != "" {
		delete(wfExec.Variables, cleanRetryKey)
	}
	wfExec.SetAgentRoute(agentID, step.ID)
	wfExec.State = ExecWaiting
	e.logger.Info("workflow.run-agent", "task_id", taskID, "step", step.ID, "role", step.Config.Role, "agent_id", agentID, "provider", provider)
	if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return e.deferStartedAgentRoute(taskID, step.ID, agentID, err)
	}
	e.clearPendingAgentStep(taskID, agentID)
	return nil
}

func (e *Engine) parkRunAgentStartError(taskID, stepID string, wfExec *Execution, err error) (bool, error) {
	switch {
	case errors.Is(err, ErrDispatchInFlight):
		wfExec.State = ExecWaiting
		e.logger.Info("workflow.run-agent.dispatch-in-flight", "task_id", taskID, "step", stepID)
		return true, e.tasks.SetWorkflow(taskID, wfExec)
	case errors.Is(err, ErrTestRunnerBusy):
		wfExec.State = ExecWaiting
		e.logger.Info("workflow.run-agent.test-runner-busy", "task_id", taskID, "step", stepID)
		return true, e.tasks.SetWorkflow(taskID, wfExec)
	case errors.Is(err, ErrAgentPoolBusy):
		wfExec.State = ExecWaiting
		e.logger.Info("workflow.run-agent.agent-pool-busy", "task_id", taskID, "step", stepID)
		return true, e.tasks.SetWorkflow(taskID, wfExec)
	default:
		return false, nil
	}
}

func (e *Engine) selectABVariant(ctx abtest.SelectionContext) (AgentAssignment, bool, error) {
	// Snapshot the A/B config once so a concurrent SetABTestingConfig swap by
	// the routing ticker cannot race this selection or split it across two
	// generations mid-flight.
	cfg := e.abTestingConfig()
	eligibility := e.providerEligibilitySnapshot(cfg)
	providerAllowed := func(provider string) bool {
		status, ok := eligibility[provider]
		if !ok {
			status = e.providerEligibility(provider)
		}
		return status.allowed
	}
	var evalPassed abtest.EvalPassed
	if e.evalGate != nil {
		evalPassed = func(variantID, digest string) bool {
			allow, _, gateErr := e.evalGate.AllowEnrollment(variantID, digest)
			if gateErr != nil {
				// Gate read failure (store I/O, not "no verdict") — fail closed
				// rather than silently enrolling an unverified variant.
				return false
			}
			return allow
		}
	}
	a, ok, err := abtest.SelectEligibleForContextWithCohort(cfg, ctx, providerAllowed, evalPassed, e.cohortObserved)
	if err != nil || !ok {
		if err == nil && !ok {
			e.reportProviderShutout(cfg, ctx, eligibility, evalPassed)
		}
		return AgentAssignment{}, ok, err
	}
	e.reportProviderDemotion(cfg, ctx, a, eligibility, evalPassed)
	return AgentAssignment{
		ExperimentID:    a.ExperimentID,
		Kind:            a.Kind,
		VariantID:       a.VariantID,
		RoutingReason:   a.RoutingReason,
		Provider:        a.Provider,
		Model:           a.Model,
		AssignmentUnit:  a.AssignmentUnit,
		AssignmentKey:   a.AssignmentKey,
		ReasoningEffort: a.ReasoningEffort,
		PromptTransform: workflowPromptTransform(a.PromptTransform),
		SkillAliases:    cloneWorkflowSkillAliases(a.SkillAliases),
		DecisionVersion: a.DecisionVersion,
	}, true, nil
}

type providerEligibilityStatus struct {
	allowed bool
	reason  string
}

func (e *Engine) providerEligibility(provider string) providerEligibilityStatus {
	switch {
	case !providerAvailable(provider):
		return providerEligibilityStatus{reason: "cli_not_found"}
	case !e.agents.ProviderHealthy(provider):
		return providerEligibilityStatus{reason: "unhealthy"}
	case e.agents.ProviderRateLimited(provider):
		return providerEligibilityStatus{reason: "rate_limited"}
	default:
		return providerEligibilityStatus{allowed: true}
	}
}

func (e *Engine) providerEligibilitySnapshot(cfg abtest.Config) map[string]providerEligibilityStatus {
	statuses := map[string]providerEligibilityStatus{}
	for i := range cfg.Experiments {
		exp := cfg.Experiments[i]
		if !exp.EnabledValue() {
			continue
		}
		for j := range exp.Variants {
			provider := exp.Variants[j].Provider
			if _, ok := statuses[provider]; ok {
				continue
			}
			statuses[provider] = e.providerEligibility(provider)
		}
	}
	return statuses
}

// reportProviderDemotion detects when provider eligibility filtering (CLI
// missing, unhealthy, rate-limited) changed the A/B outcome for this routing
// context. It re-runs selection with no provider filter to find the provider
// that would have won on hash alone, compares it to the actual (filtered)
// assignment, then logs the captured selection-time reason. Throttling is
// fleet-wide per routing context + demotion tuple so a sustained outage does
// not emit one ERROR per task.
func (e *Engine) reportProviderDemotion(cfg abtest.Config, ctx abtest.SelectionContext, actual abtest.Assignment, eligibility map[string]providerEligibilityStatus, evalPassed abtest.EvalPassed) {
	// Cohort-aware (e.cohortObserved), same as the actual selection: a
	// canary-gated experiment's unfiltered "wanted" pick must apply the same
	// cohort gate the real dispatch used, or a legitimately-enrolled canary
	// candidate would be misreported as a demotion from its own baseline.
	wanted, ok, err := abtest.SelectEligibleForContextWithCohort(cfg, ctx, nil, evalPassed, e.cohortObserved)
	if err != nil || !ok || wanted.Provider == actual.Provider {
		return
	}
	status, ok := eligibility[wanted.Provider]
	if !ok {
		status = e.providerEligibility(wanted.Provider)
	}
	reason := status.reason
	if reason == "" {
		reason = "unknown"
	}
	key := strings.Join([]string{ctx.WorkflowID, ctx.Role, ctx.StepID, wanted.Provider, actual.Provider, reason}, "|")
	msg := fmt.Sprintf("wanted=%s got=%s reason=%s", wanted.Provider, actual.Provider, reason)
	e.demotionThrottle.Log(e.logger, "workflow.ab.provider_demoted", key, errors.New(msg),
		"task_id", ctx.TaskID, "workflow_id", ctx.WorkflowID, "role", ctx.Role, "step_id", ctx.StepID,
		"wanted_provider", wanted.Provider, "selected_provider", actual.Provider, "reason", reason)
}

// reportProviderShutout detects the silent A/B fallback: filtered selection
// yielded no assignment (ok=false, err=nil), yet dropping the provider filter
// would have matched an experiment. That means provider eligibility filtering
// (CLI missing, unhealthy, rate-limited) excluded *every* variant, so the whole
// experiment degraded to normal (non-A/B) dispatch rather than a different
// variant winning (that partial case is reportProviderDemotion's job). Without
// this signal the total shutout is indistinguishable from A/B being disabled or
// no experiment matching the role. Throttled per routing context + experiment +
// provider + reason so a sustained outage does not emit one ERROR per task.
func (e *Engine) reportProviderShutout(cfg abtest.Config, ctx abtest.SelectionContext, eligibility map[string]providerEligibilityStatus, evalPassed abtest.EvalPassed) {
	wanted, ok, err := abtest.SelectEligibleForContextWithCohort(cfg, ctx, nil, evalPassed, e.cohortObserved)
	if err != nil || !ok {
		return
	}
	status, found := eligibility[wanted.Provider]
	if !found {
		status = e.providerEligibility(wanted.Provider)
	}
	reason := status.reason
	if reason == "" {
		reason = "unknown"
	}
	key := strings.Join([]string{ctx.WorkflowID, ctx.Role, ctx.StepID, wanted.ExperimentID, wanted.Provider, reason}, "|")
	msg := fmt.Sprintf("experiment=%s wanted=%s reason=%s fell back to non-A/B dispatch", wanted.ExperimentID, wanted.Provider, reason)
	e.demotionThrottle.Log(e.logger, "workflow.ab.provider_shutout", key, errors.New(msg),
		"task_id", ctx.TaskID, "workflow_id", ctx.WorkflowID, "role", ctx.Role, "step_id", ctx.StepID,
		"experiment_id", wanted.ExperimentID, "wanted_provider", wanted.Provider, "reason", reason)
}

func (e *Engine) renderAssignedPrompt(taskID string, step *Step, ctx TemplateContext, assignment AgentAssignment, steerLog string) (string, error) {
	templateText := applyPromptTransform(step.Config.Prompt, assignment.PromptTransform)
	prompt, err := RenderTemplate(templateText, ctx)
	if err != nil {
		return "", fmt.Errorf("render prompt: %w", err)
	}
	if steered, sErr := e.tasks.ConsumeSupervisorSteer(taskID, prompt); sErr != nil {
		e.logger.Warn(steerLog, "task_id", taskID, "step", step.ID, "err", sErr)
	} else {
		prompt = steered
	}
	return skillinvoke.ApplyAliases(prompt, assignment.SkillAliases), nil
}

func applyPromptTransform(prompt string, transform *PromptTransform) string {
	if transform == nil {
		return prompt
	}
	return abtest.ApplyPromptTransformOpText(prompt, transform.Op, transform.Text)
}

func workflowPromptTransform(in *abtest.PromptTransform) *PromptTransform {
	if in == nil {
		return nil
	}
	return &PromptTransform{Op: in.Op, Text: in.Text}
}

func cloneWorkflowSkillAliases(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func (e *Engine) resolveAgentVariant(t TaskInfo, step *Step, wfExec *Execution, model, fallbackLog string) (provider, resolvedModel string, assignment AgentAssignment, err error) {
	defaultModel := model
	provider = resolveProvider(step.Config.Provider, wfExec, e.agents.DefaultProvider(), t)
	resolvedModel = model
	if step.Config.Provider == "" || step.Config.Provider == "ab" {
		selected, ok, err := e.selectABVariant(abtest.SelectionContext{
			TaskID:     t.ID,
			WorkflowID: wfExec.WorkflowID,
			Role:       step.Config.Role,
			StepID:     step.ID,
			Prompt:     step.Config.Prompt,
		})
		if err != nil {
			return "", "", AgentAssignment{}, fmt.Errorf("select ab variant: %w", err)
		}
		if ok {
			provider = selected.Provider
			resolvedModel = selected.Model
			assignment = selected
			if selected.ReasoningEffort != "" {
				wfExec.SetVar("ab."+step.ID+".reasoning_effort", selected.ReasoningEffort)
			}
		}
	}
	if routed, ok := e.routeAroundSilentHang(t, step, wfExec, provider); ok {
		provider = routed
		assignment.RoutingReason = "silent_hang_avoid"
	}
	if provider != "" && !providerAvailable(provider) {
		e.logger.Warn(fallbackLog, "wanted", provider, "reason", "CLI not found")
		return "", defaultModel, AgentAssignment{}, nil
	}
	if assignment.RoutingReason == "" && step.Config.Provider == "cross" {
		assignment.RoutingReason = "cross"
	}
	return provider, resolvedModel, assignment, nil
}

// routeAroundSilentHang moves this one dispatch off the provider whose last run
// on this step went silent, and consumes the hint so the step returns to normal
// routing afterwards. The provider stays healthy for every other task, which is
// the whole point of not reporting a silent child to the health gate — but the
// run that just watched it produce nothing should not be handed straight back
// to it.
func (e *Engine) routeAroundSilentHang(t TaskInfo, step *Step, wfExec *Execution, provider string) (string, bool) {
	if wfExec == nil || step == nil {
		return "", false
	}
	avoid := wfExec.Variables[watchdogSilentHangAvoidKey(step.ID)]
	if avoid == "" {
		return "", false
	}
	wfExec.SetVar(watchdogSilentHangAvoidKey(step.ID), "")
	effective := provider
	if effective == "" {
		effective = e.agents.DefaultProvider()
	}
	if effective != avoid {
		return "", false
	}
	alt := crossProvider(effective)
	if alt == "" || alt == avoid || !providerAvailable(alt) {
		return "", false
	}
	e.logger.Info("workflow.silent-hang.reroute",
		"task_id", t.ID, "step", step.ID, "from", avoid, "to", alt)
	return alt, true
}

// resolveProvider resolves the step-level provider string.
// "cross" flips the most relevant code-producing provider; "" defers to the
// manager default.
func resolveProvider(stepProv string, wfExec *Execution, _ string, t TaskInfo) string {
	switch stepProv {
	case "cross":
		if p := lastWorkflowProvider(wfExec); p != "" {
			return crossProvider(p)
		}
		if p := lastCodeAuthorProvider(t.AgentRuns); p != "" {
			return crossProvider(p)
		}
		if p := normalizeExplicitWorkflowProvider(t.HandoffSourceProvider); p != "" {
			return crossProvider(p)
		}
		return ""
	case "", "ab":
		return ""
	default:
		return stepProv
	}
}

func lastWorkflowProvider(wfExec *Execution) string {
	if wfExec == nil {
		return ""
	}
	for i := range slices.Backward(wfExec.StepHistory) {
		if p := normalizeExplicitWorkflowProvider(wfExec.StepHistory[i].Provider); p != "" {
			return p
		}
	}
	return ""
}

func lastCodeAuthorProvider(runs []AgentRunInfo) string {
	for i := range slices.Backward(runs) {
		if !isCodeAuthorRole(runs[i].Role) {
			continue
		}
		if p := normalizeExplicitWorkflowProvider(runs[i].Provider); p != "" {
			return p
		}
	}
	return ""
}

func isCodeAuthorRole(role string) bool {
	switch role {
	case "", "implementation", "fix-review", "pr-fix", "test-fix":
		return true
	default:
		return false
	}
}

func crossProvider(provider string) string {
	author := normalizeWorkflowProvider(provider)
	order := providerid.All()
	start := slices.Index(order, author)
	if start < 0 {
		start = slices.Index(order, providerid.Claude)
	}
	firstDifferent := ""
	for i := 1; i <= len(order); i++ {
		cand := order[(start+i)%len(order)]
		if cand == author {
			continue
		}
		if firstDifferent == "" {
			firstDifferent = cand
		}
		if providerAvailable(cand) {
			return cand
		}
	}
	return firstDifferent
}

func normalizeWorkflowProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", providerid.Claude:
		return providerid.Claude
	case providerid.Codex:
		return providerid.Codex
	case providerid.Copilot:
		return providerid.Copilot
	case providerid.OpenCode:
		return providerid.OpenCode
	default:
		return ""
	}
}

func normalizeExplicitWorkflowProvider(provider string) string {
	if strings.TrimSpace(provider) == "" {
		return ""
	}
	return normalizeWorkflowProvider(provider)
}

// providerAvailable reports whether the CLI for a provider is on PATH.
// Indirected through a var so tests can short-circuit the lookup — engine
// unit tests run with mock agents and don't care whether the real CLI is
// installed on the runner; without the indirection a CI host without
// claude/codex on PATH causes the engine's fallback to strip the
// step-configured provider, breaking provider-aware assertions.
var providerAvailable = func(provider string) bool {
	_, err := exec.LookPath(provider)
	return err == nil
}

func (e *Engine) execWaitHuman(taskID string, step *Step, wfExec *Execution) error {
	wfExec.State = ExecWaiting
	e.logger.Info("workflow.wait-human", "task_id", taskID, "step", step.ID, "actions", step.Config.HumanActions)
	if step.Config.Status != "" {
		if err := e.tasks.SetStatusAndWorkflow(taskID, step.Config.Status, step.Config.StatusReason, wfExec); err != nil {
			return err
		}
	} else if err := e.tasks.SetWorkflow(taskID, wfExec); err != nil {
		return err
	}
	e.maybeAutoApprovePlanReview(taskID, step)
	return nil
}

func (e *Engine) maybeAutoApprovePlanReview(taskID string, step *Step) {
	if !e.autoApprovePlansWithoutDecisions || step.ID != "review_plan" {
		return
	}
	if !slices.Contains(step.Config.HumanActions, "approve") {
		return
	}

	go func() {
		// Let the caller unwind first so workflow completion/cascade callbacks
		// run outside StartWorkflow/DispatchEvent marker scopes.
		time.Sleep(10 * time.Millisecond)
		t, err := e.tasks.GetTask(taskID)
		if err != nil {
			e.logger.Warn("workflow.plan-auto-approve.get", "task_id", taskID, "err", err)
			return
		}
		if !e.shouldAutoApprovePlanReview(t) {
			return
		}
		e.logger.Info("workflow.plan-auto-approve", "task_id", taskID, "reason", "no_open_decisions")
		err = e.HandleHumanAction(taskID, "approve", map[string]string{
			"auto_approved":       "true",
			"auto_approve_reason": "no_open_decisions",
		})
		if err != nil {
			e.logger.Warn("workflow.plan-auto-approve.approve", "task_id", taskID, "err", err)
			return
		}
		if e.planAutoApproveHook != nil {
			updated, getErr := e.tasks.GetTask(taskID)
			if getErr != nil {
				e.logger.Warn("workflow.plan-auto-approve.hook-task", "task_id", taskID, "err", getErr)
				return
			}
			e.planAutoApproveHook(updated, "no_open_decisions")
		}
	}()
}

func (e *Engine) shouldAutoApprovePlanReview(t TaskInfo) bool {
	if t.Status != taskstatus.PlanReview || t.Workflow == nil ||
		t.Workflow.WorkflowID != "simple-task-plan" ||
		t.Workflow.State != ExecWaiting ||
		t.Workflow.CurrentStep != "review_plan" {
		return false
	}
	if PlanHasOpenDecisions(t.PlanDecisions) {
		return false
	}
	// "No open decisions" only means nothing needs a human's judgment call —
	// it says nothing about whether the plan-critic found the contract itself
	// unsafe to execute. A REFINE/REJECT verdict names concrete blockers (e.g.
	// a compile-breaking gap in the file list) that execFlagPlanCritique's own
	// doc comment says review_plan is supposed to require an explicit human
	// look at, regardless of open decisions. Auto-approving through that flag
	// silently discarded every REFINE finding straight into implementation.
	if verdict := parsePlanCritiqueVerdict(t.PlanCritique); verdict == "REFINE" || verdict == "REJECT" {
		return false
	}
	if strings.TrimSpace(t.PlanContract) == "" {
		return false
	}
	return len(ValidatePlanContractForTask(t.PlanContract, t.ID, t.Body)) == 0
}

func (e *Engine) execSetStatus(taskID string, step *Step) (StepOutput, error) {
	if err := e.tasks.UpdateTaskStatus(taskID, taskstatus.Status(step.Config.Status), step.Config.StatusReason); err != nil {
		return StepOutput{}, err
	}

	e.logger.Info("workflow.set-status", "task_id", taskID, "step", step.ID, "status", step.Config.Status)
	return StepOutput{StepID: step.ID, Status: "completed"}, nil
}

func (e *Engine) execCondition(step *Step, wfExec *Execution, t TaskInfo) (StepOutput, error) {
	// Condition is a no-op execution; transition resolution in the caller handles branching.
	_ = t
	_ = wfExec
	return StepOutput{StepID: step.ID, Status: "completed"}, nil
}

func (e *Engine) execShell(step *Step, ctx TemplateContext) (StepOutput, error) {
	command, err := RenderTemplate(step.Config.Command, ctx)
	if err != nil {
		return StepOutput{}, fmt.Errorf("render command: %w", err)
	}

	shellCtx, cancel := context.WithTimeout(e.ctx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(shellCtx, "bash", "-c", command)
	if step.Config.Dir != "" {
		// Rendered the same as Command: lets a shell step target a
		// dynamically-resolved dir (e.g. best-of-n's canonical worktree,
		// only known after promote_best_of_n) via {{getvar .Vars "_dir"}}.
		dir, dErr := RenderTemplate(step.Config.Dir, ctx)
		if dErr != nil {
			return StepOutput{}, fmt.Errorf("render dir: %w", dErr)
		}
		if strings.TrimSpace(dir) == "" {
			return StepOutput{}, errors.New("render dir: resolved to empty path")
		}
		cmd.Dir = dir
	}

	// Expose task fields as env vars to avoid shell injection via template interpolation.
	ti := ctx.Task
	cmd.Env = append(cmd.Environ(),
		"SYBRA_TASK_ID="+ti.ID,
		"SYBRA_TASK_TITLE="+ti.Title,
		"SYBRA_TASK_STATUS="+string(ti.Status),
		"SYBRA_TASK_PROJECT="+ti.ProjectID,
		"SYBRA_TASK_BRANCH="+ti.Branch,
		fmt.Sprintf("SYBRA_TASK_PR=%d", ti.PRNumber),
	)

	output, runErr := cmd.CombinedOutput()
	status := "completed"
	if runErr != nil {
		status = "failed"
	}

	return StepOutput{
		StepID: step.ID,
		Status: status,
		Output: string(output),
	}, nil
}
