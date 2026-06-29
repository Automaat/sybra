package sybra

import (
	"log/slog"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
)

type triageCoordinator struct {
	tasks          *task.Manager
	projects       *project.Store
	auditLog       *audit.Logger
	logger         *slog.Logger
	cfg            *config.Config
	providerHealth *provider.Checker
	handler        *poll.TriageHandler
}

func newTriageCoordinator(
	tasks *task.Manager,
	projects *project.Store,
	auditLog *audit.Logger,
	logger *slog.Logger,
	cfg *config.Config,
	providerHealth *provider.Checker,
) *triageCoordinator {
	return &triageCoordinator{
		tasks:          tasks,
		projects:       projects,
		auditLog:       auditLog,
		logger:         logger,
		cfg:            cfg,
		providerHealth: providerHealth,
	}
}

func (c *triageCoordinator) init() {
	if !c.cfg.Triage.Enabled {
		return
	}
	c.handler = poll.NewTriageHandler(c.tasks, c.projects, c.auditLog, c.logger, &c.cfg.Triage)
	c.handler.SetProviderGate(c.providerHealth)
	c.logger.Info("triage.enabled", "poll_seconds", c.cfg.Triage.PollSeconds, "model", c.cfg.Triage.Model)
}

func (c *triageCoordinator) poller() *poll.TriageHandler {
	if c == nil {
		return nil
	}
	return c.handler
}

// initTriage constructs the background auto-triage handler if enabled.
// The handler is registered with the poll hub in startPollHub alongside
// renovate and the issues fetcher.
func (a *App) initTriage() {
	a.triage = newTriageCoordinator(a.tasks, a.projects, a.audit, a.logger, a.cfg, a.providerHealth)
	a.triage.init()
}
