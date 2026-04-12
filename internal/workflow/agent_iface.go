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
	StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool) (agentID string, err error)
	HasRunningAgent(taskID string) bool
	FindRunningAgentForRole(taskID, role string) (agentID string, found bool)
	StopAgentsForTask(taskID string, role string)
	SendPrompt(agentID, message string) error
	DefaultProvider() string
}

// WorkflowVarDir is the reserved variable name used to pass a pre-prepared
// working directory to run_agent steps, bypassing worktree creation inside
// the engine. Callers set this before StartWorkflowWithVars when they have
// already prepared the worktree (e.g. PR-fix flow that needs PrepareForFix).
const WorkflowVarDir = "_dir"
