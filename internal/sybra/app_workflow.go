package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/experience"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/prcontent"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sandbox"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/triage"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
	"github.com/Automaat/sybra/internal/worktreeerr"
)

// Compile-time interface checks.
var (
	_ workflow.TaskProvider           = (*taskAdapter)(nil)
	_ workflow.TaskClassifier         = (*taskClassifierAdapter)(nil)
	_ workflow.AgentLauncher          = (*agentAdapter)(nil)
	_ workflow.PRLinker               = (*prLinkerAdapter)(nil)
	_ workflow.PRStateFetcher         = (*prStateFetcherAdapter)(nil)
	_ workflow.PRHeadFetcher          = (*prHeadFetcherAdapter)(nil)
	_ workflow.PRCreator              = (*prCreatorAdapter)(nil)
	_ workflow.PRFinder               = (*prFinderAdapter)(nil)
	_ workflow.PRContentGenerator     = (*prContentGeneratorAdapter)(nil)
	_ workflow.PRReviewRequester      = (*prReviewRequesterAdapter)(nil)
	_ workflow.WorktreeGetter         = (*worktreeGetterAdapter)(nil)
	_ workflow.AttemptNoteAppender    = (*attemptNoteAppenderAdapter)(nil)
	_ workflow.BranchSyncer           = (*branchSyncerAdapter)(nil)
	_ workflow.CheckConfigGetter      = (*checkConfigGetterAdapter)(nil)
	_ workflow.ManualTestConfigGetter = (*manualTestConfigGetterAdapter)(nil)
	_ workflow.ArtifactRecorder       = (*artifactRecorderAdapter)(nil)
	_ workflow.CostBudgetChecker      = (*agentAdapter)(nil)
	_ workflow.AttemptWorktreeManager = (*attemptWorktreeAdapter)(nil)
)

// attemptWorktreeAdapter bridges worktree.Manager → workflow.AttemptWorktreeManager.
type attemptWorktreeAdapter struct {
	tasks *task.Manager
	mgr   *worktree.Manager
}

func (a *attemptWorktreeAdapter) PrepareAttempt(taskID, attemptID string) (dir, branch string, err error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return "", "", fmt.Errorf("get task: %w", err)
	}
	// context.Background(): AttemptWorktreeManager is a fixed interface
	// signature invoked from workflow step execution, which never threads a
	// caller ctx today (see the identical rationale on PrepareForTask calls
	// elsewhere in this file).
	return a.mgr.PrepareAttempt(context.Background(), t, attemptID)
}

func (a *attemptWorktreeAdapter) PromoteAttempt(taskID, winnerDir, winnerBranch string) (string, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	return a.mgr.PromoteAttempt(context.Background(), t, winnerDir, winnerBranch)
}

func (a *attemptWorktreeAdapter) CleanupAttempts(taskID string, attemptIDs []string) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	a.mgr.CleanupAttempts(context.Background(), t, attemptIDs)
}

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
	return a.tasks.UpdateRun(taskID, agentID, task.RunPatch{ProtocolViolation: task.Ptr(violation)})
}

func (a *taskAdapter) MarkAgentRunTestOutcome(taskID, agentID, outcome, fingerprint string) error {
	patch := task.RunPatch{TestOutcome: task.Ptr(outcome)}
	if fingerprint != "" {
		patch.TestFailureFingerprint = task.Ptr(fingerprint)
	}
	return a.tasks.UpdateRun(taskID, agentID, patch)
}

func (a *taskAdapter) AppendTaskBody(id, content string) error {
	_, err := a.tasks.AppendBody(id, content)
	return err
}

func (a *taskAdapter) ReplaceTaskBody(id, body string) error {
	_, err := a.tasks.Update(id, task.Update{Body: &body})
	return err
}

func (a *taskAdapter) SetWorkflow(id string, wf *workflow.Execution) error {
	_, err := a.tasks.Update(id, task.Update{Workflow: &wf})
	return err
}

func (a *taskAdapter) ConsumeSupervisorSteer(taskID, prompt string) (string, error) {
	return agentorch.PrependSupervisorSteer(a.tasks, taskID, prompt)
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

// taskClassifierAdapter bridges internal/triage's deterministic classifier to
// workflow.TaskClassifier for the `classify_task` step. It runs the same
// classify+apply pipeline as `sybra-cli triage classify <id>` and the
// poll-based auto-triage handler (internal/poll.TriageHandler), so the
// workflow step no longer needs a full agent session to reach it.
type taskClassifierAdapter struct {
	tasks      *task.Manager
	projects   *project.Store
	classifier triage.Classifier
	audit      *audit.Logger
}

func (a *taskClassifierAdapter) ClassifyTask(ctx context.Context, taskID string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}
	var projects []project.Project
	if a.projects != nil {
		projects, err = a.projects.List()
		if err != nil {
			return err
		}
	}
	_, _, err = triage.ClassifyAndApply(ctx, a.classifier, a.tasks, a.audit, t, projects)
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

// prStateFetcherAdapter wires the workflow engine's PRStateFetcher interface
// to the github package. Stateless — all state lives in `gh` / GitHub.
type prStateFetcherAdapter struct{}

func (prStateFetcherAdapter) FetchPRState(repo string, number int) (github.PRState, error) {
	return github.FetchPRState(repo, number)
}

// prHeadFetcherAdapter wires the workflow engine's PRHeadFetcher interface to
// the github package. Stateless — all state lives in `gh` / GitHub.
type prHeadFetcherAdapter struct{}

func (prHeadFetcherAdapter) FetchPRHeadSHA(ctx context.Context, repo string, number int) (string, error) {
	return github.FetchPRHeadSHAContext(ctx, repo, number)
}

// prCreatorAdapter wires the workflow engine's PRCreator interface to the
// github package. Stateless — all state lives in `gh` / GitHub.
type prCreatorAdapter struct{}

func (prCreatorAdapter) CreatePR(ctx context.Context, dir string, req workflow.PRCreateRequest) (number int, headSHA string, err error) {
	return github.CreatePR(ctx, dir, github.CreatePRRequest{
		Repo:  req.Repo,
		Head:  req.Head,
		Draft: req.Draft,
		Title: req.Title,
		Body:  req.Body,
	})
}

// prFinderAdapter wires the workflow engine's PRFinder interface to the github
// package. Stateless — all state lives in `gh` / GitHub.
type prFinderAdapter struct{}

func (prFinderAdapter) FindPRForBranch(ctx context.Context, repo, head string) (number int, found bool, err error) {
	return github.FindPRForBranch(ctx, repo, head)
}

// prContentGeneratorAdapter wires the workflow engine's PRContentGenerator
// interface to internal/prcontent's LLM-backed drafter.
type prContentGeneratorAdapter struct {
	gen prcontent.Generator
}

func (a prContentGeneratorAdapter) GeneratePRContent(ctx context.Context, taskTitle, taskBody string, commitSubjects []string) (title, body string, err error) {
	c, err := a.gen.Generate(ctx, prcontent.Request{
		TaskTitle:      taskTitle,
		TaskBody:       taskBody,
		CommitSubjects: commitSubjects,
	})
	if err != nil {
		return "", "", err
	}
	return c.Title, c.Body, nil
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

type attemptNoteAppenderAdapter struct{}

// checkConfigGetterAdapter resolves a task's codegen/verify commands by merging
// the repo `.sybra.yaml` checks (read from the project's trusted default
// branch, never the checked-out worktree — see resolveTrustedSetupCommands
// and issue #1519) with the app-level project config.
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

func (a *checkConfigGetterAdapter) CodegenCommands(ctx context.Context, taskID string) []string {
	merged := a.mergedChecks(ctx, taskID)
	if merged == nil {
		return nil
	}
	return merged.Codegen
}

func (a *checkConfigGetterAdapter) VerifyCommands(ctx context.Context, taskID string) []string {
	merged := a.mergedChecks(ctx, taskID)
	if merged == nil {
		return nil
	}
	return merged.Verify
}

func (a *checkConfigGetterAdapter) mergedChecks(ctx context.Context, taskID string) *project.ChecksConfig {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return nil
	}
	wtPath := a.mgr.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		return nil
	}
	var repoChecks *project.ChecksConfig
	var appChecks *project.ChecksConfig
	if t.ProjectID != "" {
		if p, pErr := a.projects.Get(t.ProjectID); pErr == nil {
			appChecks = p.Checks
			// Read checks.{codegen,verify} from the project's trusted default
			// branch, never the checked-out worktree: the worktree's own
			// .sybra.yaml may carry malicious commands planted by a
			// compromised or prompt-injected agent, and these commands run
			// unsandboxed
			// via `sh -c` (see resolveTrustedSetupCommands, issue #1519).
			// ctx carries the caller's step deadline so a hung
			// git show/symbolic-ref on the bare repo can't block indefinitely.
			if repoCfg, rErr := project.LoadRepoConfigAtDefaultBranch(ctx, p.ClonePath); rErr == nil && repoCfg != nil {
				repoChecks = repoCfg.Checks
			}
		}
	}
	return project.MergeChecks(repoChecks, appChecks)
}

func (a *checkConfigGetterAdapter) SetupCommands(ctx context.Context, taskID string) []string {
	t, err := a.tasks.Get(taskID)
	if err != nil || t.ProjectID == "" {
		return nil
	}
	wtPath := a.mgr.PathFor(t)
	if _, statErr := os.Stat(wtPath); statErr != nil {
		return nil
	}
	p, pErr := a.projects.Get(t.ProjectID)
	if pErr != nil {
		return nil
	}
	var repoSetup []string
	if repoCfg, rErr := project.LoadRepoConfigAtDefaultBranch(ctx, p.ClonePath); rErr == nil && repoCfg != nil {
		repoSetup = repoCfg.Setup
	}
	return project.MergeSetup(repoSetup, p.SetupCommands)
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

func (*attemptNoteAppenderAdapter) AppendReimplementNote(ctx context.Context, _, wtPath, marker, note string) error {
	return worktree.AppendNote(ctx, wtPath, marker, note)
}

// branchSyncerAdapter bridges task.Manager + worktree.Manager → workflow.BranchSyncer.
type branchSyncerAdapter struct {
	tasks *task.Manager
	mgr   *worktree.Manager
}

func (a *branchSyncerAdapter) SyncTaskBranch(ctx context.Context, taskID string) (string, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return worktree.SyncFailed.String(), fmt.Errorf("sync branch: get task: %w", err)
	}
	result, err := a.mgr.SyncTaskBranch(ctx, t)
	return result.String(), err
}

// agentAdapter bridges agent.Manager + agentorch.Orchestrator → workflow.AgentLauncher.
type agentAdapter struct {
	agents     *agent.Manager
	agentOrch  *agentorch.Orchestrator
	tasks      *task.Manager
	projects   *project.Store
	sandboxes  *sandbox.Manager
	experience *experience.Store
}

func translatePoolBusy(err error) error {
	if errors.Is(err, agent.ErrMaxConcurrentReached) {
		return fmt.Errorf("%w: %w", workflow.ErrAgentPoolBusy, err)
	}
	return err
}

func (a *agentAdapter) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	// For implementation agents without a pre-staged dir, use the full
	// orchestrator (handles worktree, project assignment). A workflow that
	// seeds WorkflowVarDir (e.g. tests or flows that pre-stage via
	// PrepareForFix) bypasses the orchestrator's worktree path and uses the
	// caller-provided dir directly.
	if (role == "" || role == string(agent.RoleImplementation)) && dir == "" {
		// agentOrch.StartAgentWithAssignment already translates
		// agent.ErrMaxConcurrentReached into workflow.ErrAgentPoolBusy at the
		// source, so every caller (this adapter, recovery.Recovery) sees the
		// same benign sentinel without needing its own wrap here.
		ag, baselineRef, err := a.agentOrch.StartAgentWithAssignment(taskID, mode, prompt, false, oneShot, cleanRetryRef, assignment)
		if err != nil {
			return "", "", "", err
		}
		return ag.ID, "", baselineRef, nil
	}

	// For roles that don't go through StartAgentWithAssignment (triage, eval,
	// plan, pr-fix, fix-review, test-runner, ...), build RunConfig directly.
	r := agent.Role(role)
	// claimDirectDispatch serializes this path per task, closing the same
	// check-then-act race StartAgentWithAssignment closes for implementation
	// agents: without it, two dispatchers (e.g. a fast ResumeStalled retry)
	// can each observe no running agent and start a duplicate agent against
	// the same task/worktree. claim.Release is idempotent, so the
	// worktree-prep recovery path below can release it early (to unblock a
	// nested same-task recovery dispatch) without this deferred call
	// double-releasing on return.
	claim, ok := a.agents.TryClaimDispatch(taskID)
	if !ok {
		return "", "", "", workflow.ErrDispatchInFlight
	}
	defer claim.Release()

	t, err := a.tasks.Get(taskID)
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

	posture, postureErr := agentorch.ResolveHeadlessPermissionMode(t, a.agentOrch.Cfg())
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
		RequirePermissions:      agentorch.ResolvePermission(t, a.agentOrch.Cfg()),
		HeadlessPermissionMode:  posture,
		ReasoningEffort:         agentorch.FirstNonEmpty(assignment.ReasoningEffort, t.ReasoningEffort, agentorch.ResolveRoleEffort(r, a.agentOrch.Cfg())),
		// Code-author roles (implementation/fix-review/pr-fix) are primed with
		// NOTES.md; verifier roles (review/test-runner/eval) share the same
		// worktree but must stay independent of the implementer's scratchpad.
		SeedWorkingMemory: r.AuthorsCode(),
		// fork_subagent is a task-level opt-in, but must never reach a
		// verifier role (review/test-runner/eval) — a forked subagent's own
		// token spend would multiply on every independent check, and a
		// verifier has no need for the parallelism it buys an implementer.
		ForkSubagent: t.ForkSubagent && r.AuthorsCode(),
		OutputSchema: outputSchema,
	}
	a.withExperiencePrompt(&cfg, r, t)

	cleanRetryReset := false
	if cfg.Dir == "" && needsWorktree {
		var dir string
		t, dir, cleanRetryReset, err = a.resolveWorktreeDir(t, taskID, role, cleanRetryRef, claim)
		if err != nil {
			return "", "", "", err
		}
		cfg.Dir = dir
	}
	if cfg.Dir != "" && cleanRetryRef != "" && !cleanRetryReset {
		if resetErr := a.resetWorktreeForRetry(t, cfg.Dir, cleanRetryRef); resetErr != nil {
			return "", "", "", resetErr
		}
	}
	if cfg.Dir == "" {
		// System-role fallback (triage, plan, eval, …): no worktree required,
		// but the agent process still needs an existing cwd. Use the sybra
		// home dir rather than letting the process inherit Sybra's own cwd
		// (which in dev mode would be the sybra source repo — the bug that
		// caused branch changes on main).
		cfg.Dir = config.HomeDir()
	}

	baselineRef = agentorch.CurrentWorktreeHead(cfg.Dir)
	a.configureTestRunnerRun(&cfg, taskID, r, t)

	ag, err := a.agents.Run(cfg)
	if err != nil {
		return "", "", "", translatePoolBusy(err)
	}

	a.recordSystemAgentStart(taskID, role, mode, cfg, ag)

	return ag.ID, cfg.Dir, baselineRef, nil
}

func (a *agentAdapter) configureTestRunnerRun(cfg *agent.RunConfig, taskID string, role agent.Role, t task.Task) {
	if a.sandboxes != nil {
		if role == agent.RoleTestRunner {
			cfg.ExtraEnv = a.agentOrch.SandboxEnv(taskID, cfg.Dir, t)
		} else if inst := a.sandboxes.Get(taskID); inst != nil {
			cfg.ExtraEnv = inst.EnvVars()
		}
	}
	if role != agent.RoleTestRunner {
		return
	}
	// Eligibility only — Manager.preparePlaywrightMCP decides whether to
	// actually attach the MCP server, gated on config enablement and the
	// FINAL resolved provider (not this raw role/provider check), so a
	// test-runner that fails over to codex never gets a claude-only flag.
	cfg.PlaywrightMCPEligible = true
	cfg.PlaywrightMCPOutputDir = filepath.Join(cfg.Dir, worktree.EvidenceDirName)
}

// resolveWorktreeDir auto-assigns a project to t (if needed), optionally
// resets the worktree for a clean retry, and prepares the worktree dir for
// the direct-dispatch path. claim is the caller's held dispatch claim: on a
// worktree-prep failure this releases it early (see the claim.Release() call
// below) rather than the caller's own deferred release, which — since
// DispatchClaim.Release is idempotent — is then a safe no-op.
func (a *agentAdapter) resolveWorktreeDir(t task.Task, taskID, role, cleanRetryRef string, claim *agent.DispatchClaim) (updated task.Task, dir string, cleanRetryReset bool, err error) {
	t, err = a.agentOrch.AutoAssignProject(t)
	if err != nil {
		return t, "", false, err
	}
	if t.ProjectID == "" {
		return t, "", false, fmt.Errorf("task %s has no project_id: refusing to start %s agent without isolated worktree: %w", taskID, role, workflow.ErrNoProjectAssigned)
	}
	if cleanRetryRef != "" {
		if resetErr := a.resetWorktreeForRetry(t, "", cleanRetryRef); resetErr != nil {
			return t, "", false, resetErr
		}
		cleanRetryReset = true
	}
	// context.Background(): StartAgent implements workflow.AgentDispatcher,
	// a fixed interface signature with no ctx parameter (invoked from many
	// workflow step-execution call sites); see the Engine.SetContext /
	// e.ctx pattern for why threading ctx across that interface is out of
	// scope for this pass.
	d, wtErr := a.agentOrch.Worktrees().PrepareForTask(context.Background(), t, nil)
	if wtErr != nil {
		// Release our dispatch claim before classifying/recovering: a
		// rebase-blocked wtErr routes through RecoverFromWorktreePrepFailure
		// -> RecoverStaleBranchConflict, which synchronously starts the
		// branch-conflict-fix workflow and dispatches ITS OWN "fix" agent for
		// this same taskID. dispatchClaims is a non-reentrant per-task map
		// (agent.Manager.ClaimTaskDispatch), so if we still held the claim
		// here, that nested dispatch would collide with it and park on
		// ErrDispatchInFlight without ever starting the conflict-resolution
		// agent. We're bailing out of this dispatch
		// attempt regardless (wtErr != nil means we never call a.agents.Run
		// below), so releasing early is safe: it doesn't overlap with our own
		// (never-attempted) agent start.
		claim.Release()
		return t, "", cleanRetryReset, a.classifyDirectDispatchWorktreeErr(taskID, wtErr)
	}
	return t, d, cleanRetryReset, nil
}

// classifyDirectDispatchWorktreeErr translates a PrepareForTask failure from
// the direct-dispatch path into the error execRunAgent should see. A tracked
// agent still live in the worktree (worktree.ErrAgentRunning) is a benign
// timing collision with a stale "no agent running" read upstream, not a real
// worktree conflict — treat it like ErrDispatchInFlight so the step parks
// and retries once the agent is genuinely idle, instead of escalating.
func (a *agentAdapter) classifyDirectDispatchWorktreeErr(taskID string, wtErr error) error {
	if errors.Is(wtErr, worktreeerr.ErrAgentRunning) {
		return workflow.ErrDispatchInFlight
	}
	// handled=true covers every branch of RecoverFromWorktreePrepFailure that
	// already wrote the task's terminal status itself: an autonomous
	// conflict-fix redispatch (recovered=true), MarkRebaseBlocked parking the
	// task at human-required, or its already-resolved-on-remote downgrade to
	// in_review. Only checking recovered (as before) let an unhandled-but-not-
	// recovered rebase failure fall through to `return wtErr` below even
	// though the status was already resolved — the caller's surfaceStartFailure
	// would then reclassify the same wtErr and clobber that resolved status
	// (e.g. overwriting in_review back to human-required) using a stale
	// pre-dispatch status snapshot.
	if handled, _ := a.agentOrch.RecoverFromWorktreePrepFailure(a.tasks, taskID, wtErr); handled {
		return workflow.ErrDispatchInFlight
	}
	return wtErr
}

func (a *agentAdapter) resetWorktreeForRetry(t task.Task, dir, ref string) error {
	target := dir
	if target == "" {
		if t.WorktreeDir != "" {
			target = t.WorktreeDir
		} else {
			target = a.agentOrch.Worktrees().PathFor(t)
		}
	}
	if target == "" {
		return nil
	}
	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat clean retry worktree: %w", err)
	}
	// context.Background(): StartAgent implements workflow.AgentDispatcher,
	// a fixed interface signature with no ctx parameter (see the earlier
	// comment on the PrepareForTask call in this file).
	if err := project.ResetWorktreeForRetry(context.Background(), target, ref); err != nil {
		a.agentOrch.Logger().Warn("worktree.clean-retry.reset", "task_id", t.ID, "path", target, "ref", ref, "err", err)
		return err
	}
	a.agentOrch.Logger().Info("worktree.clean-retry.reset", "task_id", t.ID, "path", target, "ref", ref)
	return nil
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
		if errors.Is(addErr, os.ErrNotExist) {
			// A stale workflow dispatcher can lose the task underneath it
			// (delete/cleanup/terminal teardown) after StartAgent succeeded but
			// before the AgentRun write. Treat that as a silent no-op: the
			// workflow already no longer owns a task file to update.
			return
		}
		slog.Error("agent-adapter.add-run", "task_id", taskID, "agent_id", ag.ID, "err", addErr)
	}
}

func (a *agentAdapter) withExperiencePrompt(cfg *agent.RunConfig, role agent.Role, t task.Task) {
	if a == nil || a.experience == nil || a.agentOrch == nil || a.agentOrch.Cfg() == nil {
		return
	}
	if !roleReceivesExperience(role) || !a.agentOrch.Cfg().Experience.Enabled || t.ProjectID == "" {
		return
	}
	projStore := a.projects
	if projStore == nil {
		projStore = a.agentOrch.Projects()
	}
	if projStore == nil {
		return
	}
	proj, err := projStore.Get(t.ProjectID)
	if err != nil || proj.ID == "" || !a.agentOrch.Cfg().AllowsProjectType(string(proj.Type)) {
		return
	}
	projectKey := experience.ProjectKey(proj)
	records, err := a.experience.Query(projectKey, a.agentOrch.Cfg().Experience.MaxRecords)
	if err != nil || len(records) == 0 {
		return
	}
	// Gate each candidate on TTL + tag-overlap trigger instead of injecting
	// every retained record unconditionally — see experience.Eligible.
	ttlDays := a.agentOrch.Cfg().Experience.TTLDays
	now := time.Now()
	records = slices.DeleteFunc(records, func(rec experience.Record) bool {
		return !experience.Eligible(rec, t.Tags, ttlDays, now)
	})
	if len(records) == 0 {
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
	data := map[string]any{"record_ids": ids, "role": string(role)}
	if proj.Type == project.ProjectTypeWork {
		data["project_key"] = projectKey
	} else {
		data["project_id"] = t.ProjectID
	}
	a.agentOrch.LogAudit(audit.EventExperienceInjected, t.ID, "", data)
}

func roleReceivesExperience(role agent.Role) bool {
	return role == agent.RolePlan || role == agent.RoleTriage
}

func (a *agentAdapter) ensureTestRunnerCapacity(role agent.Role) error {
	if role == agent.RoleTestRunner && a.agents.CountLiveByRole(agent.RoleTestRunner) >= a.agentOrch.Cfg().TestingMaxConcurrent() {
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

func (a *agentAdapter) ProviderHealthy(provider string) bool {
	return a.agents.ProviderHealthy(provider)
}

func (a *agentAdapter) TryClaimDispatch(taskID string) (workflow.DispatchClaim, bool) {
	return a.agents.TryClaimDispatch(taskID)
}

func (a *agentAdapter) IsDispatching(taskID string) bool {
	return a.agents.IsDispatching(taskID)
}

// CheckTaskCostBudget implements workflow.CostBudgetChecker for the
// best_of_n/judge preflight — see agentorch.Orchestrator.CheckTaskCostBudget.
func (a *agentAdapter) CheckTaskCostBudget(taskID string) error {
	return a.agentOrch.CheckTaskCostBudget(taskID)
}
