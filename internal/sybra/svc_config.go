package sybra

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/notification"
	"github.com/Automaat/sybra/internal/workflow"
)

// ConfigService exposes settings read/write as Wails-bound methods.
type ConfigService struct {
	mu             sync.RWMutex
	cfg            *config.Config
	logLevel       *slog.LevelVar
	notifier       *notification.Emitter
	agents         *agent.Manager
	limits         *limits.Store
	workflowEngine *workflow.Engine
	logger         *slog.Logger
	policy         func() limits.Policy
	reloadHook     func() // called after todoist config changes
}

// GetSettings returns the current app settings for the config UI.
// Secret fields (e.g. Todoist.APIToken) are redacted — callers must use
// dedicated write-only methods (UpdateTodoistToken) to rotate them.
func (s *ConfigService) GetSettings() AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	todoist := c.Todoist
	tokenSet := todoist.APIToken != ""
	todoist.APIToken = "" // never leak the token over the read API
	return AppSettings{
		Agent:        c.Agent,
		Notification: c.Notification,
		Orchestrator: c.Orchestrator,
		Logging: LoggingSettings{
			Level:     c.Logging.Level,
			MaxSizeMB: c.Logging.MaxSizeMB,
			MaxFiles:  c.Logging.MaxFiles,
		},
		Audit:           c.Audit,
		Todoist:         todoist,
		Renovate:        c.Renovate,
		Providers:       c.Providers,
		GitHub:          c.GitHub,
		Monitor:         c.Monitor,
		SelfMonitor:     c.SelfMonitor,
		Triage:          c.Triage,
		Umbrella:        c.Umbrella,
		Testing:         c.Testing,
		Experience:      c.Experience,
		Metrics:         c.Metrics,
		Browser:         c.Browser,
		ProjectTypes:    c.ProjectTypes,
		Directories:     c.Directories(),
		TodoistTokenSet: tokenSet,
	}
}

// UpdateTodoistToken sets or clears the Todoist API token and persists the config.
// Pass an empty string to remove the stored token.
// This is the only write path for the token — GetSettings never returns it.
func (s *ConfigService) UpdateTodoistToken(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Todoist.APIToken = token
	if err := s.cfg.Save(); err != nil {
		return err
	}
	if s.reloadHook != nil {
		s.reloadHook()
	}
	return nil
}

// GetDefaultSettings returns the settings a fresh install would have. The UI
// diffs live values against these to flag "modified from default" fields and to
// power per-field reset-to-default, without hardcoding defaults in TypeScript.
func (s *ConfigService) GetDefaultSettings() AppSettings {
	return configToSettings(config.DefaultConfig())
}

// GetRawConfig returns the raw config.yaml text for the Advanced (YAML) editor.
// Unlike GetSettings this is NOT redacted — it is the user's own local file,
// surfaced behind an explicit Advanced disclosure with a secrets warning.
func (s *ConfigService) GetRawConfig() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return config.ReadRawConfig()
}

// SaveRawConfig validates raw YAML, atomically writes it (preserving the user's
// formatting and comments), then hot-reloads. Invalid YAML or a value that
// fails validation is rejected without touching disk.
func (s *ConfigService) SaveRawConfig(raw string) error {
	fileCfg, err := config.ParseFileConfig([]byte(raw))
	if err != nil {
		return validationError(fmt.Sprintf("invalid config: %s", err))
	}
	if _, err := config.ResolveFromCurrentEnvironment(fileCfg, config.ResolveOptions{}); err != nil {
		return validationError(err.Error())
	}
	if err := config.WriteRawConfig([]byte(raw)); err != nil {
		return err
	}
	_, err = s.ReloadFromDisk()
	return err
}

// UpdateSettings validates, persists, and hot-reloads the provided settings.
func (s *ConfigService) UpdateSettings(settings AppSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validateSettings(settings); err != nil {
		return err
	}
	if err := s.applyFromConfig(settingsToConfig(s.cfg, settings)); err != nil {
		return err
	}
	return s.cfg.Save()
}

// validateSettings checks all editable fields for validity.
func (s *ConfigService) validateSettings(settings AppSettings) error {
	next := settingsToConfig(s.cfg, settings)
	if next.Todoist.PollSeconds == 0 {
		next.Todoist.PollSeconds = 120
	}
	if err := config.ValidateResolvedConfig(&next); err != nil {
		return validationError(err.Error())
	}
	return nil
}

// applyFromConfig assigns all hot-reloadable fields from next into s.cfg and
// pushes the manager settings that are intentionally live. s.mu must be held by the caller.
// This never writes to disk — callers that need persistence must call s.cfg.Save().
func (s *ConfigService) applyFromConfig(next config.Config) error {
	s.cfg.Agent = next.Agent
	s.cfg.Notification = next.Notification
	s.cfg.Orchestrator = next.Orchestrator
	s.cfg.Logging.Level = next.Logging.Level
	s.cfg.Logging.MaxSizeMB = next.Logging.MaxSizeMB
	s.cfg.Logging.MaxFiles = next.Logging.MaxFiles
	s.cfg.Audit = next.Audit
	s.cfg.Todoist = next.Todoist
	// In-place field assignment: the renovate coordinator holds &s.cfg.Renovate.
	s.cfg.Renovate.Enabled = next.Renovate.Enabled
	s.cfg.Renovate.Author = next.Renovate.Author
	s.cfg.Providers = next.Providers
	s.cfg.GitHub = next.GitHub
	s.cfg.Triage = next.Triage
	s.cfg.Monitor = next.Monitor
	s.cfg.SelfMonitor = next.SelfMonitor
	s.cfg.Umbrella = next.Umbrella
	s.cfg.Testing = next.Testing
	s.cfg.Experience = next.Experience
	s.cfg.Browser = next.Browser
	s.cfg.ABTesting = next.ABTesting
	s.cfg.Metrics = next.Metrics
	s.cfg.ProjectTypes = next.ProjectTypes
	s.notifier.SetDesktop(next.Notification.Desktop)
	if err := s.refreshAgentRuntimeConfig(next); err != nil {
		return err
	}
	if s.agents != nil {
		s.agents.SetGuardrails(agent.Guardrails{
			MaxCostUSD:              next.Agent.MaxCostUSD,
			MaxTurns:                next.Agent.MaxTurns,
			MaxCheckpoints:          next.MaxCheckpoints(),
			TurnCostFraction:        next.Agent.TurnCostFraction,
			TurnMultiplier:          next.Agent.TurnMultiplier,
			CheckpointOnTurnCeiling: next.CheckpointOnTurnCeilingEnabled(),
		})
	}
	if s.workflowEngine != nil {
		s.workflowEngine.SetMaxCheckpoints(next.MaxCheckpoints())
	}
	if s.logLevel != nil {
		s.logLevel.Set(s.cfg.Logging.SlogLevel())
	}
	if s.reloadHook != nil {
		s.reloadHook()
	}
	return nil
}

func (s *ConfigService) refreshAgentRuntimeConfig(next config.Config) error {
	if s.agents == nil {
		return nil
	}
	return s.agents.ReplaceRuntimeConfig(s.managerRuntimeConfig(next))
}

func (s *ConfigService) managerRuntimeConfig(cfg config.Config) agent.ManagerRuntimeConfig {
	policy := limits.DefaultPolicy()
	if s.policy != nil {
		policy = s.policy()
	}
	return agent.ManagerRuntimeConfig{
		MaxConcurrent:          cfg.Agent.MaxConcurrent,
		DefaultProvider:        cfg.Agent.Provider,
		DefaultModel:           cfg.Agent.Model,
		BashTimeoutMs:          cfg.BashTimeoutMs(),
		RetryWatchdog:          cfg.RetryWatchdog(),
		FallbackModel:          cfg.Agent.FallbackModel,
		LimitGate:              agent.LimitGateOrNil(s.limits),
		LimitPolicy:            policy,
		MaxInFlightPerProvider: cfg.Providers.Limits.MaxInFlightPerProvider,
		DispatchJitterMs:       cfg.Agent.DispatchJitterMs,
		HeadlessSteerable:      cfg.DefaultHeadlessSteerable(),
		SandboxMode:            cfg.DefaultSandboxMode(),
		PlaywrightMCPEnabled:   cfg.PlaywrightMCPEnabled(),
		PlaywrightMCPExtraArgs: cfg.PlaywrightMCPExtraArgs(),
		K8sJobsEnabled:         cfg.Agent.K8sJobs.Enabled,
		K8sJobs:                k8sJobRunnerConfigFromConfig(cfg.Agent.K8sJobs),
	}
}

// settingsToConfig converts AppSettings into a config.Config overlay, filling
// fields not present in AppSettings from the existing cfg.
func settingsToConfig(existing *config.Config, settings AppSettings) config.Config {
	next := *existing
	next.Agent = settings.Agent
	next.Notification = settings.Notification
	next.Orchestrator = settings.Orchestrator
	next.Logging.Level = settings.Logging.Level
	next.Logging.MaxSizeMB = settings.Logging.MaxSizeMB
	next.Logging.MaxFiles = settings.Logging.MaxFiles
	next.Audit = settings.Audit
	next.Todoist = settings.Todoist
	// Preserve the stored token when the caller sends a blank (redacted) value.
	if settings.Todoist.APIToken == "" {
		next.Todoist.APIToken = existing.Todoist.APIToken
	}
	next.Renovate = settings.Renovate
	next.Providers = settings.Providers
	next.GitHub = settings.GitHub
	next.Monitor = settings.Monitor
	next.SelfMonitor = settings.SelfMonitor
	next.Triage = settings.Triage
	next.Umbrella = settings.Umbrella
	next.Testing = settings.Testing
	next.Experience = settings.Experience
	next.Metrics = settings.Metrics
	next.Browser = settings.Browser
	next.ProjectTypes = settings.ProjectTypes
	return next
}
