package config

import "github.com/Automaat/sybra/internal/abtest"

type Config struct {
	Logging       LoggingConfig       `yaml:"logging" json:"logging"`
	Audit         AuditConfig         `yaml:"audit" json:"audit"`
	Agent         AgentDefaults       `yaml:"agent" json:"agent"`
	Testing       TestingConfig       `yaml:"testing" json:"testing"`
	Notification  NotificationConfig  `yaml:"notification" json:"notification"`
	Orchestrator  OrchestratorConfig  `yaml:"orchestrator" json:"orchestrator"`
	Todoist       TodoistConfig       `yaml:"todoist" json:"todoist"`
	Renovate      RenovateConfig      `yaml:"renovate" json:"renovate"`
	GitHub        GitHubConfig        `yaml:"github" json:"github"`
	Umbrella      UmbrellaConfig      `yaml:"umbrella" json:"umbrella"`
	Triage        TriageConfig        `yaml:"triage" json:"triage"`
	HumanReview   HumanReviewConfig   `yaml:"human_review" json:"humanReview"`
	Monitor       MonitorConfig       `yaml:"monitor" json:"monitor"`
	Watchdog      WatchdogConfig      `yaml:"watchdog" json:"watchdog"`
	SelfMonitor   SelfMonitorConfig   `yaml:"self_monitor" json:"selfMonitor"`
	Evaluation    EvaluationConfig    `yaml:"evaluation" json:"evaluation"`
	HarnessEvolve HarnessEvolveConfig `yaml:"harness_evolution" json:"harnessEvolution"`
	Experience    ExperienceConfig    `yaml:"experience" json:"experience"`
	ABTesting     abtest.Config       `yaml:"ab_testing" json:"abTesting"`
	Providers     ProvidersConfig     `yaml:"providers" json:"providers"`
	Metrics       MetricsConfig       `yaml:"metrics" json:"metrics"`
	AutoUpdate    AutoUpdateConfig    `yaml:"auto_update" json:"autoUpdate"`
	ProjectTypes  []string            `yaml:"project_types" json:"projectTypes"`
	TasksDir      string              `yaml:"tasks_dir" json:"tasksDir"`
	SkillsDir     string              `yaml:"skills_dir" json:"skillsDir"`
	RepoDir       string              `yaml:"repo_dir" json:"repoDir"`
	ProjectsDir   string              `yaml:"projects_dir" json:"projectsDir"`
	ClonesDir     string              `yaml:"clones_dir" json:"clonesDir"`
	WorktreesDir  string              `yaml:"worktrees_dir" json:"worktreesDir"`
	LoopAgentsDir string              `yaml:"loop_agents_dir" json:"loopAgentsDir"`
}

type AuditConfig struct {
	Enabled       bool `yaml:"enabled" json:"enabled"`
	RetentionDays int  `yaml:"retention_days" json:"retentionDays"`
}

type LoggingConfig struct {
	Level     string `yaml:"level" json:"level"`
	Dir       string `yaml:"dir" json:"dir"`
	MaxSizeMB int    `yaml:"max_size_mb" json:"maxSizeMB"`
	MaxFiles  int    `yaml:"max_files" json:"maxFiles"`
}

type AgentDefaults struct {
	Provider           string  `yaml:"provider" json:"provider"`
	Model              string  `yaml:"model" json:"model"`
	Mode               string  `yaml:"mode" json:"mode"`
	MaxConcurrent      int     `yaml:"max_concurrent" json:"maxConcurrent"`
	ResearchMachineDir string  `yaml:"research_machine_dir" json:"researchMachineDir"`
	MaxCostUSD         float64 `yaml:"max_cost_usd" json:"maxCostUsd"`
	MaxTurns           int     `yaml:"max_turns" json:"maxTurns"`
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
}

// TestingConfig controls the autonomous manual-testing phase. A task entering
// status=testing spawns a single adversarial test-runner agent that starts the
// real app/cluster in an isolated per-task sandbox and tries to prove the
// implementation does not satisfy the task. Each test-runner holds its own
// sandbox (Docker compose project / k3d cluster), so MaxConcurrent bounds
// real-app/cluster load independently of Agent.MaxConcurrent.
type TestingConfig struct {
	// MaxConcurrent caps simultaneously-running test-runner agents on this
	// machine. 0 falls back to DefaultTestingMaxConcurrent.
	MaxConcurrent int `yaml:"max_concurrent" json:"maxConcurrent"`
	// MaxAttempts caps how many times a task may fail testing and bounce back
	// to in-progress for re-implementation before it is escalated to
	// human-required instead. 0 falls back to DefaultTestingMaxAttempts.
	MaxAttempts int `yaml:"max_attempts" json:"maxAttempts"`
}

type NotificationConfig struct {
	Desktop bool `yaml:"desktop" json:"desktop"`
}

type ExperienceConfig struct {
	Enabled    bool `yaml:"enabled" json:"enabled"`
	MaxRecords int  `yaml:"max_records" json:"maxRecords"`
}

type OrchestratorConfig struct {
	AutoTriage bool `yaml:"auto_triage" json:"autoTriage"`
	AutoPlan   bool `yaml:"auto_plan" json:"autoPlan"`
	// DispatchIntervalSeconds is the cadence of the cheap, latency-sensitive
	// dispatch pass (start the orchestrator, release unblocked children). Kept
	// short — and also fired on demand on every status change — so a
	// freshly-ready task is not left idle for a full tick. Default 10.
	DispatchIntervalSeconds int `yaml:"dispatch_interval_seconds" json:"dispatchIntervalSeconds"`
	// MaintenanceIntervalSeconds is the cadence of the expensive recovery/cleanup
	// pass (resume stalled workflows, restart stale agents, prune orphan
	// worktrees) which hits git and may spawn agents, so it must not run hot.
	// Default 60.
	MaintenanceIntervalSeconds int `yaml:"maintenance_interval_seconds" json:"maintenanceIntervalSeconds"`
}

type TodoistConfig struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	APIToken         string `yaml:"api_token" json:"apiToken"`
	ProjectID        string `yaml:"project_id" json:"projectId"`
	DefaultProjectID string `yaml:"default_project_id" json:"defaultProjectId"`
	PollSeconds      int    `yaml:"poll_seconds" json:"pollSeconds"`
}

type RenovateConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Author  string `yaml:"author" json:"author"`
}

type GitHubConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// PollerRole splits GitHub search polling (reviews/issues/renovate) across
	// machines sharing one token. "primary" (or empty) runs the search pollers;
	// "secondary" skips them so a sibling instance owns the searches and the
	// shared token isn't billed twice. On-demand per-PR/issue calls still run on
	// every machine — only the periodic searches are gated.
	PollerRole string `yaml:"poller_role" json:"pollerRole"`
	// Poll-interval overrides in seconds. Zero falls back to the built-in
	// default. Raised defaults (vs. the original 1m/5m) cut steady-state request
	// volume; lower them only on a high-limit (App-token) instance.
	ReviewsFastSeconds  int `yaml:"reviews_fast_seconds" json:"reviewsFastSeconds"`
	ReviewsSlowSeconds  int `yaml:"reviews_slow_seconds" json:"reviewsSlowSeconds"`
	IssuesSeconds       int `yaml:"issues_seconds" json:"issuesSeconds"`
	RenovateFastSeconds int `yaml:"renovate_fast_seconds" json:"renovateFastSeconds"`
	RenovateSlowSeconds int `yaml:"renovate_slow_seconds" json:"renovateSlowSeconds"`
	// App configures GitHub App installation-token auth. When enabled, Sybra
	// mints a short-lived installation token and injects it into the gh
	// subprocess (GH_TOKEN), raising the REST ceiling to 15k/hr. Unset = fall
	// back to gh's own auth.
	App GitHubAppConfig `yaml:"app" json:"app"`
}

// GitHubAppConfig holds GitHub App installation-token credentials. The private
// key never leaves disk as plaintext config — only its path is stored.
type GitHubAppConfig struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	AppID          int64  `yaml:"app_id" json:"appId"`
	InstallationID int64  `yaml:"installation_id" json:"installationId"`
	PrivateKeyPath string `yaml:"private_key_path" json:"privateKeyPath"`
}

// UmbrellaConfig governs auto-expansion of ☂️ umbrella issues by the GitHub
// issue fetcher. Disabled by default; project-scoped via the top-level
// project_types allowlist so only one machine expands a given umbrella.
type UmbrellaConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Model overrides the planner model (empty = claude default).
	Model string `yaml:"model" json:"model"`
}

// TriageConfig controls the background auto-triage worker. When Enabled,
// sybra periodically classifies tasks in status=new via claude -p and
// atomically applies the verdict (title, tags, size/type, mode, project).
type TriageConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	PollSeconds int    `yaml:"poll_seconds" json:"pollSeconds"`
	Model       string `yaml:"model" json:"model"`
}

// HumanReviewConfig controls the in-process automation that spawns a
// headless review agent every time a task transitions to human-required.
// The agent inspects the task, its agent runs, recent logs and the Sybra
// source tree, decides whether the transition is genuine or a Sybra bug,
// and (on bug) files a deduplicated GitHub issue + flips the task to
// blocked. Per-machine toggle: enable on the laptop with the source
// checkout, leave disabled on the server.
type HumanReviewConfig struct {
	Enabled      bool   `yaml:"enabled" json:"enabled"`
	SybraRepoDir string `yaml:"sybra_repo_dir" json:"sybraRepoDir"`
	// Repo is the owner/name where bug issues are filed. Defaults to
	// "Automaat/sybra" when empty.
	Repo string `yaml:"repo" json:"repo"`
	// Model is the Claude model alias (e.g. "sonnet", "opus"). Defaults
	// to "sonnet" when empty.
	Model string `yaml:"model" json:"model"`
	// MaxPerHour caps how many review agents may be spawned in any rolling
	// 60-minute window across all tasks on this machine. Zero falls back
	// to DefaultHumanReviewMaxPerHour.
	MaxPerHour int `yaml:"max_per_hour" json:"maxPerHour"`
	// IssueLabel is the label applied to filed issues (in addition to
	// "bug"). Defaults to "sybra-bug".
	IssueLabel string `yaml:"issue_label" json:"issueLabel"`
	// SybraBugAction controls the side-effect for sybra_bug verdicts:
	// file_issue (default), local_task, block_only, or note_only.
	SybraBugAction string `yaml:"sybra_bug_action" json:"sybraBugAction"`
}

// SelfMonitorConfig controls the in-process selfmonitor service that
// replaces the /loop 6h /sybra-self-monitor skill. Each tick snapshots
// the latest health report, distills per-finding agent logs into a
// LogSummary, runs a two-stage LLM judge + synthesizer (Phase C), files
// deduped issues via the shared monitor.IssueSink, and autonomously
// remediates a whitelisted set of categories (Phase D). Enabled stays
// false until users opt in.
type SelfMonitorConfig struct {
	Enabled              bool     `yaml:"enabled" json:"enabled"`
	IntervalHours        float64  `yaml:"interval_hours" json:"intervalHours"`
	JudgeModel           string   `yaml:"judge_model" json:"judgeModel"`
	SynthesizerModel     string   `yaml:"synthesizer_model" json:"synthesizerModel"`
	MaxIssuesPerRun      int      `yaml:"max_issues_per_run" json:"maxIssuesPerRun"`
	MaxAutoActionsPerDay int      `yaml:"max_auto_actions_per_day" json:"maxAutoActionsPerDay"`
	AutoActCategories    []string `yaml:"auto_act_categories" json:"autoActCategories"`
	DryRun               bool     `yaml:"dry_run" json:"dryRun"`
	IssueCooldownHours   float64  `yaml:"issue_cooldown_hours" json:"issueCooldownHours"`
	IssueLabel           string   `yaml:"issue_label" json:"issueLabel"`
	MaxCostPerTickUSD    float64  `yaml:"max_cost_per_tick_usd" json:"maxCostPerTickUsd"`
	JudgeParallelism     int      `yaml:"judge_parallelism" json:"judgeParallelism"`
	SuppressionDays      int      `yaml:"suppression_days" json:"suppressionDays"`
	SuppressionThreshold int      `yaml:"suppression_threshold" json:"suppressionThreshold"`
}

// MonitorConfig controls the in-process monitor service that replaces the
// /loop 5m /sybra-monitor skill. Each tick snapshots the board + audit
// window, detects anomalies (lost agents, PR gaps, dwell, failure spikes,
// bottlenecks), runs idempotent remediations directly, and dispatches a
// focused headless agent for anomalies that need LLM judgment.
type MonitorConfig struct {
	Enabled              bool               `yaml:"enabled" json:"enabled"`
	IntervalSeconds      int                `yaml:"interval_seconds" json:"intervalSeconds"`
	Model                string             `yaml:"model" json:"model"`
	IssueCooldownMinutes int                `yaml:"issue_cooldown_minutes" json:"issueCooldownMinutes"`
	DispatchLimit        int                `yaml:"dispatch_limit" json:"dispatchLimit"`
	StuckHumanHours      float64            `yaml:"stuck_human_hours" json:"stuckHumanHours"`
	LostAgentMinutes     int                `yaml:"lost_agent_minutes" json:"lostAgentMinutes"`
	PRGapGraceMinutes    int                `yaml:"pr_gap_grace_minutes" json:"prGapGraceMinutes"`
	FailureRateThreshold float64            `yaml:"failure_rate_threshold" json:"failureRateThreshold"`
	BottleneckHours      map[string]float64 `yaml:"bottleneck_hours" json:"bottleneckHours"`
	IssueLabel           string             `yaml:"issue_label" json:"issueLabel"`
	IssueRepo            string             `yaml:"issue_repo" json:"issueRepo"`
}

// WatchdogConfig controls the in-process agent watchdog (internal/watchdog),
// which supervises running headless agents: it triggers a cheap LLM inspection
// when an agent stalls, overruns its size budget, or loops on the same tool
// call (real-time loop detection), then stops/escalates/nudges based on the
// verdict. Enabled defaults to true — the watchdog is an always-on safety net,
// not an opt-in automation. Model selects the cheap judge model; LoopThreshold
// is the number of consecutive identical tool-call signatures that flags a loop.
type WatchdogConfig struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	Model         string `yaml:"model" json:"model"`
	LoopThreshold int    `yaml:"loop_threshold" json:"loopThreshold"`
}

// EvaluationConfig controls the in-process evaluation service, which periodically
// computes a fleet scorecard (autonomy, throughput, reliability, efficiency) from
// stats + audit data. Read-only: it never dispatches agents or files issues, so
// it needs no project-type routing — each machine scores its own local data.
type EvaluationConfig struct {
	Enabled       bool    `yaml:"enabled" json:"enabled"`
	IntervalHours float64 `yaml:"interval_hours" json:"intervalHours"`
	WindowDays    int     `yaml:"window_days" json:"windowDays"`
}

// HarnessEvolveConfig controls the governed harness-evolution proposal loop.
// The loop proposes reviewable tasks/issues from telemetry; it never applies
// prompt, workflow, permission, retry, validator, or deployment changes itself.
type HarnessEvolveConfig struct {
	Enabled        bool    `yaml:"enabled" json:"enabled"`
	IntervalHours  float64 `yaml:"interval_hours" json:"intervalHours"`
	LookbackHours  float64 `yaml:"lookback_hours" json:"lookbackHours"`
	MinClusterSize int     `yaml:"min_cluster_size" json:"minClusterSize"`
	Sink           string  `yaml:"sink" json:"sink"`
}

// ProvidersConfig groups per-machine routing for CLI providers (claude, codex,
// copilot) and their background health-check loop. A missing block defaults to
// "all providers enabled, health check on, auto-failover on, 300s interval".
type ProvidersConfig struct {
	HealthCheck  ProviderHealthCheckConfig `yaml:"health_check" json:"healthCheck"`
	Claude       ProviderEntryConfig       `yaml:"claude" json:"claude"`
	Codex        ProviderEntryConfig       `yaml:"codex" json:"codex"`
	Copilot      ProviderEntryConfig       `yaml:"copilot" json:"copilot"`
	Limits       ProviderLimitsConfig      `yaml:"limits" json:"limits"`
	AutoFailover bool                      `yaml:"auto_failover" json:"autoFailover"`
}

type ProviderHealthCheckConfig struct {
	Enabled         bool `yaml:"enabled" json:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds" json:"intervalSeconds"`
}

type ProviderEntryConfig struct {
	Enabled                  bool `yaml:"enabled" json:"enabled"`
	RateLimitCooldownSeconds int  `yaml:"rate_limit_cooldown_seconds" json:"rateLimitCooldownSeconds"`
	// MonthlySubscriptionUSD is optional and used only for Stats value
	// comparison. Zero means "not configured".
	MonthlySubscriptionUSD float64 `yaml:"monthly_subscription_usd" json:"monthlySubscriptionUsd"`
}

type ProviderLimitsConfig struct {
	Enabled                 bool    `yaml:"enabled" json:"enabled"`
	SessionThresholdPercent float64 `yaml:"session_threshold_percent" json:"sessionThresholdPercent"`
	WeeklyThresholdPercent  float64 `yaml:"weekly_threshold_percent" json:"weeklyThresholdPercent"`
	PreferUnderused         bool    `yaml:"prefer_underused" json:"preferUnderused"`
	BackfillDays            int     `yaml:"backfill_days" json:"backfillDays"`
}

// MetricsConfig controls the OpenTelemetry metrics pipeline. When Enabled is
// true, sybra-server mounts /metrics on its existing mux and emits
// Prometheus-format output for external scrapers.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// AutoUpdateConfig controls source update checks. In "auto" mode a clean
// fast-forward update is applied and Sybra requests a supervisor restart.
type AutoUpdateConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	RepoDir     string `yaml:"repo_dir" json:"repoDir"`
	Remote      string `yaml:"remote" json:"remote"`
	Branch      string `yaml:"branch" json:"branch"`
	Mode        string `yaml:"mode" json:"mode"`
	PollSeconds int    `yaml:"poll_seconds" json:"pollSeconds"`
	// Deprecated: ignored. Kept so existing config files continue to load.
	RestartDelaySeconds int `yaml:"restart_delay_seconds" json:"restartDelaySeconds"`
}
