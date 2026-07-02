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
	mu       sync.Mutex
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

func (c *todoistCoordinator) init() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
}

func (c *todoistCoordinator) initLocked() {
	if !c.cfg.Todoist.Enabled || c.cfg.Todoist.APIToken == "" {
		return
	}
	tc := todoist.NewClient(c.cfg.Todoist.APIToken)
	c.handler = poll.NewTodoistHandler(c.tasks, c.svc.CreateTask, tc, c.auditLog, c.logger, c.emit, c.cfg.Todoist)
	c.logger.Info("todoist.enabled", "project_id", c.cfg.Todoist.ProjectID)
}

func (c *todoistCoordinator) start(parent context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startLocked(parent)
}

func (c *todoistCoordinator) startLocked(parent context.Context) {
	if c.handler == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	p := poll.New(c.handler, 15*time.Second, c.logger)
	c.wg.Go(func() { p.Run(ctx) })
}

func (c *todoistCoordinator) stopLocked() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.handler = nil
}

func (c *todoistCoordinator) reload(parent context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	c.initLocked()
	c.startLocked(parent)
}

func (c *todoistCoordinator) enabled() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handler != nil
}

func (c *todoistCoordinator) syncNow() error {
	if c == nil {
		return fmt.Errorf("todoist integration not enabled")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler == nil {
		return fmt.Errorf("todoist integration not enabled")
	}
	c.handler.PollAndSync(context.Background())
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
