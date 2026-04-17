package sybra

import (
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/notification"
)

// ConfigService exposes settings read/write as Wails-bound methods.
type ConfigService struct {
	mu         sync.RWMutex
	cfg        *config.Config
	logLevel   *slog.LevelVar
	notifier   *notification.Emitter
	agents     *agent.Manager
	logger     *slog.Logger
	reloadHook func() // called after todoist config changes
}

// GetSettings returns the current app settings for the config UI.
func (s *ConfigService) GetSettings() AppSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cfg
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
		Todoist:     c.Todoist,
		Renovate:    c.Renovate,
		Providers:   c.Providers,
		Directories: c.Directories(),
	}
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
	validProviders := map[string]bool{"": true, "claude": true, "codex": true}
	if !validProviders[settings.Agent.Provider] {
		return fmt.Errorf("invalid provider: %q", settings.Agent.Provider)
	}
	if settings.Agent.Model != "" && !regexp.MustCompile(`^[A-Za-z0-9._/-]+$`).MatchString(settings.Agent.Model) {
		return fmt.Errorf("invalid model: %q", settings.Agent.Model)
	}
	validModes := map[string]bool{"": true, "headless": true, "interactive": true}
	if !validModes[settings.Agent.Mode] {
		return fmt.Errorf("invalid mode: %q", settings.Agent.Mode)
	}
	if settings.Agent.MaxConcurrent < 1 || settings.Agent.MaxConcurrent > 10 {
		return fmt.Errorf("maxConcurrent must be 1–10")
	}
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[settings.Logging.Level] {
		return fmt.Errorf("invalid log level: %q", settings.Logging.Level)
	}
	if settings.Logging.MaxSizeMB < 1 || settings.Logging.MaxSizeMB > 500 {
		return fmt.Errorf("maxSizeMB must be 1–500")
	}
	if settings.Logging.MaxFiles < 1 || settings.Logging.MaxFiles > 50 {
		return fmt.Errorf("maxFiles must be 1–50")
	}
	if settings.Audit.RetentionDays < 1 || settings.Audit.RetentionDays > 365 {
		return fmt.Errorf("retentionDays must be 1–365")
	}
	if settings.Todoist.Enabled && settings.Todoist.APIToken == "" {
		return fmt.Errorf("todoist API token required when enabled")
	}
	if settings.Todoist.PollSeconds < 30 || settings.Todoist.PollSeconds > 3600 {
		settings.Todoist.PollSeconds = 120
	}
	return nil
}

// applyFromConfig assigns all hot-reloadable fields from next into s.cfg and
// calls the corresponding live setters. s.mu must be held by the caller.
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
	s.cfg.Metrics = next.Metrics
	s.cfg.ProjectTypes = next.ProjectTypes

	s.notifier.SetDesktop(next.Notification.Desktop)
	s.agents.SetMaxConcurrent(next.Agent.MaxConcurrent)
	s.agents.SetDefaultProvider(next.Agent.Provider)
	s.agents.SetGuardrails(agent.Guardrails{
		MaxCostUSD: next.Agent.MaxCostUSD,
		MaxTurns:   next.Agent.MaxTurns,
	})
	if s.logLevel != nil {
		s.logLevel.Set(s.cfg.Logging.SlogLevel())
	}
	if s.reloadHook != nil {
		s.reloadHook()
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
	next.Renovate = settings.Renovate
	next.Providers = settings.Providers
	return next
}
