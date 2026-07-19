package sybra

import "github.com/Automaat/sybra/internal/abtest"

// applyRoutingWeights is internal/routing's WeightApplier: it pushes a
// merged (base + overlay) A/B config to every live selection site.
//
//   - a.cfg.ABTesting is mutated in place. This is what app_human_review.go
//     and svc_tasks.go's staff-review dispatch read directly on every call
//     (s.cfg.ABTesting / h.cfg.ABTesting), so those two sites pick up the new
//     weights with no further wiring. It also means a later config.yaml edit
//     that changes ab_testing (config_registry.go's "ab_testing" hot-reload
//     path) still fully replaces this field, as intended — the next routing
//     tick re-applies the overlay on top of whatever base the operator just
//     saved.
//   - workflowEngine, orchSvc, and evaluationSvc each hold their own copy
//     (set once at wiring time) rather than reading a.cfg live, so each needs
//     its own explicit push.
//
// Never called with routing disabled (routing.Service gates Apply on
// Cfg.Enabled itself), and always safe to call with a nil App collaborator
// (e.g. a test App that never wired workflowEngine) — each push is a no-op
// on a nil receiver.
func (a *App) applyRoutingWeights(cfg abtest.Config) error {
	if a.cfg != nil {
		a.cfg.ABTesting = cfg
	}
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
