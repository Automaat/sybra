package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Logging       LoggingConfig      `yaml:"logging" json:"logging"`
	Audit         AuditConfig        `yaml:"audit" json:"audit"`
	Agent         AgentDefaults      `yaml:"agent" json:"agent"`
	Notification  NotificationConfig `yaml:"notification" json:"notification"`
	Orchestrator  OrchestratorConfig `yaml:"orchestrator" json:"orchestrator"`
	Todoist       TodoistConfig      `yaml:"todoist" json:"todoist"`
	Renovate      RenovateConfig     `yaml:"renovate" json:"renovate"`
	GitHub        GitHubConfig       `yaml:"github" json:"github"`
	Triage        TriageConfig       `yaml:"triage" json:"triage"`
	Monitor       MonitorConfig      `yaml:"monitor" json:"monitor"`
	SelfMonitor   SelfMonitorConfig  `yaml:"self_monitor" json:"selfMonitor"`
	Providers     ProvidersConfig    `yaml:"providers" json:"providers"`
	Metrics       MetricsConfig      `yaml:"metrics" json:"metrics"`
	ProjectTypes  []string           `yaml:"project_types" json:"projectTypes"`
	TasksDir      string             `yaml:"tasks_dir" json:"tasksDir"`
	SkillsDir     string             `yaml:"skills_dir" json:"skillsDir"`
	RepoDir       string             `yaml:"repo_dir" json:"repoDir"`
	ProjectsDir   string             `yaml:"projects_dir" json:"projectsDir"`
	ClonesDir     string             `yaml:"clones_dir" json:"clonesDir"`
	WorktreesDir  string             `yaml:"worktrees_dir" json:"worktreesDir"`
	LoopAgentsDir string             `yaml:"loop_agents_dir" json:"loopAgentsDir"`
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
	// RequirePermissions sets the default permission requirement for agents.
	// nil means not configured (falls back to true — safe default).
	// Set to false in config to opt all tasks into skip-permissions mode.
	RequirePermissions *bool `yaml:"require_permissions" json:"requirePermissions"`
	// MaxLogEvents caps how many NDJSON events are returned when replaying
	// a completed agent's log file. 0 means use DefaultMaxLogEvents (500).
	MaxLogEvents int `yaml:"max_log_events" json:"maxLogEvents"`
}

// DefaultMaxLogEvents returns the configured cap or 500 if unset.
func (c *Config) DefaultMaxLogEvents() int {
	if c != nil && c.Agent.MaxLogEvents > 0 {
		return c.Agent.MaxLogEvents
	}
	return 500
}

// DefaultRequirePermissions returns the configured default, or true if unset.
func (c *Config) DefaultRequirePermissions() bool {
	if c != nil && c.Agent.RequirePermissions != nil {
		return *c.Agent.RequirePermissions
	}
	return true
}

type NotificationConfig struct {
	Desktop bool `yaml:"desktop" json:"desktop"`
}

type OrchestratorConfig struct {
	AutoTriage bool `yaml:"auto_triage" json:"autoTriage"`
	AutoPlan   bool `yaml:"auto_plan" json:"autoPlan"`
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

// TriageConfig controls the background auto-triage worker. When Enabled,
// synapse periodically classifies tasks in status=new via claude -p and
// atomically applies the verdict (title, tags, size/type, mode, project).
type TriageConfig struct {
	Enabled     bool   `yaml:"enabled" json:"enabled"`
	PollSeconds int    `yaml:"poll_seconds" json:"pollSeconds"`
	Model       string `yaml:"model" json:"model"`
}

// SelfMonitorConfig controls the in-process selfmonitor service that
// replaces the /loop 6h /synapse-self-monitor skill. Each tick snapshots
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
// /loop 5m /synapse-monitor skill. Each tick snapshots the board + audit
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
}

// ProvidersConfig groups per-machine routing for CLI providers (claude, codex)
// and their background health-check loop. A missing block defaults to "both
// providers enabled, health check on, auto-failover on, 300s interval".
type ProvidersConfig struct {
	HealthCheck  ProviderHealthCheckConfig `yaml:"health_check" json:"healthCheck"`
	Claude       ProviderEntryConfig       `yaml:"claude" json:"claude"`
	Codex        ProviderEntryConfig       `yaml:"codex" json:"codex"`
	AutoFailover bool                      `yaml:"auto_failover" json:"autoFailover"`
}

type ProviderHealthCheckConfig struct {
	Enabled         bool `yaml:"enabled" json:"enabled"`
	IntervalSeconds int  `yaml:"interval_seconds" json:"intervalSeconds"`
}

type ProviderEntryConfig struct {
	Enabled                  bool `yaml:"enabled" json:"enabled"`
	RateLimitCooldownSeconds int  `yaml:"rate_limit_cooldown_seconds" json:"rateLimitCooldownSeconds"`
}

// MetricsConfig controls the OpenTelemetry metrics pipeline. When Enabled is
// true, synapse-server mounts /metrics on its existing mux and emits
// Prometheus-format output for external scrapers.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

func HomeDir() string {
	if dir := os.Getenv("SYNAPSE_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".synapse")
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
			MaxConcurrent: 3,
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
		Providers: ProvidersConfig{
			HealthCheck: ProviderHealthCheckConfig{
				Enabled:         true,
				IntervalSeconds: 300,
			},
			Claude:       ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
			Codex:        ProviderEntryConfig{Enabled: true, RateLimitCooldownSeconds: 900},
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

// Directories returns the resolved paths for all synapse data directories.
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

	if v := os.Getenv("SYNAPSE_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("SYNAPSE_LOG_DIR"); v != "" {
		cfg.Logging.Dir = v
	}

	if cfg.Logging.Dir == "" {
		cfg.Logging.Dir = defaultLogDir()
	}
	if cfg.TasksDir == "" {
		cfg.TasksDir = defaultTasksDir()
	}
	if v := os.Getenv("SYNAPSE_TASKS_DIR"); v != "" {
		cfg.TasksDir = v
	}

	if cfg.SkillsDir == "" {
		cfg.SkillsDir = defaultSkillsDir()
	}
	// Migration: previous releases defaulted to ~/.synapse/skills which Claude
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

	if v := os.Getenv("SYNAPSE_TODOIST_TOKEN"); v != "" {
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
	applySelfMonitorDefaults(cfg)

	return cfg, nil
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
		cfg.Monitor.DispatchLimit = 3
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
	return os.WriteFile(path, []byte("# Synapse configuration\n# All values are optional — defaults apply when omitted.\n"), 0o644)
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

func StatsFile() string {
	return filepath.Join(HomeDir(), "stats.json")
}

// SelfMonitorDir is the directory under ~/.synapse that holds the
// selfmonitor ledger, last-report snapshot, and any other persisted state
// the service owns.
func SelfMonitorDir() string {
	return filepath.Join(HomeDir(), "selfmonitor")
}

// SelfMonitorLedgerPath is the append-only ledger file selfmonitor.Open uses.
func SelfMonitorLedgerPath() string {
	return filepath.Join(SelfMonitorDir(), "ledger.jsonl")
}

// SelfMonitorLastReportPath is where the service writes the most recent
// Report as JSON. The CLI `synapse-cli selfmonitor scan` reads from here.
func SelfMonitorLastReportPath() string {
	return filepath.Join(SelfMonitorDir(), "last-report.json")
}

// HealthReportPath is the canonical path the health.Checker persists its
// rollup report to. Exposed here so CLI commands (and the selfmonitor
// service) can read it without hardcoding the layout.
func HealthReportPath() string {
	return filepath.Join(HomeDir(), "health-report.json")
}
