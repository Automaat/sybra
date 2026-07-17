package config

type AgentDefaults struct {
	Provider           string  `yaml:"provider" json:"provider"`
	Model              string  `yaml:"model" json:"model"`
	Mode               string  `yaml:"mode" json:"mode"`
	MaxConcurrent      int     `yaml:"max_concurrent" json:"maxConcurrent"`
	ResearchMachineDir string  `yaml:"research_machine_dir" json:"researchMachineDir"`
	MaxCostUSD         float64 `yaml:"max_cost_usd" json:"maxCostUsd"`
	MaxTurns           int     `yaml:"max_turns" json:"maxTurns"`
	// MaxCheckpoints bounds how many times a single workflow step may
	// checkpoint-and-handoff after hitting the per-run turn ceiling. 0 means
	// use DefaultMaxCheckpoints (3).
	MaxCheckpoints int `yaml:"max_checkpoints" json:"maxCheckpoints"`
	// CheckpointOnTurnCeiling swaps the legacy raise-MaxTurns auto-continue for
	// a checkpoint-and-handoff to a fresh run when an eligible code-author
	// headless run hits its per-run turn ceiling. nil means not configured
	// (defaults to true). Set false to restore the legacy in-process
	// auto-continue behavior with no code revert.
	CheckpointOnTurnCeiling *bool `yaml:"checkpoint_on_turn_ceiling" json:"checkpointOnTurnCeiling"`
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
	// ReviewUntilClean keeps simple-task-review cycling review→fix→review
	// until the reviewer returns a CLEAN verdict, so the fix agent's diff is
	// never the last word. nil means not configured (falls back to true).
	// The cycle is uncapped by design — a round cap would censor the
	// review-rounds distribution the stats page reports — and is bounded only
	// by MaxTaskCostUSD, which is enforced before every dispatch. false falls
	// back to a single review pass per task: cheaper and more predictable when
	// no per-task budget is configured.
	ReviewUntilClean *bool `yaml:"review_until_clean" json:"reviewUntilClean"`
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
	// thereafter. 0 falls back to DefaultLogRetentionDays (14). A negative
	// value disables age-based deletion (0-byte files are still removed).
	LogRetentionDays int `yaml:"log_retention_days" json:"logRetentionDays"`
	// LogGzipAfterDays bounds how long a retained per-agent NDJSON log
	// stays uncompressed before the retention sweep gzips it in place
	// (original removed once the .gz sibling is written successfully). 0
	// falls back to DefaultLogGzipAfterDays (3). A negative value disables
	// compression entirely.
	LogGzipAfterDays int `yaml:"log_gzip_after_days" json:"logGzipAfterDays"`
	// LogRetentionMaxSizeMB caps the total size (in MB) of the per-agent
	// NDJSON log directory (~/.sybra/logs/agents/, including any
	// gzip-compressed and .stderr sidecar files). When the age/gzip passes
	// still leave the directory over this cap, the sweep deletes the
	// oldest non-active files (by mtime) until it's back under the cap, or
	// only currently-active agents' logs remain. 0 falls back to
	// DefaultLogRetentionMaxSizeMB (1024, i.e. 1 GiB). A negative value
	// disables size-based enforcement entirely.
	LogRetentionMaxSizeMB int `yaml:"log_retention_max_size_mb" json:"logRetentionMaxSizeMb"`
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
	// subprocesses (darwin: sandbox-exec seatbelt, linux: bwrap). "off"
	// spawns unwrapped with no validation. "report" (default) validates and
	// logs the resolved write allowlist (worktree/sandbox-home/tmp plus
	// task-scoped git metadata) without ever wrapping the spawn, so a
	// profile/wrapper defect can only affect an explicit "enforce" posture,
	// never the default rollout posture. "enforce" actually wraps the spawn
	// and blocks writes outside that allowlist, failing the spawn closed if
	// the wrapper is unavailable.
	// Empty treated as "report".
	SandboxMode string `yaml:"sandbox_mode" json:"sandboxMode"`
	// HeadlessSteerable controls whether headless claude runs launch with the
	// stdin/stream-json shape that accepts mid-run steer messages (instead of
	// the legacy one-shot `-p <prompt>` invocation). nil means not configured
	// (defaults to true). Set false to restore the legacy launch shape with
	// no stdin transport — a config-only rollback with no code revert.
	HeadlessSteerable *bool `yaml:"headless_steerable" json:"headlessSteerable"`
	// DefaultProjectID pins the project a project-less task auto-assigns to
	// when it needs an isolated worktree (e.g. a meta/self-referential task
	// routed to the plan step). Without it, auto-assignment only fires when
	// exactly one project is registered — on a machine with two or more
	// projects, a project-less task can never dispatch and always ends up
	// human-required. Empty means no default (falls back to the
	// sole-project behavior).
	DefaultProjectID string `yaml:"default_project_id" json:"defaultProjectId"`
	// RoleEffort overrides the built-in per-role reasoning-effort baseline
	// (see agent.Role.DefaultReasoningEffort), keyed by role name (e.g.
	// "triage", "implementation"). Still loses to an experiment assignment's
	// or the task's own ReasoningEffort — this only replaces the role
	// fallback, not an explicit per-task/per-run override. Unknown role keys
	// or invalid effort values are ignored (falls back to the built-in
	// default for that role).
	RoleEffort map[string]string `yaml:"role_effort" json:"roleEffort"`
	// PlaywrightMCP configures the default-off headless Playwright MCP server
	// attached to test-runner runs that resolve to the Claude provider.
	PlaywrightMCP PlaywrightMCPConfig `yaml:"playwright_mcp" json:"playwrightMcp"`
	// K8sJobs configures an experimental backend that runs headless agents as
	// short-lived Kubernetes Jobs instead of local subprocesses.
	K8sJobs K8sJobsConfig `yaml:"k8s_jobs" json:"k8sJobs"`
	// Queue configures the agent-dispatch admission queue (internal/agentqueue)
	// that a workflow implementation dispatch falls back to when the agent
	// pool is saturated, instead of erroring or wasting a worktree prep.
	Queue QueueConfig `yaml:"queue" json:"queue"`
}

// QueueConfig configures the agent-dispatch admission queue.
type QueueConfig struct {
	// MaxDepth caps the number of distinct tasks the admission queue holds at
	// once (agentqueue.Options.MaxDepth). 0 means unbounded. Once full, a new
	// task that can't get a pool slot is rejected with a normal dispatch
	// error instead of being queued.
	MaxDepth int `yaml:"max_depth" json:"maxDepth"`
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

// K8sJobsConfig is a PoC execution backend for headless-only Sybra. When
// enabled, future headless agents are run as Kubernetes Jobs using the
// in-cluster service account.
type K8sJobsConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	Namespace string   `yaml:"namespace,omitempty" json:"namespace"`
	Image     string   `yaml:"image,omitempty" json:"image"`
	Command   []string `yaml:"command,omitempty" json:"command"`
	TTL       int      `yaml:"ttl_seconds_after_finished,omitempty" json:"ttlSecondsAfterFinished"`
	Mode      string   `yaml:"mode,omitempty" json:"mode"`
	// CreatePR lets the agent Job open its own pull request once it has pushed
	// its branch, instead of the server shelling gh in the task worktree. Only
	// fires when the task's remote is a GitHub URL — a PVC-backed bare clone
	// has no PR to open. Default false: the server-side create_pr workflow step
	// still owns the normal path, and this would otherwise open a PR after
	// every agent run rather than at the pr stage.
	CreatePR  bool                 `yaml:"create_pr,omitempty" json:"createPr"`
	Env       []K8sJobEnvVar       `yaml:"env,omitempty" json:"env"`
	SecretEnv []K8sJobSecretEnvVar `yaml:"secret_env,omitempty" json:"secretEnv"`
	Volumes   []K8sJobVolume       `yaml:"volumes,omitempty" json:"volumes"`
}

type K8sJobEnvVar struct {
	Name  string `yaml:"name" json:"name"`
	Value string `yaml:"value" json:"value"`
}

type K8sJobSecretEnvVar struct {
	Name       string `yaml:"name" json:"name"`
	SecretName string `yaml:"secret_name" json:"secretName"`
	SecretKey  string `yaml:"secret_key" json:"secretKey"`
}

type K8sJobVolume struct {
	Name      string `yaml:"name" json:"name"`
	ClaimName string `yaml:"claim_name" json:"claimName"`
	MountPath string `yaml:"mount_path" json:"mountPath"`
	ReadOnly  bool   `yaml:"read_only,omitempty" json:"readOnly,omitempty"`
}
