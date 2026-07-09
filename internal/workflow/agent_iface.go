package workflow

// AgentCompletion carries the result of an agent run to the workflow engine.
// Using a typed struct instead of positional string args makes the
// success/failure contract explicit and independent of agent package constants.
type AgentCompletion struct {
	AgentID  string
	Result   string
	Provider string
	// Success reports whether the agent exited cleanly. false triggers the
	// step failure/retry path in AdvanceStep.
	Success bool
}

// AgentLauncher starts agents and queries running state.
// `dir` overrides worktree preparation — when non-empty the caller has
// already staged a directory (e.g. PrepareForFix) and the adapter must reuse
// it instead of calling PrepareForTask.
// `oneShot` asks the runner to close stdin after the first `result` event in
// conversational mode so the process exits naturally. Required for interactive
// workflow steps that expect a single turn — otherwise the agent sits paused
// forever and the workflow never advances to the next step.
type AgentLauncher interface {
	StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment AgentAssignment) (agentID, startedDir, baselineRef string, err error)
	HasRunningAgent(taskID string) bool
	// HasOtherRunningAgentForTask reports whether an agent other than
	// exceptAgentID is still running for the task. verify_commits uses it to
	// avoid a premature verdict while a sibling agent is mid-flight, excluding
	// the agent whose completion is currently being processed.
	HasOtherRunningAgentForTask(taskID, exceptAgentID string) bool
	FindRunningAgentForRole(taskID, role string) (agentID string, found bool)
	StopAgentsForTask(taskID string, role string)
	SendPrompt(agentID, message string) error
	DefaultProvider() string
	// ProviderRateLimited reports whether the named provider is in a rate-limit
	// cooldown (distinct from logged out / auth failure). ResumeStalled consults
	// it to wait out a transient throttle without also stalling auth failures,
	// which must take the human-required path. Empty name = default provider.
	ProviderRateLimited(provider string) bool
	// ProviderCanFailover reports whether a currently blocked provider has a
	// healthy peer available for this run.
	ProviderCanFailover(provider string) bool
	// ProviderHealthy reports whether the named provider is currently usable
	// per the health gate — false for both a probe-detected outage and a
	// config-disabled provider. selectABVariant consults it so a
	// config-disabled provider is never picked as an eligible weighted
	// variant. Empty name = default provider.
	ProviderHealthy(provider string) bool
}

// AgentAssignment carries A/B experiment attribution selected before dispatch.
type AgentAssignment struct {
	ExperimentID    string
	Kind            string
	VariantID       string
	Provider        string
	Model           string
	AssignmentUnit  string
	AssignmentKey   string
	ReasoningEffort string
	PromptTransform *PromptTransform
	SkillAliases    map[string]string
}

// PromptTransform mirrors the A/B assignment payload used to rewrite a prompt
// template before workflow rendering.
type PromptTransform struct {
	Op   string
	Text string
}

// CostBudgetChecker is consulted before fanning out best-of-N attempts
// (execBestOfN) and before dispatching a budget-preflight run_agent step such
// as the judge (preflightRunAgentBudget): unlike StartAgentWithAssignment
// (implementation agents on the canonical worktree), the direct-dispatch
// AgentLauncher.StartAgent branch — which best-of-N attempts and the judge
// both use, since they pass a pre-staged `dir` — does not itself enforce the
// cumulative task cost budget. Engine operates with a nil checker: the
// preflight is then skipped rather than panicking, so existing engine unit
// tests that never wire one keep compiling unchanged.
type CostBudgetChecker interface {
	// CheckTaskCostBudget returns workflow.ErrTaskCostExceeded (wrapped) when
	// the task has already spent its configured budget.
	CheckTaskCostBudget(taskID string) error
}

// AttemptWorktreeManager creates, promotes, and cleans up the isolated
// per-attempt worktrees a `best_of_n` step's attempts run in — distinct from
// the task's shared canonical worktree that `parallel` children use. Engine
// operates with a nil manager: a best_of_n/promote_best_of_n step then fails
// closed to human-required with a distinct reason instead of panicking.
type AttemptWorktreeManager interface {
	// PrepareAttempt creates (or resumes) an isolated worktree+branch for one
	// attempt and returns its dir and branch name.
	PrepareAttempt(taskID, attemptID string) (dir, branch string, err error)
	// PromoteAttempt fast-forwards the canonical task branch/worktree onto the
	// winning attempt's HEAD and returns the canonical worktree dir.
	PromoteAttempt(taskID, winnerDir, winnerBranch string) (canonicalDir string, err error)
	// CleanupAttempts best-effort removes the given attempts' worktree dirs.
	// Never returns an error — a leftover directory is disk waste, not a
	// correctness problem, and must never block workflow advancement.
	CleanupAttempts(taskID string, attemptIDs []string)
}

// WorkflowVarDir is the reserved variable name used to pass a pre-prepared
// working directory to run_agent steps, bypassing worktree creation inside
// the engine. Callers set this before StartWorkflowWithVars when they have
// already prepared the worktree (e.g. PR-fix flow that needs PrepareForFix).
const WorkflowVarDir = "_dir"
