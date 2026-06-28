package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Automaat/sybra/internal/abtest"
	"gopkg.in/yaml.v3"
)

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

// AllowsProjectType reports whether automations on this machine should act on
// projects of the given type. An empty ProjectTypes list means "all types".
func (c *Config) AllowsProjectType(t string) bool {
	if c == nil || len(c.ProjectTypes) == 0 {
		return true
	}
	return slices.Contains(c.ProjectTypes, t)
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

// DefaultBashTimeoutSeconds is the per-bash-tool-call timeout used when
// BashTimeoutSeconds is not set in config.
const DefaultBashTimeoutSeconds = 300

// DefaultRetryWatchdog is the CLAUDE_CODE_RETRY_WATCHDOG value used when
// RetryWatchdog is not set in config. Exceeds the old CLAUDE_CODE_MAX_RETRIES
// cap of 15, appropriate for unattended server runs.
const DefaultRetryWatchdog = 30

// BashTimeoutMs returns the bash tool timeout in milliseconds, exported into
// the claude subprocess as BASH_DEFAULT_TIMEOUT_MS / BASH_MAX_TIMEOUT_MS.
func (c *Config) BashTimeoutMs() int {
	if c != nil && c.Agent.BashTimeoutSeconds > 0 {
		return c.Agent.BashTimeoutSeconds * 1000
	}
	return DefaultBashTimeoutSeconds * 1000
}

// RetryWatchdog returns the configured watchdog value, DefaultRetryWatchdog
// when unset (0), or 0 when explicitly disabled (negative value).
func (c *Config) RetryWatchdog() int {
	if c != nil && c.Agent.RetryWatchdog < 0 {
		return 0
	}
	if c != nil && c.Agent.RetryWatchdog > 0 {
		return c.Agent.RetryWatchdog
	}
	return DefaultRetryWatchdog
}

// DefaultMaxLogEvents returns the configured cap or 500 if unset.
func (c *Config) DefaultMaxLogEvents() int {
	if c != nil && c.Agent.MaxLogEvents > 0 {
		return c.Agent.MaxLogEvents
	}
	return 500
}

// DefaultLogRetentionDays returns the configured retention window for
// per-agent NDJSON logs, or 14 days if unset. A negative value disables
// age-based pruning (0-byte files are still removed).
func (c *Config) DefaultLogRetentionDays() int {
	if c == nil {
		return 14
	}
	if c.Agent.LogRetentionDays < 0 {
		return c.Agent.LogRetentionDays
	}
	if c.Agent.LogRetentionDays == 0 {
		return 14
	}
	return c.Agent.LogRetentionDays
}

// DefaultRequirePermissions returns the configured default, or true if unset.
func (c *Config) DefaultRequirePermissions() bool {
	if c != nil && c.Agent.RequirePermissions != nil {
		return *c.Agent.RequirePermissions
	}
	return true
}

// NormalizeHeadlessPermissionMode canonicalizes a headless permission mode value.
// Empty string maps to "bypass". "bypass" and "auto" pass through unchanged.
// Any other value is rejected with an error.
func NormalizeHeadlessPermissionMode(s string) (string, error) {
	// Trim + lowercase first: a formatting slip (`auto `, `Auto`) must not
	// silently fall through to the permissive bypass default and disable the
	// guardrail this feature exists to provide.
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "bypass":
		return "bypass", nil
	case "auto":
		return "auto", nil
	default:
		return "", fmt.Errorf("invalid headless_permission_mode %q (valid: bypass, auto)", s)
	}
}

// DefaultHeadlessPermissionMode returns the configured default headless permission
// mode, or "bypass" if unset. An invalid config value is logged and treated as
// "bypass" so a misconfigured server never silently switches posture.
func (c *Config) DefaultHeadlessPermissionMode() string {
	if c == nil || c.Agent.HeadlessPermissionMode == "" {
		return "bypass"
	}
	mode, err := NormalizeHeadlessPermissionMode(c.Agent.HeadlessPermissionMode)
	if err != nil {
		slog.Warn("config: invalid agent.headless_permission_mode; falling back to bypass", "value", c.Agent.HeadlessPermissionMode)
		return "bypass"
	}
	return mode
}

// SurviveRestartEnabled reports whether agent subprocesses should be
// detached to survive an app restart and reattached on the next startup.
// Defaults to true when unset.
func (c *Config) SurviveRestartEnabled() bool {
	if c != nil && c.Agent.SurviveRestart != nil {
		return *c.Agent.SurviveRestart
	}
	return true
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

// DefaultTestingMaxConcurrent bounds concurrent test-runner agents (each owns
// an isolated sandbox) when TestingConfig.MaxConcurrent is unset.
const DefaultTestingMaxConcurrent = 3

// DefaultTestingMaxAttempts bounds the testing → in-progress re-implementation
// loop when TestingConfig.MaxAttempts is unset.
const DefaultTestingMaxAttempts = 3

// TestingMaxConcurrent returns the configured cap or DefaultTestingMaxConcurrent.
func (c *Config) TestingMaxConcurrent() int {
	if c != nil && c.Testing.MaxConcurrent > 0 {
		return c.Testing.MaxConcurrent
	}
	return DefaultTestingMaxConcurrent
}

// TestingMaxAttempts returns the configured cap or DefaultTestingMaxAttempts.
func (c *Config) TestingMaxAttempts() int {
	if c != nil && c.Testing.MaxAttempts > 0 {
		return c.Testing.MaxAttempts
	}
	return DefaultTestingMaxAttempts
}

type NotificationConfig struct {
	Desktop bool `yaml:"desktop" json:"desktop"`
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

// DefaultHumanReviewMaxPerHour is the fallback rate-limit cap used when
// HumanReviewConfig.MaxPerHour is zero.
const DefaultHumanReviewMaxPerHour = 6

const (
	HumanReviewSybraBugActionFileIssue = "file_issue"
	HumanReviewSybraBugActionLocalTask = "local_task"
	HumanReviewSybraBugActionBlockOnly = "block_only"
	HumanReviewSybraBugActionNoteOnly  = "note_only"
)

// HumanReviewMaxPerHour returns the configured cap or the package default.
func (c *Config) HumanReviewMaxPerHour() int {
	if c != nil && c.HumanReview.MaxPerHour > 0 {
		return c.HumanReview.MaxPerHour
	}
	return DefaultHumanReviewMaxPerHour
}

// HumanReviewRepo returns the configured target repo or "Automaat/sybra".
func (c *Config) HumanReviewRepo() string {
	if c != nil && c.HumanReview.Repo != "" {
		return c.HumanReview.Repo
	}
	return "Automaat/sybra"
}

// HumanReviewModel returns the configured model alias or "sonnet".
func (c *Config) HumanReviewModel() string {
	if c != nil && c.HumanReview.Model != "" {
		return c.HumanReview.Model
	}
	return "sonnet"
}

// HumanReviewIssueLabel returns the configured label or "sybra-bug".
func (c *Config) HumanReviewIssueLabel() string {
	if c != nil && c.HumanReview.IssueLabel != "" {
		return c.HumanReview.IssueLabel
	}
	return "sybra-bug"
}

func (c *Config) HumanReviewSybraBugAction() string {
	if c == nil {
		return HumanReviewSybraBugActionFileIssue
	}
	switch strings.ToLower(strings.TrimSpace(c.HumanReview.SybraBugAction)) {
	case "", HumanReviewSybraBugActionFileIssue:
		return HumanReviewSybraBugActionFileIssue
	case HumanReviewSybraBugActionLocalTask:
		return HumanReviewSybraBugActionLocalTask
	case HumanReviewSybraBugActionBlockOnly:
		return HumanReviewSybraBugActionBlockOnly
	case HumanReviewSybraBugActionNoteOnly:
		return HumanReviewSybraBugActionNoteOnly
	default:
		slog.Warn("config: invalid human_review.sybra_bug_action; falling back to file_issue", "value", c.HumanReview.SybraBugAction)
		return HumanReviewSybraBugActionFileIssue
	}
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

func HomeDir() string {
	if dir := os.Getenv("SYBRA_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sybra")
}

func DefaultConfig() *Config {
	return &Config{
		Logging: LoggingConfig{
			Level:     "info",
			Dir:       defaultLogDir(),
			MaxSizeMB: 50,
			MaxFiles:  5,
		},
		Audit: AuditConfig{
			Enabled:       true,
			RetentionDays: 30,
		},
		Agent: AgentDefaults{
			Provider:      "claude",
			MaxConcurrent: 100,
			MaxCostUSD:    5.0,
			MaxTurns:      150,
		},
		Notification: NotificationConfig{
			Desktop: true,
		},
		Renovate: RenovateConfig{
			Enabled: true,
			Author:  "app/renovate",
		},
		GitHub: GitHubConfig{
			Enabled: true,
		},
		Monitor: MonitorConfig{
			Enabled: true,
		},
		HarnessEvolve: HarnessEvolveConfig{
			Enabled: true,
		},
		Watchdog: WatchdogConfig{
			Enabled:       true,
			LoopThreshold: 6,
		},
		ABTesting: abtest.DefaultConfig(),
		AutoUpdate: AutoUpdateConfig{
			Remote:              "origin",
			Branch:              "main",
			Mode:                "notify",
			PollSeconds:         300,
			RestartDelaySeconds: 2,
		},
		Providers: ProvidersConfig{
			HealthCheck: ProviderHealthCheckConfig{
				Enabled:         true,
				IntervalSeconds: 300,
			},
			Claude:  ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Codex:   ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Copilot: ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Limits: ProviderLimitsConfig{
				Enabled:                 true,
				SessionThresholdPercent: 85,
				WeeklyThresholdPercent:  90,
				PreferUnderused:         true,
				BackfillDays:            14,
			},
			AutoFailover: true,
		},
		TasksDir: defaultTasksDir(),
	}
}

func (c *Config) AuditDir() string {
	return filepath.Join(c.Logging.Dir, "audit")
}

// Save writes the current config to disk.
func (c *Config) Save() error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Directories returns the resolved paths for all sybra data directories.
func (c *Config) Directories() map[string]string {
	return map[string]string{
		"tasks":       c.TasksDir,
		"skills":      c.SkillsDir,
		"projects":    c.ProjectsDir,
		"clones":      c.ClonesDir,
		"worktrees":   c.WorktreesDir,
		"logs":        c.Logging.Dir,
		"audit":       c.AuditDir(),
		"loop_agents": c.LoopAgentsDir,
		"artifacts":   ArtifactsDir(),
	}
}

func Load() (*Config, error) {
	cfg := DefaultConfig()

	path := configPath()
	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else if os.IsNotExist(err) {
		if writeErr := writeDefaultConfig(path); writeErr != nil {
			return nil, writeErr
		}
	}

	if v := os.Getenv("SYBRA_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("SYBRA_LOG_DIR"); v != "" {
		cfg.Logging.Dir = v
	}

	if cfg.Logging.Dir == "" {
		cfg.Logging.Dir = defaultLogDir()
	}
	if cfg.TasksDir == "" {
		cfg.TasksDir = defaultTasksDir()
	}
	if v := os.Getenv("SYBRA_TASKS_DIR"); v != "" {
		cfg.TasksDir = v
	}

	if cfg.SkillsDir == "" {
		cfg.SkillsDir = defaultSkillsDir()
	}
	// Migration: previous releases defaulted to ~/.sybra/skills which Claude
	// Code never reads. Silently retarget the old default so users with stale
	// configs get the fix without manual intervention. cdb6dc5 changed the
	// default but did not migrate persisted overrides.
	if cfg.SkillsDir == filepath.Join(HomeDir(), "skills") {
		cfg.SkillsDir = defaultSkillsDir()
	}
	if cfg.ProjectsDir == "" {
		cfg.ProjectsDir = defaultProjectsDir()
	}
	if cfg.ClonesDir == "" {
		cfg.ClonesDir = defaultClonesDir()
	}
	if cfg.WorktreesDir == "" {
		cfg.WorktreesDir = defaultWorktreesDir()
	}
	if cfg.LoopAgentsDir == "" {
		cfg.LoopAgentsDir = defaultLoopAgentsDir()
	}

	if v := os.Getenv("SYBRA_TODOIST_TOKEN"); v != "" {
		cfg.Todoist.APIToken = v
	}
	if cfg.Todoist.PollSeconds <= 0 {
		cfg.Todoist.PollSeconds = 120
	}

	if cfg.Renovate.Author == "" {
		cfg.Renovate.Author = "app/renovate"
	}
	if cfg.Triage.PollSeconds <= 0 {
		cfg.Triage.PollSeconds = 60
	}
	if cfg.Triage.Model == "" {
		cfg.Triage.Model = "sonnet"
	}
	if cfg.Agent.Provider == "" {
		cfg.Agent.Provider = "claude"
	}

	applyProvidersDefaults(cfg)
	applyMonitorDefaults(cfg)
	applyWatchdogDefaults(cfg)
	applySelfMonitorDefaults(cfg)
	applyEvaluationDefaults(cfg)
	applyHarnessEvolveDefaults(cfg)
	applyABTestingDefaults(cfg)
	applyOrchestratorDefaults(cfg)
	applyAutoUpdateDefaults(cfg)

	return cfg, nil
}

func applyAutoUpdateDefaults(cfg *Config) {
	if cfg.AutoUpdate.Remote == "" {
		cfg.AutoUpdate.Remote = "origin"
	}
	if cfg.AutoUpdate.Branch == "" {
		cfg.AutoUpdate.Branch = "main"
	}
	if cfg.AutoUpdate.Mode == "" {
		cfg.AutoUpdate.Mode = "notify"
	}
	if cfg.AutoUpdate.PollSeconds <= 0 {
		cfg.AutoUpdate.PollSeconds = 300
	}
	if cfg.AutoUpdate.RestartDelaySeconds <= 0 {
		cfg.AutoUpdate.RestartDelaySeconds = 2
	}
}

func applyABTestingDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	def := abtest.DefaultConfig()
	if cfg.ABTesting.Enabled == nil {
		cfg.ABTesting.Enabled = def.Enabled
	}
	if cfg.ABTesting.MinSamplesPerVariant <= 0 {
		cfg.ABTesting.MinSamplesPerVariant = def.MinSamplesPerVariant
	}
	if len(cfg.ABTesting.Experiments) == 0 {
		cfg.ABTesting.Experiments = def.Experiments
	}
}

// applyWatchdogDefaults fills the Watchdog model default. Enabled and
// LoopThreshold are seeded by DefaultConfig (true / 6), so a config missing the
// watchdog block keeps the always-on default while an explicit `enabled: false`
// or `loop_threshold: 0` survives — the latter being the documented way to keep
// the watchdog running with the real-time loop trigger off. LoopThreshold is
// deliberately NOT defaulted here so an explicit 0 is not clobbered back to 6.
func applyWatchdogDefaults(cfg *Config) {
	w := &cfg.Watchdog
	if w.Model == "" {
		w.Model = "claude-haiku-4-5-20251001"
	}
}

// applyEvaluationDefaults fills zero values for the Evaluation block so older
// configs behave deterministically. Enabled stays false until operators opt in.
func applyEvaluationDefaults(cfg *Config) {
	e := &cfg.Evaluation
	if e.IntervalHours < 1 {
		e.IntervalHours = 24
	}
	if e.WindowDays <= 0 {
		e.WindowDays = 30
	}
}

// applyHarnessEvolveDefaults fills zero values while preserving Enabled from
// DefaultConfig or an explicit YAML override.
func applyHarnessEvolveDefaults(cfg *Config) {
	h := &cfg.HarnessEvolve
	if h.IntervalHours < 1 {
		h.IntervalHours = 24
	}
	if h.LookbackHours <= 0 {
		h.LookbackHours = 168
	}
	if h.MinClusterSize <= 0 {
		h.MinClusterSize = 2
	}
	if h.Sink == "" {
		h.Sink = "local-task"
	}
}

// applySelfMonitorDefaults fills zero values for the SelfMonitor block so
// older configs behave deterministically and the service can rely on every
// field. Enabled stays false until operators opt in.
func applySelfMonitorDefaults(cfg *Config) {
	s := &cfg.SelfMonitor
	if s.IntervalHours < 1 {
		s.IntervalHours = 6
	}
	if s.JudgeModel == "" {
		s.JudgeModel = "claude-haiku-4-5-20251001"
	}
	if s.SynthesizerModel == "" {
		s.SynthesizerModel = "claude-sonnet-4-6"
	}
	if s.MaxIssuesPerRun <= 0 {
		s.MaxIssuesPerRun = 5
	}
	if s.MaxAutoActionsPerDay <= 0 {
		s.MaxAutoActionsPerDay = 3
	}
	if len(s.AutoActCategories) == 0 {
		s.AutoActCategories = []string{
			"stuck_task",
			"workflow_loop",
			"cost_outlier",
			"triage_mismatch",
		}
	}
	// DryRun defaults to true as the first-week safety net. Operators flip
	// it to false once the ledger shows clean ActionRecords. Because bool
	// zero-values are indistinguishable from explicit false, we only flip
	// to true when the whole SelfMonitor block is freshly populated — i.e.
	// when none of the user-facing knobs were set. This avoids silently
	// re-enabling DryRun on an operator who explicitly disabled it.
	//
	// Proxy for "freshly populated": IssueLabel is the last field the
	// operator typically edits; if it's empty after the above defaults
	// ran, we know nothing in the block was user-specified.
	if s.IssueCooldownHours <= 0 {
		s.IssueCooldownHours = 24
	}
	if s.IssueLabel == "" {
		s.IssueLabel = "selfmonitor"
		s.DryRun = true
	}
	if s.MaxCostPerTickUSD <= 0 {
		s.MaxCostPerTickUSD = 2.0
	}
	if s.JudgeParallelism <= 0 {
		s.JudgeParallelism = 4
	}
	if s.SuppressionDays <= 0 {
		s.SuppressionDays = 7
	}
	if s.SuppressionThreshold <= 0 {
		s.SuppressionThreshold = 3
	}
}

// applyOrchestratorDefaults fills zero values for the Orchestrator block so the
// dispatch loop has a fast scheduling cadence and a slower maintenance cadence
// even on configs that predate the split.
func applyOrchestratorDefaults(cfg *Config) {
	if cfg.Orchestrator.DispatchIntervalSeconds <= 0 {
		cfg.Orchestrator.DispatchIntervalSeconds = 10
	}
	if cfg.Orchestrator.MaintenanceIntervalSeconds <= 0 {
		cfg.Orchestrator.MaintenanceIntervalSeconds = 60
	}
}

// applyMonitorDefaults fills zero values for the Monitor block so older
// configs behave deterministically and the service can rely on every field.
// Enabled stays false until users opt in.
func applyMonitorDefaults(cfg *Config) {
	if cfg.Monitor.IntervalSeconds < 60 {
		cfg.Monitor.IntervalSeconds = 300
	}
	if cfg.Monitor.Model == "" {
		cfg.Monitor.Model = "sonnet"
	}
	if cfg.Monitor.IssueCooldownMinutes <= 0 {
		cfg.Monitor.IssueCooldownMinutes = 30
	}
	if cfg.Monitor.DispatchLimit <= 0 {
		cfg.Monitor.DispatchLimit = cfg.Agent.MaxConcurrent
	}
	if cfg.Monitor.StuckHumanHours <= 0 {
		cfg.Monitor.StuckHumanHours = 8
	}
	if cfg.Monitor.LostAgentMinutes <= 0 {
		cfg.Monitor.LostAgentMinutes = 15
	}
	if cfg.Monitor.FailureRateThreshold <= 0 {
		cfg.Monitor.FailureRateThreshold = 0.3
	}
	if cfg.Monitor.IssueLabel == "" {
		cfg.Monitor.IssueLabel = "monitor"
	}
	if cfg.Monitor.IssueRepo == "" {
		cfg.Monitor.IssueRepo = "Automaat/sybra"
	}
	if cfg.Monitor.BottleneckHours == nil {
		cfg.Monitor.BottleneckHours = map[string]float64{}
	}
	if _, ok := cfg.Monitor.BottleneckHours["plan-review"]; !ok {
		cfg.Monitor.BottleneckHours["plan-review"] = 4
	}
	if _, ok := cfg.Monitor.BottleneckHours["human-required"]; !ok {
		cfg.Monitor.BottleneckHours["human-required"] = 8
	}
	if _, ok := cfg.Monitor.BottleneckHours["in-progress"]; !ok {
		cfg.Monitor.BottleneckHours["in-progress"] = 6
	}
	if _, ok := cfg.Monitor.BottleneckHours["default"]; !ok {
		cfg.Monitor.BottleneckHours["default"] = 12
	}
}

// applyProvidersDefaults fills zero values for the Providers block so older
// configs (which predate the block entirely) behave identically to the
// DefaultConfig factory.
func applyProvidersDefaults(cfg *Config) {
	if cfg.Providers.HealthCheck.IntervalSeconds <= 0 {
		cfg.Providers.HealthCheck.IntervalSeconds = 300
	}
	if cfg.Providers.HealthCheck.IntervalSeconds < 60 {
		cfg.Providers.HealthCheck.IntervalSeconds = 60
	}
	if cfg.Providers.Claude.RateLimitCooldownSeconds <= 0 {
		cfg.Providers.Claude.RateLimitCooldownSeconds = 900
	}
	if cfg.Providers.Codex.RateLimitCooldownSeconds <= 0 {
		cfg.Providers.Codex.RateLimitCooldownSeconds = 900
	}
	if cfg.Providers.Copilot.RateLimitCooldownSeconds <= 0 {
		cfg.Providers.Copilot.RateLimitCooldownSeconds = 900
	}
	if cfg.Providers.Limits.SessionThresholdPercent <= 0 {
		cfg.Providers.Limits.SessionThresholdPercent = 85
	}
	if cfg.Providers.Limits.WeeklyThresholdPercent <= 0 {
		cfg.Providers.Limits.WeeklyThresholdPercent = 90
	}
	if cfg.Providers.Limits.BackfillDays <= 0 {
		cfg.Providers.Limits.BackfillDays = 14
	}
}

func (c *LoggingConfig) SlogLevel() slog.Level {
	switch c.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("# Sybra configuration\n# All values are optional — defaults apply when omitted.\n"), 0o644)
}

func configPath() string {
	return filepath.Join(HomeDir(), "config.yaml")
}

func defaultLogDir() string {
	return filepath.Join(HomeDir(), "logs")
}

func defaultTasksDir() string {
	return filepath.Join(HomeDir(), "tasks")
}

func defaultSkillsDir() string {
	return filepath.Join(HomeDir(), ".claude", "skills")
}

func defaultProjectsDir() string {
	return filepath.Join(HomeDir(), "projects")
}

func defaultClonesDir() string {
	return filepath.Join(HomeDir(), "clones")
}

func defaultWorktreesDir() string {
	return filepath.Join(HomeDir(), "worktrees")
}

func defaultLoopAgentsDir() string {
	return filepath.Join(HomeDir(), "loop-agents")
}

func WorkflowsDir() string {
	return filepath.Join(HomeDir(), "workflows")
}

// AgentsDir is the directory under ~/.sybra that holds the live-agent
// registry (one YAML file per running agent) used to reattach to
// subprocesses that survived an app restart.
func AgentsDir() string {
	return filepath.Join(HomeDir(), "agents")
}

func StatsFile() string {
	return filepath.Join(HomeDir(), "stats.json")
}

func LimitsFile() string {
	return filepath.Join(HomeDir(), "limits.json")
}

// ArtifactsDir is the directory under ~/.sybra that holds per-task harness
// artifacts (plan snapshots, trace events, generic intermediate outputs).
// Layout: <dir>/<task-id>/<name> + <name>.meta.json.
func ArtifactsDir() string {
	return filepath.Join(HomeDir(), "artifacts")
}

// SelfMonitorDir is the directory under ~/.sybra that holds the
// selfmonitor ledger, last-report snapshot, and any other persisted state
// the service owns.
func SelfMonitorDir() string {
	return filepath.Join(HomeDir(), "selfmonitor")
}

// HarnessEvolveDir is the local store for governed harness-evolution proposal
// records and run snapshots.
func HarnessEvolveDir() string {
	return filepath.Join(HomeDir(), "harness-evolution")
}

// SelfMonitorLedgerPath is the append-only ledger file selfmonitor.Open uses.
func SelfMonitorLedgerPath() string {
	return filepath.Join(SelfMonitorDir(), "ledger.jsonl")
}

// EvaluationReportPath is where the background evaluation service persists its
// most recent scorecard as JSON for the dashboard and inspection. The CLI
// `sybra-cli evaluation scan` recomputes a fresh report rather than reading it.
func EvaluationReportPath() string {
	return filepath.Join(HomeDir(), "evaluation-report.json")
}

// SelfMonitorLastReportPath is where the service writes the most recent
// Report as JSON. The CLI `sybra-cli selfmonitor scan` reads from here.
func SelfMonitorLastReportPath() string {
	return filepath.Join(SelfMonitorDir(), "last-report.json")
}

// HealthReportPath is the canonical path the health.Checker persists its
// rollup report to. Exposed here so CLI commands (and the selfmonitor
// service) can read it without hardcoding the layout.
func HealthReportPath() string {
	return filepath.Join(HomeDir(), "health-report.json")
}
