package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment holds the supported config env overrides so Resolve is
// deterministic and testable.
type Environment struct {
	LogLevel       string
	LogDir         string
	TasksDir       string
	TodoistToken   string
	AuthToken      string
	AllowedOrigins []string
}

func environmentFromOS() Environment {
	env := Environment{
		LogLevel:     os.Getenv("SYBRA_LOG_LEVEL"),
		LogDir:       os.Getenv("SYBRA_LOG_DIR"),
		TasksDir:     os.Getenv("SYBRA_TASKS_DIR"),
		TodoistToken: os.Getenv("SYBRA_TODOIST_TOKEN"),
		AuthToken:    os.Getenv("SYBRA_AUTH_TOKEN"),
	}
	if raw := os.Getenv("SYBRA_ALLOWED_ORIGINS"); raw != "" {
		parts := strings.Split(raw, ",")
		env.AllowedOrigins = make([]string, 0, len(parts))
		for _, part := range parts {
			env.AllowedOrigins = append(env.AllowedOrigins, strings.TrimSpace(part))
		}
	}
	return env
}

func ResolveFromCurrentEnvironment(file *FileConfig, opts ResolveOptions) (*ResolveResult, error) {
	return Resolve(file, environmentFromOS(), opts)
}

type ResolveOptions struct {
	GenerateSecrets bool
	ExistingFile    bool
}

type ResolveResult struct {
	Config               *ResolvedConfig
	ABTestingReconciled  bool
	ServerTokenGenerated bool
}

// Resolve converts optional file input into the concrete runtime config used by
// the app, applying defaults, legacy aliases, env overrides, reconciles, and
// cross-field validation in one order.
func Resolve(file *FileConfig, env Environment, opts ResolveOptions) (*ResolveResult, error) {
	cfg := DefaultConfig()

	if file != nil && len(file.data) > 0 {
		cfg.ABTesting.BuiltinVersion = nil
		if err := yaml.Unmarshal(file.data, cfg); err != nil {
			return nil, err
		}
		if err := applyDurationAliases(file, cfg); err != nil {
			return nil, err
		}
		if !file.HasSchemaVersion() {
			applyLegacyGitHubDefault(file.data, cfg)
		}
	}

	applyEnvironmentOverrides(cfg, env)
	applyResolvedDefaults(cfg, file)
	abTestingReconciled := applyABTestingDefaults(cfg, opts.ExistingFile && opts.GenerateSecrets)
	serverTokenGenerated := applyServerDefaultsFromEnvironment(cfg, env, opts.GenerateSecrets)

	if err := ValidateResolvedConfig(cfg); err != nil {
		return nil, err
	}
	return &ResolveResult{
		Config:               cfg,
		ABTestingReconciled:  abTestingReconciled,
		ServerTokenGenerated: serverTokenGenerated,
	}, nil
}

func applyEnvironmentOverrides(cfg *ResolvedConfig, env Environment) {
	if env.LogLevel != "" {
		cfg.Logging.Level = env.LogLevel
	}
	if env.LogDir != "" {
		cfg.Logging.Dir = env.LogDir
	}
	if env.TasksDir != "" {
		cfg.TasksDir = env.TasksDir
	}
	if env.TodoistToken != "" {
		cfg.Todoist.APIToken = env.TodoistToken
	}
	if env.AuthToken != "" {
		cfg.Server.AuthToken = env.AuthToken
	}
	if len(env.AllowedOrigins) > 0 {
		cfg.Server.AllowedOrigins = append([]string(nil), env.AllowedOrigins...)
	}
}

func applyResolvedDefaults(cfg *ResolvedConfig, file *FileConfig) {
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = CurrentSchemaVersion
	}
	if cfg.Logging.Dir == "" {
		cfg.Logging.Dir = defaultLogDir()
	}
	if cfg.TasksDir == "" {
		cfg.TasksDir = defaultTasksDir()
	}
	if cfg.SkillsDir == "" {
		cfg.SkillsDir = defaultSkillsDir()
	}
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
	if cfg.Todoist.PollSeconds <= 0 {
		cfg.Todoist.PollSeconds = 120
	}
	if cfg.Renovate.Author == "" {
		cfg.Renovate.Author = "app/renovate"
	}
	if cfg.Triage.PollSeconds <= 0 {
		cfg.Triage.PollSeconds = 60
	}
	if cfg.Agent.Provider == "" {
		cfg.Agent.Provider = "claude"
	}
	applyProvidersDefaults(cfg)
	applyMonitorDefaults(cfg, file)
	applyWatchdogDefaults(cfg)
	applySelfMonitorDefaults(cfg, file)
	applyEvaluationDefaults(cfg)
	applyLearningDigestDefaults(cfg)
	applyHarnessEvolveDefaults(cfg)
	applyPromptLabDefaults(cfg)
	applyExperienceDefaults(cfg)
	applyOrchestratorDefaults(cfg)
	applyAutoUpdateDefaults(cfg)
	applyReviewHoldDefaults(cfg)
}

func applyServerDefaultsFromEnvironment(cfg *ResolvedConfig, env Environment, allowGenerate bool) bool {
	if env.AuthToken != "" {
		cfg.Server.AuthToken = env.AuthToken
	}
	if len(env.AllowedOrigins) > 0 {
		cfg.Server.AllowedOrigins = append([]string(nil), env.AllowedOrigins...)
	}
	if cfg.Server.AuthToken != "" {
		return false
	}
	if !allowGenerate {
		return false
	}
	token, err := generateAuthToken()
	if err != nil {
		slog.Warn("config: failed to generate server auth token", "err", err)
		return false
	}
	cfg.Server.AuthToken = token
	return true
}
