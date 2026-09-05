package sybra

import (
	"fmt"

	"github.com/Automaat/sybra/internal/config"
)

// ReloadFromDisk re-reads ~/.sybra/config.yaml, validates it, diffs against
// the persisted intent snapshot, and applies only hot-reloadable changes.
// Restart-required values stay pending until process restart. If an external
// writer leaves config.yaml unreadable or invalid, restore the last-known-good
// file so the process does not stay wedged on a broken operator config.
func (s *ConfigService) ReloadFromDisk() (ConfigMutationResult, error) {
	next, err := config.LoadNoPersist()
	if err != nil {
		result := ConfigMutationResult{}
		if restoreErr := config.RestoreLastKnownGoodConfig(); restoreErr != nil {
			result.Recovery = &ConfigRecovery{
				Message: fmt.Sprintf("config reload failed and last-known-good restore failed: %v", restoreErr),
			}
			return result, err
		}
		result.Recovery = &ConfigRecovery{
			RestoredLastKnownGood: true,
			Message:               "restored config.yaml from last-known-good after reload failure",
		}
		return result, err
	}

	result, pending, err := func() (ConfigMutationResult, pendingNotify, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.mutateLocked(next, nil)
	}()
	// Outside the lock: subscribers may read config back through this service.
	pending.run()
	return result, err
}

// configToSettings converts a *config.Config into AppSettings for validation
// and for the default-settings baseline the UI diffs against. It mirrors every
// editable section GetSettings exposes.
func configToSettings(c *config.Config) AppSettings {
	return AppSettings{
		Agent:        c.Agent,
		Notification: c.Notification,
		Orchestrator: c.Orchestrator,
		Logging: LoggingSettings{
			Level:     c.Logging.Level,
			MaxSizeMB: c.Logging.MaxSizeMB,
			MaxFiles:  c.Logging.MaxFiles,
		},
		Attachments: c.Attachments,
		Audit:       c.Audit,
		Renovate:    c.Renovate,
		Providers:   c.Providers,
		ProviderRouting: ProviderRoutingSettings{
			ABTestingEnabled:              c.ABTesting.Enabled,
			ABTestingMinSamplesPerVariant: c.ABTesting.MinSamplesPerVariant,
			Summary:                       config.BuildRoutingSummary(c),
		},
		GitHub:       githubSettingsWithoutSecrets(c.GitHub),
		Monitor:      c.Monitor,
		SelfMonitor:  c.SelfMonitor,
		Triage:       c.Triage,
		Umbrella:     c.Umbrella,
		Testing:      c.Testing,
		Experience:   c.Experience,
		Metrics:      c.Metrics,
		Browser:      c.Browser,
		ProjectTypes: c.ProjectTypes,
	}
}
