//go:build darwin

// V3Services returns the v3 service slice for the existing v2 services.
// Darwin-gated because Wails v3 alpha pulls gtk3/webkit2gtk-4.1 cgo on
// Linux that CI runners do not have. See docs/migrations/wails-v3.md
// for the full plan.

package sybra

import "github.com/wailsapp/wails/v3/pkg/application"

// V3Services exposes the App and its 12 bound services as Wails v3
// application.Service values, in the same order as BindTargets so the
// v3 binary keeps method-name parity with the v2 path. Phase 5 cutover
// retires BindTargets.
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
	}
}
