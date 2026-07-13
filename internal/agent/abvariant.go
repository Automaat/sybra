package agent

import (
	"os/exec"

	"github.com/Automaat/sybra/internal/abtest"
)

var providerCLIAvailable = func(provider string) bool {
	_, err := exec.LookPath(provider)
	return err == nil
}

// ApplyABVariant selects an A/B variant for (taskID, role) and stamps the
// chosen provider/model/effort and experiment attribution onto cfg. It is the
// reuse point for dispatch sites outside the workflow engine (orchestrator,
// human-review, staff PR review) so they route through the same A/B suite as
// normal task agents instead of a hardcoded provider.
//
// Variants whose provider CLI is missing, config-disabled, unhealthy, or
// rate-limited are filtered out before selection — the same eligibility gate
// the workflow engine applies — so a variant is never assigned to a provider
// that would crash at spawn (e.g. copilot enabled in config but absent on a
// server host). When every variant is ineligible, A/B is disabled, or no
// experiment matches role, cfg is returned unchanged and the provider follows
// the manager default plus failover.
//
// DisableProviderFailover is deliberately left unset: a selected provider that
// caps mid-flight fails over rather than parking (availability over clean
// attribution).
func (m *Manager) ApplyABVariant(cfg RunConfig, ab abtest.Config, taskID, role string) RunConfig {
	allowed := func(provider string) bool {
		return providerCLIAvailable(provider) &&
			m.ProviderHealthy(provider) &&
			!m.ProviderRateLimited(provider)
	}
	m.mu.RLock()
	evalPassed := m.evalPassed
	m.mu.RUnlock()
	a, ok, err := abtest.SelectEligibleForContext(
		ab,
		abtest.SelectionContext{TaskID: taskID, Role: role},
		allowed,
		evalPassed,
	)
	if err != nil || !ok {
		return cfg
	}
	cfg.Provider = a.Provider
	if a.Model != "" {
		cfg.Model = a.Model
	}
	if a.ReasoningEffort != "" {
		cfg.ReasoningEffort = a.ReasoningEffort
	}
	cfg.Prompt = abtest.ApplyPromptTransform(cfg.Prompt, a.PromptTransform)
	cfg.ExperimentID = a.ExperimentID
	cfg.VariantID = a.VariantID
	cfg.AssignmentUnit = a.AssignmentUnit
	cfg.AssignmentKey = a.AssignmentKey
	return cfg
}
