package main

import (
	"context"
	"path/filepath"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/watcher"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx      context.Context
	tasks    *task.Store
	agents   *agent.Manager
	watcher  *watcher.Watcher
	tasksDir string
}

func NewApp() *App {
	return &App{
		tasksDir: "tasks",
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	absDir, _ := filepath.Abs(a.tasksDir)
	store, _ := task.NewStore(absDir)
	a.tasks = store

	tm := tmux.NewManager()
	emit := func(event string, data any) {
		runtime.EventsEmit(ctx, event, data)
	}
	a.agents = agent.NewManager(ctx, tm, emit)

	w := watcher.New(absDir, emit)
	a.watcher = w
	_ = w.Start(ctx)
}

func (a *App) shutdown(_ context.Context) {
	a.agents.Shutdown()
}

func (a *App) ListTasks() ([]task.Task, error) {
	return a.tasks.List()
}

func (a *App) GetTask(id string) (task.Task, error) {
	return a.tasks.Get(id)
}

func (a *App) CreateTask(title, body, mode string) (task.Task, error) {
	return a.tasks.Create(title, body, mode)
}

func (a *App) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	return a.tasks.Update(id, updates)
}

func (a *App) StartAgent(taskID, mode, prompt string) (*agent.Agent, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	return a.agents.StartAgent(taskID, mode, prompt, t.AllowedTools)
}

func (a *App) StopAgent(agentID string) error {
	return a.agents.StopAgent(agentID)
}

func (a *App) ListAgents() []*agent.Agent {
	return a.agents.ListAgents()
}

func (a *App) GetAgentOutput(agentID string) ([]agent.StreamEvent, error) {
	ag, err := a.agents.GetAgent(agentID)
	if err != nil {
		return nil, err
	}
	return ag.Output(), nil
}
