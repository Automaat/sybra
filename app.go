package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/spotlight"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/watcher"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx       context.Context
	tasks     *task.Store
	agents    *agent.Manager
	tmux      *tmux.Manager
	watcher   *watcher.Watcher
	tasksDir  string
	skillsDir string
	repoDir   string
	logger    *slog.Logger
	logDir    string
}

func NewApp(logger *slog.Logger, logDir, tasksDir, skillsDir, repoDir string) *App {
	return &App{
		tasksDir:  tasksDir,
		skillsDir: skillsDir,
		repoDir:   repoDir,
		logger:    logger,
		logDir:    logDir,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.logger.Info("app.starting")

	store, _ := task.NewStore(a.tasksDir)
	a.tasks = store

	a.tmux = tmux.NewManager()
	emit := func(event string, data any) {
		runtime.EventsEmit(ctx, event, data)
	}
	a.agents = agent.NewManager(ctx, a.tmux, emit, a.logger, a.logDir)
	a.agents.SetOnComplete(a.handleAgentComplete)

	w := watcher.New(a.tasksDir, emit, a.logger)
	a.watcher = w
	_ = w.Start(ctx)

	a.syncSkills()
	a.RegisterSpotlightHotkey()
	a.logger.Info("app.started")
}

func (a *App) shutdown(_ context.Context) {
	a.logger.Info("app.stopping")
	a.agents.Shutdown()
	a.logger.Info("app.stopped")
}

func (a *App) ListTasks() ([]task.Task, error) {
	return a.tasks.List()
}

func (a *App) GetTask(id string) (task.Task, error) {
	return a.tasks.Get(id)
}

func (a *App) CreateTask(title, body, mode string) (task.Task, error) {
	t, err := a.tasks.Create(title, body, mode)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusNew {
		a.logger.Info("auto-triage.start", "task_id", t.ID, "title", t.Title)
		go func() {
			if triageErr := a.TriageTask(t.ID); triageErr != nil {
				a.logger.Error("auto-triage.failed", "task_id", t.ID, "err", triageErr)
			}
		}()
	}
	return t, nil
}

func (a *App) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	return a.tasks.Update(id, updates)
}

func (a *App) DeleteTask(id string) error {
	return a.tasks.Delete(id)
}

func (a *App) StartAgent(taskID, mode, prompt string) (*agent.Agent, error) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return nil, err
	}
	if t.Status != task.StatusInProgress {
		if _, err := a.tasks.Update(taskID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			a.logger.Error("task.auto-status", "task_id", taskID, "err", err)
		}
	}
	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := a.agents.StartAgent(taskID, t.Title, mode, fullPrompt, t.AllowedTools)
	if err != nil {
		return nil, err
	}
	if err := a.tasks.AddRun(taskID, task.AgentRun{
		AgentID:   ag.ID,
		Mode:      mode,
		State:     string(agent.StateRunning),
		StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", taskID, "err", err)
	}
	return ag, nil
}

func (a *App) StopAgent(agentID string) error {
	return a.agents.StopAgent(agentID)
}

func (a *App) ListAgents() []*agent.Agent {
	return a.agents.ListAgents()
}

func (a *App) DiscoverAgents() []*agent.Agent {
	return a.agents.DiscoverAgents()
}

func (a *App) CaptureAgentPane(agentID string) (string, error) {
	return a.agents.CapturePane(agentID)
}

func (a *App) AttachAgent(agentID string) error {
	ag, err := a.agents.GetAgent(agentID)
	if err != nil {
		return err
	}
	if ag.TmuxSession == "" {
		return fmt.Errorf("agent %s has no tmux session", agentID)
	}
	title := ag.Name
	if title == "" {
		title = ag.TaskID
	}
	return openTmuxInGhostty(ag.TmuxSession, title)
}

func (a *App) ListTmuxSessions() ([]tmux.SessionInfo, error) {
	return a.tmux.ListSessions()
}

func (a *App) KillTmuxSession(name string) error {
	return a.tmux.KillSession(name)
}

func (a *App) AttachTmuxSession(name string) error {
	return openTmuxInGhostty(name, name)
}

func openTmuxInGhostty(session, tabTitle string) error {
	label := "Synapse: " + tabTitle
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Synapse:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic 'printf \"\\033]0;%s\\007\"; exec tmux attach -t %s'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, label, session)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
}

func (a *App) syncSkills() {
	repoDir := a.repoDir
	if repoDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			a.logger.Error("skills.sync.skip", "reason", "no repo_dir and cannot get cwd")
			return
		}
		repoDir = cwd
		a.logger.Info("skills.sync.fallback_cwd", "dir", cwd)
	}

	home := config.HomeDir()
	a.logger.Info("skills.sync.start", "src", repoDir, "dst", home)

	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	skillsDst := filepath.Join(home, ".claude", "skills")
	a.syncDir(skillsSrc, skillsDst)

	claudeSrc := filepath.Join(repoDir, "orchestrator", "CLAUDE.md")
	claudeDst := filepath.Join(home, "CLAUDE.md")
	a.syncFile(claudeSrc, claudeDst)

	a.logger.Info("skills.sync.done")
}

func (a *App) syncDir(src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		a.logger.Debug("sync.skip", "src", src, "reason", err)
		return
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		a.logger.Error("sync.mkdir", "dst", dst, "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		a.syncFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
	}
}

func (a *App) syncFile(src, dst string) {
	data, err := os.ReadFile(src)
	if err != nil {
		a.logger.Debug("sync.read.skip", "src", src, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		a.logger.Error("sync.mkdir", "dst", dst, "err", err)
		return
	}
	if err := os.WriteFile(dst, data, fs.FileMode(0o644)); err != nil {
		a.logger.Error("sync.write", "dst", dst, "err", err)
		return
	}
	a.logger.Info("sync.copied", "file", filepath.Base(dst))
}

const orchestratorSession = "synapse-orchestrator"

func (a *App) StartOrchestrator() error {
	if a.tmux.SessionExists(orchestratorSession) {
		return fmt.Errorf("orchestrator already running")
	}
	if err := a.tmux.CreateSessionInDir(orchestratorSession, "claude", config.HomeDir()); err != nil {
		return fmt.Errorf("create orchestrator session: %w", err)
	}
	a.logger.Info("orchestrator.started")
	runtime.EventsEmit(a.ctx, "orchestrator:state", "running")
	return nil
}

func (a *App) StopOrchestrator() error {
	if err := a.tmux.KillSession(orchestratorSession); err != nil {
		return fmt.Errorf("stop orchestrator: %w", err)
	}
	a.logger.Info("orchestrator.stopped")
	runtime.EventsEmit(a.ctx, "orchestrator:state", "stopped")
	return nil
}

func (a *App) IsOrchestratorRunning() bool {
	return a.tmux.SessionExists(orchestratorSession)
}

func (a *App) CaptureOrchestratorPane() (string, error) {
	if !a.tmux.SessionExists(orchestratorSession) {
		return "", fmt.Errorf("orchestrator not running")
	}
	return a.tmux.CapturePaneOutput(orchestratorSession)
}

func (a *App) AttachOrchestrator() error {
	if !a.tmux.SessionExists(orchestratorSession) {
		return fmt.Errorf("orchestrator not running")
	}
	return openTmuxInGhostty(orchestratorSession, "Orchestrator")
}

func (a *App) TriageTask(id string) error {
	t, err := a.tasks.Get(id)
	if err != nil {
		return err
	}

	dir := config.HomeDir()
	prompt := fmt.Sprintf("Triage task %s using /synapse-triage skill. Get the task with synapse-cli, analyze it, assign tags, mode, and update status.", t.ID)

	a.logger.Info("triage.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := a.agents.StartAgentInDir(t.ID, "triage:"+t.Title, "headless", prompt, nil, dir)
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("triage.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) handleAgentComplete(ag *agent.Agent) {
	var resultContent string
	for _, ev := range ag.Output() {
		if ev.Type == "result" {
			resultContent = ev.Content
		}
	}

	// Persist run result to task file
	truncatedResult := resultContent
	if len(truncatedResult) > 2000 {
		truncatedResult = truncatedResult[:2000] + "\n... (truncated)"
	}
	if err := a.tasks.UpdateRun(ag.TaskID, ag.ID, map[string]any{
		"state":    string(ag.State),
		"cost_usd": ag.CostUSD,
		"result":   truncatedResult,
	}); err != nil {
		a.logger.Error("task.update-run", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
	}

	if strings.HasPrefix(ag.Name, "triage:") || strings.HasPrefix(ag.Name, "eval:") {
		a.logger.Info("eval.skip", "agent_id", ag.ID, "name", ag.Name, "reason", "system_agent")
		return
	}

	go func() {
		if err := a.EvaluateTask(ag.TaskID, resultContent); err != nil {
			a.logger.Error("auto-evaluate.failed", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
		}
	}()
}

func (a *App) EvaluateTask(taskID, agentResult string) error {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return err
	}

	if t.Status != task.StatusInProgress {
		a.logger.Info("eval.skip", "task_id", t.ID, "status", string(t.Status), "reason", "not_in_progress")
		return nil
	}

	dir := config.HomeDir()

	truncated := agentResult
	if len(truncated) > 4000 {
		truncated = truncated[:4000] + "\n... (truncated)"
	}

	prompt := fmt.Sprintf(
		"Evaluate task %s using /synapse-evaluate skill. Get the task with synapse-cli, "+
			"review the agent result below, and update the task status.\n\n"+
			"## Agent Result\n\n%s", t.ID, truncated)

	a.logger.Info("eval.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := a.agents.StartAgentInDir(t.ID, "eval:"+t.Title, "headless", prompt, nil, dir)
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("eval.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) GetAgentOutput(agentID string) ([]agent.StreamEvent, error) {
	ag, err := a.agents.GetAgent(agentID)
	if err != nil {
		return nil, err
	}
	return ag.Output(), nil
}

func (a *App) FetchReviews() (github.ReviewSummary, error) {
	return github.FetchReviews()
}

func (a *App) RegisterSpotlightHotkey() {
	spotlight.OnSubmit(func(text string) {
		a.logger.Info("spotlight.submit", "text", text)
		go func() {
			if _, err := a.CreateTask(text, "", "headless"); err != nil {
				a.logger.Error("spotlight.create", "err", err)
			}
		}()
	})

	if err := spotlight.Register(func() {
		spotlight.ShowPanel(680, 72)
	}); err != nil {
		a.logger.Error("spotlight.register", "err", err)
		return
	}
	a.logger.Info("spotlight.registered", "hotkey", "ctrl+space")
}
