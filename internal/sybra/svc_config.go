package sybra

import (
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/limits"
	"github.com/Automaat/sybra/internal/notification"
)

// modelNameRe restricts the agent model identifier to characters safe to embed
// on a CLI argument without quoting. Compiled once — recompiling per call
// allocates ~1KB of regex state each time UpdateSettings runs.
var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// ConfigService exposes settings read/write as Wails-bound methods.
type ConfigService struct {
	mu         sync.RWMutex
	cfg        *config.Config
	logLevel   *slog.LevelVar
	notifier   *notification.Emitter
	agents     *agent.Manager
	limits     *limits.Store
	logger     *slog.Logger
	policy     func() limits.Policy
	reloadHook func() // called after todoist config changes
}

// GetSettings returns the current app settings for the config UI.
// Secret fields (e.g. Todoist.APIToken) are redacted — callers must use
// dedicated write-only methods (UpdateTodoistToken) to rotate them.
func (s *ConfigService) GetSettings() AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
	todoist := c.Todoist
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
		Audit:       c.Audit,
		Todoist:     todoist,
		Renovate:    c.Renovate,
		Providers:   c.Providers,
		Directories: c.Directories(),
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

// UpdateSettings validates, persists, and hot-reloads the provided settings.
func (s *ConfigService) UpdateSettings(settings AppSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.validateSettings(settings); err != nil {
		return err
	}
	s.applyFromConfig(settingsToConfig(s.cfg, settings))
	return s.cfg.Save()
}

// validateSettings checks all editable fields for validity.
func (s *ConfigService) validateSettings(settings AppSettings) error {
	validProviders := map[string]bool{"": true, "claude": true, "codex": true, "copilot": true}
	if !validProviders[settings.Agent.Provider] {
		return validationError(fmt.Sprintf("invalid provider: %q", settings.Agent.Provider))
	}
	if settings.Agent.Model != "" && !modelNameRe.MatchString(settings.Agent.Model) {
		return validationError(fmt.Sprintf("invalid model: %q", settings.Agent.Model))
	}
	if settings.Agent.FallbackModel != "" && !modelNameRe.MatchString(settings.Agent.FallbackModel) {
		return validationError(fmt.Sprintf("invalid fallback model: %q", settings.Agent.FallbackModel))
	}
	validModes := map[string]bool{"": true, "headless": true, "interactive": true}
	if !validModes[settings.Agent.Mode] {
		return validationError(fmt.Sprintf("invalid mode: %q", settings.Agent.Mode))
	}
	if settings.Agent.MaxConcurrent < 1 || settings.Agent.MaxConcurrent > 100 {
		return validationError("maxConcurrent must be 1–100")
	}
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[settings.Logging.Level] {
		return validationError(fmt.Sprintf("invalid log level: %q", settings.Logging.Level))
	}
	if settings.Logging.MaxSizeMB < 1 || settings.Logging.MaxSizeMB > 500 {
		return validationError("maxSizeMB must be 1–500")
	}
	if settings.Logging.MaxFiles < 1 || settings.Logging.MaxFiles > 50 {
		return validationError("maxFiles must be 1–50")
	}
	if settings.Audit.RetentionDays < 1 || settings.Audit.RetentionDays > 365 {
		return validationError("retentionDays must be 1–365")
	}
	if settings.Todoist.Enabled && settings.Todoist.APIToken == "" && s.cfg.Todoist.APIToken == "" {
		return validationError("todoist API token required when enabled")
	}
	if settings.Todoist.PollSeconds < 30 || settings.Todoist.PollSeconds > 3600 {
		settings.Todoist.PollSeconds = 120
	}
	return nil
}

// applyFromConfig assigns all hot-reloadable fields from next into s.cfg and
// pushes the manager settings that are intentionally live. s.mu must be held by the caller.
// This never writes to disk — callers that need persistence must call s.cfg.Save().
func (s *ConfigService) applyFromConfig(next config.Config) {
	s.cfg.Agent = next.Agent
	s.cfg.Notification = next.Notification
	s.cfg.Orchestrator = next.Orchestrator
	s.cfg.Logging.Level = next.Logging.Level
	s.cfg.Logging.MaxSizeMB = next.Logging.MaxSizeMB
	s.cfg.Logging.MaxFiles = next.Logging.MaxFiles
	s.cfg.Audit = next.Audit
	s.cfg.Todoist = next.Todoist
	// In-place field assignment: renovateHandler holds &s.cfg.Renovate
	s.cfg.Renovate.Enabled = next.Renovate.Enabled
	s.cfg.Renovate.Author = next.Renovate.Author
	s.cfg.Providers = next.Providers
	s.cfg.GitHub = next.GitHub
	s.cfg.Triage = next.Triage
	s.cfg.Monitor = next.Monitor
	s.cfg.SelfMonitor = next.SelfMonitor
	s.cfg.ABTesting = next.ABTesting
	s.cfg.Metrics = next.Metrics
	s.cfg.ProjectTypes = next.ProjectTypes
	s.notifier.SetDesktop(next.Notification.Desktop)
	s.notifier.SetDesktop(next.Notification.Desktop)
	s.refreshAgentRuntimeConfig(next)
	if s.agents != nil {
		s.agents.SetGuardrails(agent.Guardrails{
			MaxCostUSD:       next.Agent.MaxCostUSD,
			MaxTurns:         next.Agent.MaxTurns,
			TurnCostFraction: next.Agent.TurnCostFraction,
			TurnMultiplier:   next.Agent.TurnMultiplier,
		})
	}
	if s.logLevel != nil {
		s.logLevel.Set(s.cfg.Logging.SlogLevel())
	}
	if s.reloadHook != nil {
		s.reloadHook()
	}
}

func (s *ConfigService) refreshAgentRuntimeConfig(next config.Config) {
	if s.agents == nil {
		return
	}
	s.agents.UpdateRuntimeConfig(s.managerRuntimeConfig(next))
}

func (s *ConfigService) managerRuntimeConfig(cfg config.Config) agent.ManagerRuntimeConfig {
	policy := limits.DefaultPolicy()
	if s.policy != nil {
		policy = s.policy()
	}
	return agent.ManagerRuntimeConfig{
		MaxConcurrent:   cfg.Agent.MaxConcurrent,
		DefaultProvider: cfg.Agent.Provider,
		BashTimeoutMs:   cfg.BashTimeoutMs(),
		RetryWatchdog:   cfg.RetryWatchdog(),
		FallbackModel:   cfg.Agent.FallbackModel,
		LimitGate:       s.limits,
		LimitPolicy:     policy,
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
	return next
}
