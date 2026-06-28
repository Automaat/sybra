package agent

type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
)

// RunConfig is the single entry point for starting any agent.
type RunConfig struct {
	TaskID             string
	Name               string
	Mode               string // "headless", "interactive", or "conversational"
	Prompt             string
	AllowedTools       []string
	Dir                string
	Provider           string // "claude", "codex", or "copilot"
	Model              string // "opus", "sonnet", or full model ID
	ExperimentID       string
	VariantID          string
	AssignmentUnit     string
	AssignmentKey      string
	RequirePermissions bool   // when true, suppress --dangerously-skip-permissions
	PermissionMode     string // "default", "acceptEdits", "bypassPermissions" (conversational mode)
	Effort             string // "low", "medium", "high", "max" (extended thinking)
	// OneShot closes stdin after the first `result` event in conversational
	// mode so the claude process exits naturally. Without this, interactive
	// agents sit in StatePaused forever and onComplete never fires, stranding
	// any workflow that expects the agent to "finish". Ignored in headless mode.
	OneShot bool
	// IgnoreConcurrencyLimit lets an agent start even when MaxConcurrent is
	// saturated. Reserved for system-level long-lived sessions (orchestrator)
	// that must always be runnable regardless of swarm load.
	IgnoreConcurrencyLimit bool
	// IgnoreHealthGate lets an agent start even when the provider health gate
	// marks the requested provider as unhealthy. Reserved for internal probes
	// and system-critical sessions; user-initiated runs leave this false so
	// they surface a clear error instead of wasting a hopeless request.
	IgnoreHealthGate bool
	// DisableProviderFailover keeps provider selection fixed for A/B variants:
	// an unhealthy/limited provider fails the run instead of silently becoming a
	// different provider while retaining stale variant attribution.
	DisableProviderFailover bool
	// ResumeSessionID, when set, passes --resume to the claude CLI so the
	// agent continues a prior conversation instead of starting from scratch.
	// Populated from the task's last AgentRun.SessionID on restart.
	ResumeSessionID string
	// ExtraEnv is a list of "KEY=VALUE" strings appended to the subprocess
	// environment. Used to inject sandbox credentials (SANDBOX_URL, KUBECONFIG).
	ExtraEnv []string
	// MaxTurns overrides the global guardrail for this specific agent run.
	// Zero means "use the manager's global guardrail".
	MaxTurns int
	// BashTimeoutMs sets the Bash tool timeout for this run by exporting
	// BASH_DEFAULT_TIMEOUT_MS and BASH_MAX_TIMEOUT_MS into the claude
	// subprocess (claude exposes no equivalent CLI flag). Zero means "use
	// the manager's default".
	BashTimeoutMs int
	// ForkSubagent, when true, sets CLAUDE_CODE_FORK_SUBAGENT=1 in the claude
	// subprocess environment (claude provider only). Enables parallel subagent
	// spawning from a single prompt at the cost of higher token usage.
	ForkSubagent bool
	// RetryWatchdog, when > 0, sets CLAUDE_CODE_RETRY_WATCHDOG to this value
	// in the claude subprocess environment. Replaces CLAUDE_CODE_MAX_RETRIES
	// (now capped at 15) for headless/unattended server runs. Zero means "use
	// the manager's default".
	RetryWatchdog int
	// FallbackModel, when non-empty, passes --fallback-model to claude.
	// Paired with RetryWatchdog so the watchdog can retry on a less-loaded
	// model when the primary is overloaded. Empty means inherit the manager's
	// default; the flag is omitted only when the manager default is also empty.
	FallbackModel string
	// ReasoningEffort sets codex's model_reasoning_effort (low/medium/high/xhigh)
	// for this run. Empty = model default. Codex-only. NOT the same as Effort
	// (claude --effort) — different provider, CLI surface, and value set.
	ReasoningEffort string
	// SeedWorkingMemory, when true, inlines the worktree's NOTES.md scratchpad
	// into the prompt (read/maintain instruction + current contents). Set only
	// for code-author roles (see Role.AuthorsCode): verifier roles share the
	// implementation worktree, so seeding them would feed an independent
	// reviewer/tester the implementer's notes. No-op if the dir has no NOTES.md.
	SeedWorkingMemory bool
	// OutputSchema is an inline JSON Schema (codex only). The runner writes it
	// to a temp file and passes --output-schema <path> to codex exec. Empty =
	// no schema enforcement. Ignored by claude/copilot.
	OutputSchema string
	// outputSchemaPath is the temp file path the runner wrote OutputSchema to.
	// Set intra-package before buildHeadlessInvocation; cleared by defer after
	// the subprocess exits. Never set by callers.
	outputSchemaPath string
	// HeadlessPermissionMode overrides the permission posture for this run.
	// "auto" emits --permission-mode auto (Claude Code auto-mode classifier).
	// "bypass" (or empty) keeps --dangerously-skip-permissions.
	// Only effective for claude headless runs when AllowedTools is empty and
	// RequirePermissions is false.
	HeadlessPermissionMode string
}
