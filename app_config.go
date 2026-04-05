package main

import (
	"fmt"

	"github.com/Automaat/synapse/internal/config"
)

// LoggingSettings holds the editable subset of LoggingConfig (Dir is read-only).
type LoggingSettings struct {
	Level     string `json:"level"`
	MaxSizeMB int    `json:"maxSizeMB"`
	MaxFiles  int    `json:"maxFiles"`
}

// AppSettings is the shape of data exchanged with the frontend for the config view.
type AppSettings struct {
	Agent        config.AgentDefaults      `json:"agent"`
	Notification config.NotificationConfig `json:"notification"`
	Orchestrator config.OrchestratorConfig `json:"orchestrator"`
	Logging      LoggingSettings           `json:"logging"`
	Audit        config.AuditConfig        `json:"audit"`
	Directories  map[string]string         `json:"directories"`
}

// GetSettings returns the current app settings for display in the config view.
func (a *App) GetSettings() AppSettings {
	c := a.cfg
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
		Directories: c.Directories(),
	}
}

// UpdateSettings validates, persists, and hot-reloads the provided settings.
func (a *App) UpdateSettings(s AppSettings) error {
	validModels := map[string]bool{"": true, "opus": true, "sonnet": true, "haiku": true}
	if !validModels[s.Agent.Model] {
		return fmt.Errorf("invalid model: %q", s.Agent.Model)
	}
	validModes := map[string]bool{"": true, "headless": true, "interactive": true}
	if !validModes[s.Agent.Mode] {
		return fmt.Errorf("invalid mode: %q", s.Agent.Mode)
	}
	if s.Agent.MaxConcurrent < 1 || s.Agent.MaxConcurrent > 10 {
		return fmt.Errorf("maxConcurrent must be 1–10")
	}
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[s.Logging.Level] {
		return fmt.Errorf("invalid log level: %q", s.Logging.Level)
	}
	if s.Logging.MaxSizeMB < 1 || s.Logging.MaxSizeMB > 500 {
		return fmt.Errorf("maxSizeMB must be 1–500")
	}
	if s.Logging.MaxFiles < 1 || s.Logging.MaxFiles > 50 {
		return fmt.Errorf("maxFiles must be 1–50")
	}
	if s.Audit.RetentionDays < 1 || s.Audit.RetentionDays > 365 {
		return fmt.Errorf("retentionDays must be 1–365")
	}

	a.cfg.Agent = s.Agent
	a.cfg.Notification = s.Notification
	a.cfg.Orchestrator = s.Orchestrator
	a.cfg.Logging.Level = s.Logging.Level
	a.cfg.Logging.MaxSizeMB = s.Logging.MaxSizeMB
	a.cfg.Logging.MaxFiles = s.Logging.MaxFiles
	a.cfg.Audit = s.Audit

	// Hot-reload side effects
	a.notifier.SetDesktop(s.Notification.Desktop)
	a.agents.SetMaxConcurrent(s.Agent.MaxConcurrent)
	if a.logLevel != nil {
		a.logLevel.Set(a.cfg.Logging.SlogLevel())
	}

	return a.cfg.Save()
}
