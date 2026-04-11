package main

import (
	"context"
	"time"

	"github.com/Automaat/synapse/internal/poll"
	"github.com/Automaat/synapse/internal/todoist"
)

func (a *App) initTodoist(emit func(string, any)) {
	if !a.cfg.Todoist.Enabled || a.cfg.Todoist.APIToken == "" {
		return
	}
	tc := todoist.NewClient(a.cfg.Todoist.APIToken)
	a.todoistHandler = poll.NewTodoistHandler(a.tasks, a.taskSvc.CreateTask, tc, a.audit, a.logger, emit, a.cfg.Todoist)
	a.logger.Info("todoist.enabled", "project_id", a.cfg.Todoist.ProjectID)
}

// startTodoistLoop launches the poll goroutine if the handler is initialized.
func (a *App) startTodoistLoop(parent context.Context) {
	if a.todoistHandler == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	a.todoistCancel = cancel
	p := poll.New(a.todoistHandler, 15*time.Second, a.logger)
	a.wg.Go(func() { p.Run(ctx) })
}

// stopTodoistLoop cancels the running poll goroutine if any.
func (a *App) stopTodoistLoop() {
	if a.todoistCancel != nil {
		a.todoistCancel()
		a.todoistCancel = nil
	}
	a.todoistHandler = nil
}

// reloadTodoist tears down and (if enabled) re-creates the Todoist handler + poll loop.
func (a *App) reloadTodoist() {
	a.stopTodoistLoop()
	a.initTodoist(a.emit)
	a.startTodoistLoop(a.ctx)
}
