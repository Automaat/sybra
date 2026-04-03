package config

import (
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Logging      LoggingConfig `yaml:"logging"`
	TasksDir     string        `yaml:"tasks_dir"`
	SkillsDir    string        `yaml:"skills_dir"`
	RepoDir      string        `yaml:"repo_dir"`
	ProjectsDir  string        `yaml:"projects_dir"`
	ClonesDir    string        `yaml:"clones_dir"`
	WorktreesDir string        `yaml:"worktrees_dir"`
}

type LoggingConfig struct {
	Level     string `yaml:"level"`
	Dir       string `yaml:"dir"`
	MaxSizeMB int    `yaml:"max_size_mb"`
	MaxFiles  int    `yaml:"max_files"`
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
		TasksDir: defaultTasksDir(),
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
