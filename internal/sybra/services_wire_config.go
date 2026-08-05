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
	a.configSvc.subscribe(configSubscriber{
		Name:  "commit_signing",
		Paths: []string{"agent.commit_signing"},
		Apply: func(cfg config.Config) {
			a.applyCommitSigning(cfg.CommitSigning())
		},
	})
	// Read a.routingSvc lazily: it is constructed later in startRoutingService,
	// so binding the pointer now would capture nil. ApplyPersistedOverlay itself
	// no-ops when routing is disabled or no overlay was ever saved.
	a.configSvc.reapplyRouting = func() {
		if a.routingSvc != nil {
			a.routingSvc.ApplyPersistedOverlay()
		}
	}
}

// applyCommitSigning pushes the posture to every sink that caches it outside
// the config struct. Each was found only after the previous one was fixed and
// re-tested against a live server, which is why they are listed together here
// rather than discovered one at a time.
func (a *App) applyCommitSigning(raw string) {
	policy := project.NormalizeSigningPolicy(raw)
	if a.projects != nil {
		a.projects.SetSigningPolicy(policy)
	}
	if a.reviewer != nil {
		a.reviewer.SetSigningPolicy(policy)
	}
	if a.humanReview != nil {
		a.humanReview.SetSigningPolicy(policy)
	}
	// The synced skill bundle is the file a fix-review agent actually loads,
	// so leaving it on the startup posture makes the dispatched prompt and the
	// skill it invokes disagree about -S.
	a.syncSkillsBundle(policy)
	workflow.SetDefaultCommitSignFlags(policy.CommitFlags(a.ctx))
}
