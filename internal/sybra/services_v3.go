//go:build darwin

// V3Services returns the Wails v3 service slice for the App.
//
// Darwin-gated because the v3 alpha runtime needs gtk3/webkit2gtk-4.1
// cgo on Linux that CI runners do not have. The v2 BindTargets is gone;
// only the desktop binary (also darwin-only) calls into this.

package sybra

import "github.com/wailsapp/wails/v3/pkg/application"

// V3Services exposes the App and its 15 services as v3 application.Service
// values. Order matches the historical v2 BindTargets so generated bindings
// keep stable IDs across migrations — new services must only be appended.
func (a *App) V3Services() []application.Service {
	return []application.Service{
		application.NewService(a),
		application.NewService(a.taskSvc),
		application.NewService(a.planSvc),
		application.NewService(a.agentSvc),
		application.NewService(a.orchSvc),
		application.NewService(a.projectSvc),
		application.NewService(a.loopAgentSvc),
		application.NewService(a.configSvc),
		application.NewService(a.intgSvc),
		application.NewService(a.statsSvc),
		application.NewService(a.reviewSvc),
		application.NewService(a.workflowSvc),
		application.NewService(a.infoSvc),
		application.NewService(a.browserSvc),
		application.NewService(a.learningSvc),
		application.NewService(a.promptLabSvc),
	}
}
