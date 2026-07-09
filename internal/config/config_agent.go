package config

type AgentDefaults struct {
	Provider           string  `yaml:"provider" json:"provider"`
	Model              string  `yaml:"model" json:"model"`
	Mode               string  `yaml:"mode" json:"mode"`
	MaxConcurrent      int     `yaml:"max_concurrent" json:"maxConcurrent"`
	ResearchMachineDir string  `yaml:"research_machine_dir" json:"researchMachineDir"`
	MaxCostUSD         float64 `yaml:"max_cost_usd" json:"maxCostUsd"`
	MaxTurns           int     `yaml:"max_turns" json:"maxTurns"`
	// MaxTaskCostUSD caps the cumulative USD cost across every AgentRun a task
	// has ever had (unlike MaxCostUSD, which resets every run). Closes the gap
	// where each retry stays under the per-run cap but the task's total spend
	// still balloons unbounded. Checked once per dispatch, before an agent is
	// started — StartAgentWithAssignment refuses to start and flips the task
	// to human-required when the task's already-recorded AgentRuns.CostUSD sum
	// meets or exceeds this. 0 (default) disables the check.
	MaxTaskCostUSD float64 `yaml:"max_task_cost_usd" json:"maxTaskCostUsd"`
	// TurnCostFraction is the fraction of MaxCostUSD below which a turns
	// escalation is auto-continued. Default 0.8 when unset.
	TurnCostFraction float64 `yaml:"turn_cost_fraction" json:"turnCostFraction"`
	// TurnMultiplier scales the turn limit on each auto-continuation. Default 2 when unset.
	TurnMultiplier float64 `yaml:"turn_multiplier" json:"turnMultiplier"`
	// RequirePermissions sets the default permission requirement for agents.
	// nil means not configured (falls back to true — safe default).
	// Set to false in config to opt all tasks into skip-permissions mode.
	RequirePermissions *bool `yaml:"require_permissions" json:"requirePermissions"`
	// BashTimeoutSeconds sets the per-bash-tool-call timeout passed to
	// claude -p via the BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS env
	// vars (claude has no equivalent CLI flag). 0 means use
	// DefaultBashTimeoutSeconds (300).
	BashTimeoutSeconds int `yaml:"bash_timeout_seconds" json:"bashTimeoutSeconds"`
	// RetryWatchdog sets CLAUDE_CODE_RETRY_WATCHDOG on the claude subprocess
	// for headless (unattended) runs. Replaces CLAUDE_CODE_MAX_RETRIES (now
	// capped at 15) for server/unattended sessions. 0 means use
	// DefaultRetryWatchdog (30). Negative (e.g. -1) disables the watchdog
	// entirely (env var omitted), matching the zero-omit semantics at the
	// RunConfig level.
	RetryWatchdog int `yaml:"retry_watchdog" json:"retryWatchdog"`
	// FallbackModel, when set, passes --fallback-model to claude for headless
	// runs. Paired with RetryWatchdog so the watchdog can retry with a less
	// loaded model when the primary is overloaded.
	FallbackModel string `yaml:"fallback_model" json:"fallbackModel"`
	// MaxLogEvents caps how many NDJSON events are returned when replaying
	// a completed agent's log file. 0 means use DefaultMaxLogEvents (500).
	MaxLogEvents int `yaml:"max_log_events" json:"maxLogEvents"`
	// LogRetentionDays bounds how long per-agent NDJSON log files live
	// under ~/.sybra/logs/agents/. Files older than this (plus all 0-byte
	// files, regardless of age) are swept on app startup and daily
	// thereafter. 0 falls back to DefaultLogRetentionDays (14).
	LogRetentionDays int `yaml:"log_retention_days" json:"logRetentionDays"`
	// SurviveRestart keeps agent subprocesses running across an app
	// restart (detached, output streamed to their log files) and reattaches
	// to them on the next startup. nil means not configured (defaults to
	// true). Set false to revert to the legacy behaviour where agents are
	// killed on shutdown and recovered via restart-stale.
	SurviveRestart *bool `yaml:"survive_restart" json:"surviveRestart"`
	// ApprovalPort pins the localhost port of the PreToolUse approval
	// server. The hook URL is baked into a permission-gated agent's
	// --settings at spawn, so a fixed port lets a detached agent's approval
	// requests still resolve after a restart. 0 (default) binds a random
	// port (no cross-restart approval survival).
	ApprovalPort int `yaml:"approval_port" json:"approvalPort"`
	// HeadlessPermissionMode sets the default permission posture for unattended
	// headless claude runs. "bypass" (default) keeps the current
	// --dangerously-skip-permissions behavior. "auto" emits --permission-mode auto
	// which activates the Claude Code auto-mode classifier (blocks destructive ops
	// such as rm -rf $HOME, force-push, terraform destroy). Empty treated as "bypass".
	HeadlessPermissionMode string `yaml:"headless_permission_mode" json:"headlessPermissionMode"`
	// DispatchJitterMs bounds a uniform random delay applied before headless
	// agent dispatch, so a wave of concurrently ready tasks does not all
	// probe the provider health gate in the same tick. 0 disables jitter.
	// Never applied to interactive/chat dispatch. Default 1000 — set 0 to
	// disable.
	DispatchJitterMs int `yaml:"dispatch_jitter_ms" json:"dispatchJitterMs"`
	// SandboxMode sets the default OS-level process-sandbox posture for agent
	// subprocesses (darwin: sandbox-exec seatbelt). "off" spawns unwrapped
	// with no validation. "report" (default) validates and logs the
	// resolved write allowlist (worktree/sandbox-home/tmp) without ever
	// wrapping the spawn, so a profile/wrapper defect can only affect an
	// explicit "enforce" posture, never the default rollout posture.
	// "enforce" actually wraps the spawn and blocks writes outside that
	// allowlist, failing the spawn closed if the wrapper is unavailable.
	// Empty treated as "report".
	SandboxMode string `yaml:"sandbox_mode" json:"sandboxMode"`
	// DefaultProjectID pins the project a project-less task auto-assigns to
	// when it needs an isolated worktree (e.g. a meta/self-referential task
	// routed to the plan step). Without it, auto-assignment only fires when
	// exactly one project is registered — on a machine with two or more
	// projects, a project-less task can never dispatch and always ends up
	// human-required. Empty means no default (falls back to the
	// sole-project behavior).
	DefaultProjectID string `yaml:"default_project_id" json:"defaultProjectId"`
	// PlaywrightMCP configures the default-off headless Playwright MCP server
	// attached to test-runner runs that resolve to the Claude provider.
	PlaywrightMCP PlaywrightMCPConfig `yaml:"playwright_mcp" json:"playwrightMcp"`
}

// PlaywrightMCPConfig opts test-runner runs into a headless Playwright MCP
// server for visual/console verification. Default-off: Manager.prepareRunConfig
// only attaches it for headless test-runner runs that resolve to the Claude
// provider and pass a launcher preflight (see internal/agent/mcp.go).
type PlaywrightMCPConfig struct {
	// Enabled opts this machine into attaching the Playwright MCP server.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ExtraArgs are appended verbatim to the `npx -y @playwright/mcp@latest
	// --headless --output-dir <dir>` launch command.
	ExtraArgs []string `yaml:"extra_args" json:"extraArgs"`
}
