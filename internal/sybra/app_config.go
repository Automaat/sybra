package sybra

import (
	"github.com/Automaat/sybra/internal/config"
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
	Todoist      config.TodoistConfig      `json:"todoist"`
	Renovate     config.RenovateConfig     `json:"renovate"`
	Providers    config.ProvidersConfig    `json:"providers"`
	Directories  map[string]string         `json:"directories"`
}
