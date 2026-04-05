package config

import (
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Logging      LoggingConfig      `yaml:"logging" json:"logging"`
	Audit        AuditConfig        `yaml:"audit" json:"audit"`
	Agent        AgentDefaults      `yaml:"agent" json:"agent"`
	Notification NotificationConfig `yaml:"notification" json:"notification"`
	Orchestrator OrchestratorConfig `yaml:"orchestrator" json:"orchestrator"`
	TasksDir     string             `yaml:"tasks_dir" json:"tasksDir"`
	SkillsDir    string             `yaml:"skills_dir" json:"skillsDir"`
	RepoDir      string             `yaml:"repo_dir" json:"repoDir"`
	ProjectsDir  string             `yaml:"projects_dir" json:"projectsDir"`
	ClonesDir    string             `yaml:"clones_dir" json:"clonesDir"`
	WorktreesDir string             `yaml:"worktrees_dir" json:"worktreesDir"`
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
	Model         string `yaml:"model" json:"model"`
	Mode          string `yaml:"mode" json:"mode"`
	MaxConcurrent int    `yaml:"max_concurrent" json:"maxConcurrent"`
}

type NotificationConfig struct {
	Desktop bool `yaml:"desktop" json:"desktop"`
}

type OrchestratorConfig struct {
	AutoTriage bool `yaml:"auto_triage" json:"autoTriage"`
	AutoPlan   bool `yaml:"auto_plan" json:"autoPlan"`
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
			MaxConcurrent: 3,
		},
		Notification: NotificationConfig{
			Desktop: true,
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
		"tasks":     c.TasksDir,
		"skills":    c.SkillsDir,
		"projects":  c.ProjectsDir,
		"clones":    c.ClonesDir,
		"worktrees": c.WorktreesDir,
		"logs":      c.Logging.Dir,
		"audit":     c.AuditDir(),
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
	if cfg.ProjectsDir == "" {
		cfg.ProjectsDir = defaultProjectsDir()
	}
	if cfg.ClonesDir == "" {
		cfg.ClonesDir = defaultClonesDir()
	}
	if cfg.WorktreesDir == "" {
		cfg.WorktreesDir = defaultWorktreesDir()
	}

	return cfg, nil
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
	return filepath.Join(HomeDir(), "skills")
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

func StatsFile() string {
	return filepath.Join(HomeDir(), "stats.json")
}
