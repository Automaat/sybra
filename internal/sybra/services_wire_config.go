package sybra

import (
	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/workflow"
)

func (a *App) wireConfigService() {
	cfg := a.currentConfig()
	a.configSvc.cfg = cfg
	a.configSvc.persisted = cloneConfig(cfg)
	a.configSvc.publishConfig = func(next *config.Config) { a.activeCfg.Store(next) }
	a.configSvc.logLevel = a.logLevel
	a.configSvc.notifier = a.notifier
	a.configSvc.agents = a.agents
	a.configSvc.limits = a.limits
	a.configSvc.providerHealth = a.providerHealth
	a.configSvc.workflowEngine = a.workflowEngine
	a.configSvc.logger = a.logger
	a.configSvc.policy = a.limitPolicy
	a.configSvc.applyABTestingBase = func(cfg abtest.Config) {
		a.setBaseABTesting(cfg)
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
	}
	// Read a.routingSvc lazily: it is constructed later in startRoutingService,
	// so binding the pointer now would capture nil. ApplyPersistedOverlay itself
	// no-ops when routing is disabled or no overlay was ever saved.
	a.configSvc.applyCommitSigning = func(raw string) {
		policy := project.NormalizeSigningPolicy(raw)
		if a.projects != nil {
			a.projects.SetSigningPolicy(policy)
		}
		if a.reviewer != nil {
			a.reviewer.SetSigningPolicy(policy)
		}
		workflow.SetDefaultCommitSignFlags(policy.CommitFlags(a.ctx))
	}
	a.configSvc.reapplyRouting = func() {
		if a.routingSvc != nil {
			a.routingSvc.ApplyPersistedOverlay()
		}
	}
}
