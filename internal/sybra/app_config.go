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
//
// Every section here is round-tripped by GetSettings → UpdateSettings. Adding a
// field means updating four sites in lockstep: this struct, settingsToConfig,
// configToSettings (svc_config_reload.go), and — if it needs validation —
// validateSettings. applyFromConfig assigns the section into the live config.
type AppSettings struct {
	Agent        config.AgentDefaults      `json:"agent"`
	Notification config.NotificationConfig `json:"notification"`
	Orchestrator config.OrchestratorConfig `json:"orchestrator"`
	Logging      LoggingSettings           `json:"logging"`
	Audit        config.AuditConfig        `json:"audit"`
	Todoist      config.TodoistConfig      `json:"todoist"`
	Renovate     config.RenovateConfig     `json:"renovate"`
	Providers    config.ProvidersConfig    `json:"providers"`
	GitHub       config.GitHubConfig       `json:"github"`
	Monitor      config.MonitorConfig      `json:"monitor"`
	SelfMonitor  config.SelfMonitorConfig  `json:"selfMonitor"`
	Triage       config.TriageConfig       `json:"triage"`
	Umbrella     config.UmbrellaConfig     `json:"umbrella"`
	Testing      config.TestingConfig      `json:"testing"`
	Experience   config.ExperienceConfig   `json:"experience"`
	Metrics      config.MetricsConfig      `json:"metrics"`
	Browser      config.BrowserConfig      `json:"browser"`
	ProjectTypes []string                  `json:"projectTypes"`
	Directories  map[string]string         `json:"directories"`
	// TodoistTokenSet reports whether a Todoist API token is stored, without
	// leaking the token itself (which GetSettings always redacts). The UI uses
	// this to show a "token set" state and route rotation through
	// UpdateTodoistToken rather than the generic UpdateSettings payload.
	TodoistTokenSet bool `json:"todoistTokenSet"`
}
