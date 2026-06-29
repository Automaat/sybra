package sybra

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/todoist"
)

type todoistCoordinator struct {
	tasks    *task.Manager
	svc      *TaskService
	auditLog *audit.Logger
	logger   *slog.Logger
	emit     func(string, any)
	cfg      *config.Config
	wg       *sync.WaitGroup
	handler  *poll.TodoistHandler
	cancel   context.CancelFunc
}

func newTodoistCoordinator(
	tasks *task.Manager,
	svc *TaskService,
	auditLog *audit.Logger,
	logger *slog.Logger,
	emit func(string, any),
	cfg *config.Config,
	wg *sync.WaitGroup,
) *todoistCoordinator {
	return &todoistCoordinator{
		tasks:    tasks,
		svc:      svc,
		auditLog: auditLog,
		logger:   logger,
		emit:     emit,
		cfg:      cfg,
		wg:       wg,
	}
}

// newTodoistHandler constructs a poll.TodoistHandler using a TaskService.
func newTodoistHandler(
	tasks *task.Manager,
	svc *TaskService,
	client *todoist.Client,
	al *audit.Logger,
	logger *slog.Logger,
	emit func(string, any),
	cfg config.TodoistConfig,
) *poll.TodoistHandler {
	return poll.NewTodoistHandler(tasks, svc.CreateTask, client, al, logger, emit, cfg)
}

func (c *todoistCoordinator) init() {
	if !c.cfg.Todoist.Enabled || c.cfg.Todoist.APIToken == "" {
		return
	}
	tc := todoist.NewClient(c.cfg.Todoist.APIToken)
	c.handler = poll.NewTodoistHandler(c.tasks, c.svc.CreateTask, tc, c.auditLog, c.logger, c.emit, c.cfg.Todoist)
	c.logger.Info("todoist.enabled", "project_id", c.cfg.Todoist.ProjectID)
}

func (c *todoistCoordinator) start(parent context.Context) {
	if c.handler == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	p := poll.New(c.handler, 15*time.Second, c.logger)
	c.wg.Go(func() { p.Run(ctx) })
}

func (c *todoistCoordinator) stop() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.handler = nil
}

func (c *todoistCoordinator) reload(parent context.Context) {
	c.stop()
	c.init()
	c.start(parent)
}

func (c *todoistCoordinator) enabled() bool {
	return c != nil && c.handler != nil
}

func (c *todoistCoordinator) syncNow() error {
	if !c.enabled() {
		return fmt.Errorf("todoist integration not enabled")
	}
	c.handler.PollAndSync()
	return nil
}

func (a *App) initTodoist(emit func(string, any)) {
	a.todoist = newTodoistCoordinator(a.tasks, a.taskSvc, a.audit, a.logger, emit, a.cfg, &a.wg)
	a.todoist.init()
}

// startTodoistLoop launches the poll goroutine if the handler is initialized.
func (a *App) startTodoistLoop(parent context.Context) {
	if a.todoist == nil {
		return
	}
	a.todoist.start(parent)
}

// reloadTodoist tears down and (if enabled) re-creates the Todoist handler + poll loop.
func (a *App) reloadTodoist() {
	if a.todoist == nil {
		a.initTodoist(a.emit)
	}
	a.todoist.reload(a.ctx)
}
