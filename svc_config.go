package main

import (
	"fmt"
	"log/slog"
	"regexp"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/notification"
)

// ConfigService exposes settings read/write as Wails-bound methods.
type ConfigService struct {
	cfg        *config.Config
	logLevel   *slog.LevelVar
	notifier   *notification.Emitter
	agents     *agent.Manager
	reloadHook func() // called after todoist config changes
}

// GetSettings returns the current app settings for the config UI.
func (s *ConfigService) GetSettings() AppSettings {
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
		Directories: c.Directories(),
	}
}

// UpdateSettings validates, persists, and hot-reloads the provided settings.
func (s *ConfigService) UpdateSettings(settings AppSettings) error {
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

	s.cfg.Agent = settings.Agent
	s.cfg.Notification = settings.Notification
	s.cfg.Orchestrator = settings.Orchestrator
	s.cfg.Logging.Level = settings.Logging.Level
	s.cfg.Logging.MaxSizeMB = settings.Logging.MaxSizeMB
	s.cfg.Logging.MaxFiles = settings.Logging.MaxFiles
	s.cfg.Audit = settings.Audit
	s.cfg.Todoist = settings.Todoist
	s.cfg.Renovate = settings.Renovate

	s.notifier.SetDesktop(settings.Notification.Desktop)
	s.agents.SetMaxConcurrent(settings.Agent.MaxConcurrent)
	s.agents.SetDefaultProvider(settings.Agent.Provider)
	if s.logLevel != nil {
		s.logLevel.Set(s.cfg.Logging.SlogLevel())
	}
	if s.reloadHook != nil {
		s.reloadHook()
	}

	return s.cfg.Save()
}
