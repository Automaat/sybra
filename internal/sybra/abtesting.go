package sybra

import (
	"github.com/Automaat/sybra/internal/abtest"
)

func (a *App) initializeABTesting(cfg abtest.Config) {
	a.setBaseABTesting(cfg)
	a.setLiveABTesting(cfg)
}

func (a *App) baseABTestingConfig() abtest.Config {
	if v := a.baseABTesting.Load(); v != nil {
		return cloneABTestingConfig(v.(abtest.Config))
	}
	if a.cfg == nil {
		return abtest.Config{}
	}
	return cloneABTestingConfig(a.cfg.ABTesting)
}

func (a *App) abTestingConfig() abtest.Config {
	if v := a.liveABTesting.Load(); v != nil {
		return cloneABTestingConfig(v.(abtest.Config))
	}
	return a.baseABTestingConfig()
}

func (a *App) setBaseABTesting(cfg abtest.Config) {
	a.baseABTesting.Store(cloneABTestingConfig(cfg))
}

func (a *App) setLiveABTesting(cfg abtest.Config) {
	a.liveABTesting.Store(cloneABTestingConfig(cfg))
}

func cloneABTestingConfig(src abtest.Config) abtest.Config {
	return abtest.CloneConfig(src)
}
