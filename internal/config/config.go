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
	if cfg.Agent.Provider == "" {
		cfg.Agent.Provider = "claude"
	}

	applyProvidersDefaults(cfg)

	return cfg, nil
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
