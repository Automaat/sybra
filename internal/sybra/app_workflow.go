package sybra

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// Compile-time interface checks.
var (
	_ workflow.TaskProvider           = (*taskAdapter)(nil)
	_ workflow.AgentLauncher          = (*agentAdapter)(nil)
	_ workflow.PRLinker               = (*prLinkerAdapter)(nil)
	_ workflow.PRReviewRequester      = (*prReviewRequesterAdapter)(nil)
	_ workflow.WorktreeGetter         = (*worktreeGetterAdapter)(nil)
	_ workflow.CheckConfigGetter      = (*checkConfigGetterAdapter)(nil)
	_ workflow.ManualTestConfigGetter = (*manualTestConfigGetterAdapter)(nil)
	_ workflow.ArtifactRecorder       = (*artifactRecorderAdapter)(nil)
)

// artifactRecorderAdapter bridges artifact.Store → workflow.ArtifactRecorder.
type artifactRecorderAdapter struct {
	store *artifact.Store
}

func (a *artifactRecorderAdapter) RecordTrace(taskID string, ev any) error {
	return a.store.Append(taskID, artifact.KindTrace, ev)
}

func (a *artifactRecorderAdapter) PutPlanSnapshot(taskID, role, stepID, sourcePath, content string) error {
	name := ""
	if stepID != "" {
		name = "plan-" + stepID + ".md"
	}
	_, err := a.store.Put(taskID, artifact.Artifact{
		Kind:         artifact.KindPlan,
		Name:         name,
		ProducerRole: role,
		StepID:       stepID,
		SourcePath:   sourcePath,
		Content:      []byte(content),
	})
	return err
}

func (a *artifactRecorderAdapter) PutGeneric(taskID, name, stepID, content string) error {
	_, err := a.store.Put(taskID, artifact.Artifact{
		Kind:    artifact.KindGeneric,
		Name:    name,
		StepID:  stepID,
		Content: []byte(content),
	})
	return err
}

// taskAdapter bridges task.Manager → workflow.TaskProvider.
type taskAdapter struct {
	tasks    *task.Manager
	projects *project.Store
}

func (a *taskAdapter) GetTask(id string) (workflow.TaskInfo, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return workflow.TaskInfo{}, err
	}
	info := taskToInfo(t)
	if t.ProjectID != "" && a.projects != nil {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			info.ProjectType = string(p.Type)
		}
	}
	return info, nil
}

func (a *taskAdapter) ListTasks() ([]workflow.TaskInfo, error) {
	tasks, err := a.tasks.List()
	if err != nil {
		return nil, err
	}
	infos := make([]workflow.TaskInfo, 0, len(tasks))
	for i := range tasks {
		if tasks[i].TaskType == task.TaskTypeChat {
			continue
		}
		infos = append(infos, taskToInfo(tasks[i]))
	}
	return infos, nil
}

func (a *taskAdapter) UpdateTaskStatus(id, status, reason string) error {
	st, err := task.ValidateStatus(status)
	if err != nil {
		return err
	}
	u := task.Update{Status: &st}
	if reason != "" {
		u.StatusReason = &reason
	}
	_, err = a.tasks.Update(id, u)
	return err
}

func (a *taskAdapter) UpdateTaskPR(id string, prNumber int) error {
	_, err := a.tasks.Update(id, task.Update{PRNumber: &prNumber})
	return err
}

func (a *taskAdapter) MarkTaskReviewed(id string) error {
	reviewed := true
	_, err := a.tasks.Update(id, task.Update{Reviewed: &reviewed})
	return err
}

func (a *taskAdapter) MarkAgentRunProtocolViolation(taskID, agentID, violation string) error {
	return a.tasks.UpdateRun(taskID, agentID, map[string]any{"protocol_violation": violation})
}

func (a *taskAdapter) MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error {
	updates := map[string]any{"test_outcome": outcome}
	if fingerprint != "" {
		updates["test_failure_fingerprint"] = fingerprint
	}
	return a.tasks.UpdateRun(taskID, agentID, updates)
}

func (a *taskAdapter) AppendTaskBody(id, content string) error {
	_, err := a.tasks.AppendBody(id, content)
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.Update(id, task.Update{Workflow: &wf})
	return err
}

func (a *taskAdapter) ConsumeSupervisorSteer(taskID, prompt string) (string, error) {
	return prependSupervisorSteer(a.tasks, taskID, prompt)
}

func (a *taskAdapter) WriteSidecar(id, kind, content string) error {
	// Plan drafts are namespaced "plan_draft.<name>" so the workflow can fan
	// out to N parallel planners without growing the static sidecar enum.
	// The engine derives <name> from the parallel child step ID.
	if name, ok := strings.CutPrefix(kind, "plan_draft."); ok {
		return a.tasks.PlanDrafts().Write(id, name, content)
	}
	var u task.Update
	switch kind {
	case "plan":
		u.Plan = &content
	case "plan_contract":
		u.PlanContract = &content
	case "code_review":
		u.CodeReview = &content
	case "plan_critique":
		u.PlanCritique = &content
	case "plan_research":
		u.PlanResearch = &content
	case "plan_decisions":
		u.PlanDecisions = &content
	case "plan_brief":
		u.PlanBrief = &content
	default:
		return fmt.Errorf("unknown sidecar kind %q (want plan|plan_contract|code_review|plan_critique|plan_research|plan_decisions|plan_brief|plan_draft.<name>)", kind)
	}
	_, err := a.tasks.Update(id, u)
	return err
}

func taskToInfo(t task.Task) workflow.TaskInfo {
	return workflow.TaskInfo{
		ID:                    t.ID,
		Title:                 t.Title,
		Status:                string(t.Status),
		StatusReason:          t.StatusReason,
		Tags:                  t.Tags,
		AgentMode:             t.AgentMode,
		ProjectID:             t.ProjectID,
		HandoffSourceProvider: t.HandoffSourceProvider,
		PRNumber:              t.PRNumber,
		Branch:                t.Branch,
		Body:                  t.Body,
		Plan:                  t.Plan,
		PlanContract:          t.PlanContract,
		PlanCritique:          t.PlanCritique,
		PlanResearch:          t.PlanResearch,
		PlanDecisions:         t.PlanDecisions,
		PlanBrief:             t.PlanBrief,
		CodeReview:            t.CodeReview,
		PlanDrafts:            t.PlanDrafts,
		Issue:                 t.Issue,
		Reviewed:              t.Reviewed,
		Workflow:              t.Workflow,
		AgentRuns:             toRunInfos(t.AgentRuns),
		TestingCycleStartedAt: t.TestingCycleStartedAt,
	}
}

// toRunInfos projects a task's agent runs onto the engine-visible subset used
// by route_test_result and cross-provider provenance.
func toRunInfos(runs []task.AgentRun) []workflow.AgentRunInfo {
	if len(runs) == 0 {
		return nil
	}
	out := make([]workflow.AgentRunInfo, len(runs))
	for i := range runs {
		out[i] = workflow.AgentRunInfo{
			AgentID:                runs[i].AgentID,
			Role:                   runs[i].Role,
			Provider:               runs[i].Provider,
			StartedAt:              runs[i].StartedAt,
			ProtocolViolation:      runs[i].ProtocolViolation,
			TestOutcome:            runs[i].TestOutcome,
			TestFailureFingerprint: runs[i].TestFailureFingerprint,
			HeadSHA:                runs[i].HeadSHA,
		}
	}
	return out
}

// prLinkerAdapter wires the workflow engine's PRLinker interface to
// the github package. Stateless — all state lives in `gh` / GitHub.
type prLinkerAdapter struct{}

func (prLinkerAdapter) GetClosingIssues(repo string, prNumber int) (issues []int, body string, err error) {
	return github.FetchPRClosingIssues(repo, prNumber)
}

func (prLinkerAdapter) EditBody(repo string, prNumber int, body string) error {
	return github.EditPRBody(repo, prNumber, body)
}

// prReviewRequesterAdapter asks users who left actionable PR feedback to
// review again after the fix-review workflow pushes updated commits.
type prReviewRequesterAdapter struct{}

func (prReviewRequesterAdapter) RerequestReview(repo string, prNumber int) ([]string, error) {
	ctx, err := github.FetchPRContext(repo, prNumber)
	if err != nil {
		return nil, err
	}
	viewer := github.ViewerLogin()
	seen := map[string]struct{}{}
	reviewers := make([]string, 0, len(ctx.Comments))
	for _, c := range ctx.Comments {
		login := strings.TrimSpace(c.Author)
		if !eligibleRerequestReviewer(login, viewer, ctx.Author) {
			continue
		}
		key := strings.ToLower(login)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		reviewers = append(reviewers, login)
	}
	if len(reviewers) == 0 {
		return nil, nil
	}
	if err := github.RequestReviewers(repo, prNumber, reviewers); err != nil {
		return nil, err
	}
	return reviewers, nil
}

func eligibleRerequestReviewer(login, viewer, prAuthor string) bool {
	if login == "" {
		return false
	}
	if strings.EqualFold(login, viewer) || strings.EqualFold(login, prAuthor) {
		return false
	}
	return !strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// worktreeGetterAdapter bridges worktree.Manager + task.Manager → workflow.WorktreeGetter.
type worktreeGetterAdapter struct {
	tasks *task.Manager
	mgr   *worktree.Manager
}

// checkConfigGetterAdapter resolves a task's verify-suite commands by merging
// the repo `.sybra.yaml` checks with the app-level project config.
type checkConfigGetterAdapter struct {
	tasks    *task.Manager
	projects *project.Store
	mgr      *worktree.Manager
}

// manualTestConfigGetterAdapter resolves repo .sybra.yaml manual_test hints.
type manualTestConfigGetterAdapter struct {
	tasks    *task.Manager
	projects *project.Store
	mgr      *worktree.Manager
}

func (a *manualTestConfigGetterAdapter) ManualTestConfig(taskID string) workflow.ManualTestInfo {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return workflow.ManualTestInfo{}
	}

	var repoManual *project.ManualTestConfig
	if a.mgr != nil {
		wtPath := a.mgr.PathFor(t)
		if _, statErr := os.Stat(wtPath); statErr == nil {
			if repoCfg, rErr := project.LoadRepoConfig(wtPath); rErr == nil && repoCfg != nil {
				repoManual = repoCfg.ManualTest
			}
		}
	}

	var appManual *project.ManualTestConfig
	if t.ProjectID != "" && a.projects != nil {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			appManual = p.ManualTest
		}
	}
	manual := project.MergeManualTest(repoManual, appManual)
	if manual == nil {
		return workflow.ManualTestInfo{}
	}
	return workflow.ManualTestInfo{
		Kind:          string(manual.Kind),
		Command:       manual.Command,
		HealthURL:     manual.HealthURL,
		ProbeCommands: manual.ProbeCommands,
	}
}

func (a *checkConfigGetterAdapter) VerifyCommands(taskID string) []string {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return nil
	}
	wtPath := a.mgr.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		return nil
	}
	var repoChecks *project.ChecksConfig
	if repoCfg, rErr := project.LoadRepoConfig(wtPath); rErr == nil && repoCfg != nil {
		repoChecks = repoCfg.Checks
	}
	var appChecks *project.ChecksConfig
	if t.ProjectID != "" {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			appChecks = p.Checks
		}
	}
	merged := project.MergeChecks(repoChecks, appChecks)
	if merged == nil {
		return nil
	}
	return merged.Verify
}

func (a *worktreeGetterAdapter) GetWorktreePath(taskID string) (string, bool) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return "", false
	}
	path := a.mgr.PathFor(t)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

// agentAdapter bridges agent.Manager + AgentOrchestrator → workflow.AgentLauncher.
type agentAdapter struct {
	agents     *agent.Manager
	agentOrch  *AgentOrchestrator
	tasks      *task.Manager
	sandboxes  *sandbox.Manager
	experience *experience.Store
}

func (a *agentAdapter) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema string, assignment workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	// For implementation agents without a pre-staged dir, use the full
	// orchestrator (handles worktree, project assignment). A workflow that
	// seeds WorkflowVarDir (e.g. tests or flows that pre-stage via
	// PrepareForFix) bypasses the orchestrator's worktree path and uses the
	// caller-provided dir directly.
	if (role == "" || role == string(agent.RoleImplementation)) && dir == "" {
		ag, baselineRef, err := a.agentOrch.StartAgentWithAssignment(taskID, mode, prompt, false, oneShot, assignment)
		if err != nil {
			return "", "", "", err
		}
		return ag.ID, "", baselineRef, nil
	}

	// For system agents (triage, eval, plan, etc.), build RunConfig directly.
	r := agent.Role(role)
	t, err := a.agentOrch.tasks.Get(taskID)
	if err != nil {
		return "", "", "", err
	}

	// Cap concurrent test-runner agents per machine — each one starts an
	// isolated real-app/cluster sandbox, so this bounds sandbox load
	// independently of Agent.MaxConcurrent. The benign race (two dispatches
	// passing the check at once) is acceptable: the workflow step parks on
	// ErrTestRunnerBusy and ResumeStalled retries when a slot frees.
	if err := a.ensureTestRunnerCapacity(r); err != nil {
		return "", "", "", err
	}

	posture, postureErr := resolveHeadlessPermissionMode(t, a.agentOrch.cfg)
	if postureErr != nil {
		return "", "", "", postureErr
	}

	cfg := agent.RunConfig{
		TaskID:                  taskID,
		Name:                    r.AgentName(t.Title),
		Mode:                    mode,
		Prompt:                  prompt,
		AllowedTools:            allowedTools,
		Model:                   model,
		Provider:                provider,
		ExperimentID:            assignment.ExperimentID,
		VariantID:               assignment.VariantID,
		AssignmentUnit:          assignment.AssignmentUnit,
		AssignmentKey:           assignment.AssignmentKey,
		DisableProviderFailover: assignment.ExperimentID != "",
		Dir:                     dir,
		OneShot:                 oneShot,
		MaxTurns:                t.MaxTurns,
		RequirePermissions:      resolvePermission(t, a.agentOrch.cfg),
		HeadlessPermissionMode:  posture,
		ReasoningEffort:         firstNonEmpty(assignment.ReasoningEffort, t.ReasoningEffort),
		// Code-author roles (implementation/fix-review/pr-fix) are primed with
		// NOTES.md; verifier roles (review/test-runner/eval) share the same
		// worktree but must stay independent of the implementer's scratchpad.
		SeedWorkingMemory: r.AuthorsCode(),
		OutputSchema:      outputSchema,
	}
	a.withExperiencePrompt(&cfg, r, t)

	if cfg.Dir == "" && needsWorktree {
		t = a.agentOrch.autoAssignProject(t)
		if t.ProjectID == "" {
			return "", "", "", fmt.Errorf("task %s has no project_id: refusing to start %s agent without isolated worktree", taskID, role)
		}
		d, wtErr := a.agentOrch.worktrees.PrepareForTask(t, nil)
		if wtErr != nil {
			markRebaseBlocked(a.tasks, taskID, wtErr, a.agentOrch.logger)
			return "", "", "", wtErr
		}
		cfg.Dir = d
	}
	if cfg.Dir == "" {
		// System-role fallback (triage, plan, eval, …): no worktree required,
		// but the agent process still needs an existing cwd. Use the sybra
		// home dir rather than letting the process inherit Sybra's own cwd
		// (which in dev mode would be the sybra source repo — the bug that
		// caused branch changes on main).
		cfg.Dir = config.HomeDir()
	}

	baselineRef = currentWorktreeHead(cfg.Dir)

	if a.sandboxes != nil {
		if r == agent.RoleTestRunner {
			cfg.ExtraEnv = a.agentOrch.sandboxEnv(taskID, cfg.Dir, t)
		} else if inst := a.sandboxes.Get(taskID); inst != nil {
			cfg.ExtraEnv = inst.EnvVars()
		}
	}

	ag, err := a.agents.Run(cfg)
	if err != nil {
		return "", "", "", err
	}

	a.recordSystemAgentStart(taskID, role, mode, cfg, ag)

	return ag.ID, cfg.Dir, baselineRef, nil
}

func (a *agentAdapter) recordSystemAgentStart(taskID, role, mode string, cfg agent.RunConfig, ag *agent.Agent) {
	var nextStatus *task.Status
	if cur, rerr := a.tasks.Get(taskID); rerr == nil && cur.Status == task.StatusHumanRequired {
		nextStatus = task.Ptr(task.StatusInProgress)
	}
	if addErr := a.tasks.AddRunWithStatus(taskID, task.AgentRun{
		AgentID:         ag.ID,
		Role:            role,
		Mode:            mode,
		Provider:        ag.Provider,
		Model:           ag.Model,
		ExperimentID:    ag.ExperimentID,
		VariantID:       ag.VariantID,
		AssignmentUnit:  ag.AssignmentUnit,
		AssignmentKey:   ag.AssignmentKey,
		ReasoningEffort: ag.ReasoningEffort,
		OneShot:         cfg.OneShot,
		State:           string(agent.StateRunning),
		StartedAt:       ag.StartedAt,
		Prompt:          cfg.Prompt,
	}, nextStatus); addErr != nil {
		slog.Error("agent-adapter.add-run", "task_id", taskID, "agent_id", ag.ID, "err", addErr)
	}
}

func currentWorktreeHead(dir string) string {
	if dir == "" {
		return ""
	}
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (a *agentAdapter) withExperiencePrompt(cfg *agent.RunConfig, role agent.Role, t task.Task) {
	if a == nil || a.experience == nil || a.agentOrch == nil || a.agentOrch.cfg == nil {
		return
	}
	if role != agent.RolePlan || !a.agentOrch.cfg.Experience.Enabled || t.ProjectID == "" {
		return
	}
	records, err := a.experience.Query(t.ProjectID, a.agentOrch.cfg.Experience.MaxRecords)
	if err != nil || len(records) == 0 {
		return
	}
	appendix := experience.FormatForPrompt(records)
	if appendix == "" {
		return
	}
	cfg.Prompt += appendix
	ids := make([]string, 0, len(records))
	for i := range records {
		ids = append(ids, records[i].TaskID)
	}
	a.agentOrch.logAudit(audit.EventExperienceInjected, t.ID, "", map[string]any{
		"project_id": t.ProjectID,
		"record_ids": ids,
	})
}

func (a *agentAdapter) ensureTestRunnerCapacity(role agent.Role) error {
	if role == agent.RoleTestRunner && a.agents.CountLiveByRole(agent.RoleTestRunner) >= a.agentOrch.cfg.TestingMaxConcurrent() {
		return workflow.ErrTestRunnerBusy
	}
	return nil
}

func (a *agentAdapter) HasRunningAgent(taskID string) bool {
	return a.agents.HasRunningAgentForTask(taskID)
}

func (a *agentAdapter) HasOtherRunningAgentForTask(taskID, exceptAgentID string) bool {
	return a.agents.HasOtherRunningAgentForTask(taskID, exceptAgentID)
}

func (a *agentAdapter) FindRunningAgentForRole(taskID, role string) (string, bool) {
	r := agent.Role(role)
	ag := a.agents.FindRunningAgentForTask(taskID, r)
	if ag == nil {
		return "", false
	}
	return ag.ID, true
}

func (a *agentAdapter) StopAgentsForTask(taskID, role string) {
	r := agent.Role(role)
	for _, ag := range a.agents.FindAllRunningAgentsForTask(taskID, r) {
		_ = a.agents.StopAgent(ag.ID)
	}
}

func (a *agentAdapter) SendPrompt(agentID, message string) error {
	return a.agents.SendPromptToAgent(agentID, message)
}

func (a *agentAdapter) DefaultProvider() string {
	return a.agents.DefaultProvider()
}

func (a *agentAdapter) ProviderRateLimited(provider string) bool {
	return a.agents.ProviderRateLimited(provider)
}

func (a *agentAdapter) ProviderCanFailover(provider string) bool {
	return a.agents.ProviderCanFailover(provider)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
