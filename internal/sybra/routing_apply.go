package sybra

import "github.com/Automaat/sybra/internal/abtest"

// applyRoutingWeights is internal/routing's WeightApplier: it pushes a
// merged (base + overlay) A/B config to every live selection site.
//
//   - App's live A/B snapshot is swapped atomically for direct dispatch sites
//     (human-review and staff-review) that need the latest routing weights
//     without racing the config struct.
//   - workflowEngine, orchSvc, and evaluationSvc each hold their own copy
//     (set once at wiring time) rather than reading a.cfg live, so each needs
//     its own explicit push.
//
// Never called with routing disabled (routing.Service gates Apply on
// Cfg.Enabled itself), and always safe to call with a nil App collaborator
// (e.g. a test App that never wired workflowEngine) — each push is a no-op
// on a nil receiver.
func (a *App) applyRoutingWeights(cfg abtest.Config) error {
	a.setLiveABTesting(cfg)
	if a.workflowEngine != nil {
		a.workflowEngine.SetABTestingConfig(cfg)
	}
	if a.orchSvc != nil {
		a.orchSvc.setABTesting(cfg)
	}
	if a.evaluationSvc != nil {
		a.evaluationSvc.SetABTesting(cfg)
	}
	return nil
}
