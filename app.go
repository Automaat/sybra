package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/spotlight"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/watcher"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx          context.Context
	tasks        *task.Store
	projects     *project.Store
	agents       *agent.Manager
	tmux         *tmux.Manager
	watcher      *watcher.Watcher
	tasksDir     string
	skillsDir    string
	repoDir      string
	worktreesDir string
	logger       *slog.Logger
	logDir       string
}

func NewApp(logger *slog.Logger, cfg *config.Config) *App {
	return &App{
		tasksDir:     cfg.TasksDir,
		skillsDir:    cfg.SkillsDir,
		repoDir:      cfg.RepoDir,
		worktreesDir: cfg.WorktreesDir,
		logger:       logger,
		logDir:       cfg.Logging.Dir,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.logger.Info("app.starting")

	store, _ := task.NewStore(a.tasksDir)
	a.tasks = store

	projStore, _ := project.NewStore(
		filepath.Join(config.HomeDir(), "projects"),
		filepath.Join(config.HomeDir(), "clones"),
	)
	a.projects = projStore

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
	go a.orchestratorLoop(ctx)
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
	t, err := a.tasks.Update(id, updates)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusPlanning {
		a.logger.Info("auto-plan.start", "task_id", t.ID, "title", t.Title)
		go func() {
			if planErr := a.PlanTask(t.ID); planErr != nil {
				a.logger.Error("auto-plan.failed", "task_id", t.ID, "err", planErr)
			}
		}()
	}
	if t.Status == task.StatusInProgress {
		a.logger.Info("auto-implement.start", "task_id", t.ID, "title", t.Title)
		go func() {
			if _, err := a.StartAgent(t.ID, t.AgentMode, "Implement this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
				a.logger.Error("auto-implement.failed", "task_id", t.ID, "err", err)
			}
		}()
	}
	return t, nil
}

func (a *App) DeleteTask(id string) error {
	a.logger.Info("task.delete", "task_id", id)
	if err := a.tasks.Delete(id); err != nil {
		a.logger.Error("task.delete.failed", "task_id", id, "err", err)
		return err
	}
	return nil
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

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := a.agents.StartAgentInDir(taskID, t.Title, mode, fullPrompt, t.AllowedTools, dir)
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

func (a *App) prepareWorktree(t task.Task) (string, error) {
	proj, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return "", fmt.Errorf("get project: %w", err)
	}

	if err := project.FetchOrigin(proj.ClonePath); err != nil {
		a.logger.Warn("worktree.fetch", "project", proj.ID, "err", err)
	}

	branch, err := project.DefaultBranch(proj.ClonePath)
	if err != nil {
		return "", fmt.Errorf("default branch: %w", err)
	}

	wtPath := filepath.Join(a.worktreesDir, t.DirName())
	if _, statErr := os.Stat(wtPath); statErr == nil {
		return wtPath, nil
	}
	wtBranch := "synapse/" + t.DirName()
	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, branch); err != nil {
		// Branch may exist from a previous run — try checkout instead of new branch
		if errRe := project.CreateWorktreeExisting(proj.ClonePath, wtPath, wtBranch); errRe != nil {
			return "", fmt.Errorf("create worktree: %w (retry: %w)", err, errRe)
		}
	}

	a.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)
	return wtPath, nil
}

func (a *App) cleanupWorktree(ag *agent.Agent) {
	if ag.TaskID == "" {
		return
	}
	t, err := a.tasks.Get(ag.TaskID)
	if err != nil || t.ProjectID == "" {
		return
	}
	wtPath := filepath.Join(a.worktreesDir, t.DirName())
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	proj, err := a.projects.Get(t.ProjectID)
	if err != nil {
		return
	}

	if err := project.RemoveWorktree(proj.ClonePath, wtPath); err != nil {
		a.logger.Error("worktree.cleanup", "path", wtPath, "err", err)
	} else {
		a.logger.Info("worktree.cleaned", "path", wtPath)
	}
}

func (a *App) ListProjects() ([]project.Project, error) {
	return a.projects.List()
}

func (a *App) GetProject(id string) (project.Project, error) {
	return a.projects.Get(id)
}

func (a *App) CreateProject(url string) (project.Project, error) {
	a.logger.Info("project.create", "url", url)
	p, err := a.projects.Create(url)
	if err != nil {
		a.logger.Error("project.create.failed", "url", url, "err", err)
		return p, err
	}
	a.logger.Info("project.created", "id", p.ID, "url", url)
	return p, nil
}

func (a *App) DeleteProject(id string) error {
	a.logger.Info("project.delete", "id", id)
	if err := a.projects.Delete(id); err != nil {
		a.logger.Error("project.delete.failed", "id", id, "err", err)
		return err
	}
	return nil
}

func (a *App) ListWorktrees(projectID string) ([]project.Worktree, error) {
	proj, err := a.projects.Get(projectID)
	if err != nil {
		return nil, err
	}
	return project.ListWorktrees(proj.ClonePath)
}

func (a *App) OpenInTerminal(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(a.worktreesDir)) {
		return fmt.Errorf("path not within worktrees directory")
	}
	if info, err := os.Stat(clean); err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a valid directory")
	}
	return openDirInGhostty(clean)
}

func (a *App) OpenInEditor(path string) error {
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(a.worktreesDir)) {
		return fmt.Errorf("path not within worktrees directory")
	}
	if info, err := os.Stat(clean); err != nil || !info.IsDir() {
		return fmt.Errorf("path is not a valid directory")
	}
	return exec.Command("zed", clean).Start()
}

func openDirInGhostty(dir string) error {
	script := fmt.Sprintf(`tell application "Ghostty"
	activate
	set synapseWins to (every window whose name contains "Synapse:")
	set winCount to (count of synapseWins)
	set cfg to new surface configuration
	set command of cfg to "/bin/zsh -lic 'cd %s && exec zsh'"
	if winCount > 0 then
		new tab in (item 1 of synapseWins) with configuration cfg
	else
		new window with configuration cfg
	end if
end tell`, dir)
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w: %s", err, string(out))
	}
	return nil
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
	a.logger.Info("tmux.kill", "session", name)
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

func (a *App) orchestratorLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.maybeStartOrchestrator()
			a.maybeDispatchTasks()
		}
	}
}

func (a *App) maybeStartOrchestrator() {
	if a.tmux.SessionExists(orchestratorSession) {
		return
	}

	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	hasActive := false
	for i := range tasks {
		switch tasks[i].Status {
		case task.StatusPlanning, task.StatusPlanReview, task.StatusInProgress, task.StatusInReview:
			hasActive = true
		}
		if hasActive {
			break
		}
	}
	if !hasActive {
		return
	}

	a.logger.Info("orchestrator.auto-start", "reason", "active tasks detected")
	if err := a.StartOrchestrator(); err != nil {
		a.logger.Error("orchestrator.auto-start.failed", "err", err)
	}
}

const maxConcurrentAgents = 3

func (a *App) maybeDispatchTasks() {
	running := 0
	for _, ag := range a.agents.ListAgents() {
		if ag.State == agent.StateRunning {
			running++
		}
	}
	if running >= maxConcurrentAgents {
		return
	}

	tasks, err := a.tasks.List()
	if err != nil {
		return
	}

	var candidates []task.Task
	for i := range tasks {
		if tasks[i].Status != task.StatusTodo {
			continue
		}
		if tasks[i].AgentMode == "" || len(tasks[i].Tags) == 0 {
			continue
		}
		if slices.Contains(tasks[i].Tags, "large") {
			continue
		}
		candidates = append(candidates, tasks[i])
	}
	if len(candidates) == 0 {
		return
	}

	slices.SortFunc(candidates, func(a, b task.Task) int {
		pa, pb := taskPriority(a.Tags), taskPriority(b.Tags)
		if pa != pb {
			return pa - pb
		}
		sa, sb := taskSize(a.Tags), taskSize(b.Tags)
		return sa - sb
	})

	slots := maxConcurrentAgents - running
	for i := range min(slots, len(candidates)) {
		t := candidates[i]
		a.logger.Info("auto-dispatch", "task_id", t.ID, "title", t.Title)
		if _, err := a.UpdateTask(t.ID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			a.logger.Error("auto-dispatch.failed", "task_id", t.ID, "err", err)
		}
	}
}

func taskPriority(tags []string) int {
	for _, t := range tags {
		switch t {
		case "urgent":
			return 0
		case "high":
			return 1
		case "normal":
			return 2
		case "low":
			return 3
		}
	}
	return 2
}

func taskSize(tags []string) int {
	for _, t := range tags {
		switch t {
		case "small":
			return 0
		case "medium":
			return 1
		}
	}
	return 1
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

	triageTools := []string{"Bash", "Read", "Skill"}
	ag, err := a.agents.StartAgentInDir(t.ID, "triage:"+t.Title, "headless", prompt, triageTools, dir)
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

	if strings.HasPrefix(ag.Name, "triage:") {
		a.logger.Info("eval.skip", "agent_id", ag.ID, "name", ag.Name, "reason", "system_agent")
		return
	}

	if strings.HasPrefix(ag.Name, "plan:") {
		a.logger.Info("plan.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		body := ""
		if t, err := a.tasks.Get(ag.TaskID); err == nil {
			body = t.Body
		}
		if resultContent != "" {
			body += "\n\n## Plan\n\n" + resultContent
		}
		if _, err := a.tasks.Update(ag.TaskID, map[string]any{
			"status": string(task.StatusPlanReview),
			"body":   body,
		}); err != nil {
			a.logger.Error("plan.update", "task_id", ag.TaskID, "err", err)
		}
		return
	}

	if strings.HasPrefix(ag.Name, "eval:") {
		a.logger.Info("eval.done", "agent_id", ag.ID, "name", ag.Name)
		if t, err := a.tasks.Get(ag.TaskID); err == nil && t.Status == task.StatusDone {
			a.cleanupWorktree(ag)
		}
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

func (a *App) PlanTask(id string) error {
	t, err := a.tasks.Get(id)
	if err != nil {
		return err
	}

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			return fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	prompt := fmt.Sprintf(
		"Plan task %s using /synapse-plan skill. Get the task with synapse-cli, "+
			"analyze the codebase, and produce a detailed implementation plan. "+
			"Do NOT implement anything.", t.ID)

	a.logger.Info("plan.start", "task_id", t.ID, "title", t.Title)

	var ag *agent.Agent
	if dir != "" {
		ag, err = a.agents.StartAgentInDir(t.ID, "plan:"+t.Title, "headless", prompt, nil, dir)
	} else {
		ag, err = a.agents.StartAgentInDir(t.ID, "plan:"+t.Title, "headless", prompt, nil, config.HomeDir())
	}
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("plan.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) ApprovePlan(id string) (task.Task, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Status != task.StatusPlanReview {
		return task.Task{}, fmt.Errorf("task %s status is %q, expected 'plan-review'", id, t.Status)
	}
	a.logger.Info("plan.approve", "task_id", id, "title", t.Title)
	return a.tasks.Update(id, map[string]any{
		"status": string(task.StatusTodo),
	})
}

func (a *App) RejectPlan(id, feedback string) (task.Task, error) {
	t, err := a.tasks.Get(id)
	if err != nil {
		return task.Task{}, err
	}
	if t.Status != task.StatusPlanReview {
		return task.Task{}, fmt.Errorf("task %s status is %q, expected 'plan-review'", id, t.Status)
	}
	a.logger.Info("plan.reject", "task_id", id, "title", t.Title, "has_feedback", feedback != "")
	body := t.Body
	if feedback != "" {
		body += "\n\n## Plan Feedback\n\n" + feedback
	}
	return a.tasks.Update(id, map[string]any{
		"status": string(task.StatusPlanning),
		"body":   body,
	})
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
	spotlight.OnSubmit(func(title, projectID string) {
		a.logger.Info("spotlight.submit", "title", title, "project", projectID)
		go func() {
			t, err := a.CreateTask(title, "", "headless")
			if err != nil {
				a.logger.Error("spotlight.create", "err", err)
				return
			}
			if projectID != "" {
				if _, err := a.UpdateTask(t.ID, map[string]any{"project_id": projectID}); err != nil {
					a.logger.Error("spotlight.project", "err", err)
				}
			}
		}()
	})

	if err := spotlight.Register(func() {
		projectsJSON := "[]"
		if projects, err := a.ListProjects(); err == nil {
			if data, err := json.Marshal(projects); err == nil {
				projectsJSON = string(data)
			}
		}
		spotlight.ShowPanel(projectsJSON)
	}); err != nil {
		a.logger.Error("spotlight.register", "err", err)
		return
	}
	a.logger.Info("spotlight.registered", "hotkey", "ctrl+space")
}
