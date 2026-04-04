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
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/notification"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/spotlight"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/tmux"
	"github.com/Automaat/synapse/internal/watcher"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	tasks        *task.Store
	projects     *project.Store
	agents       *agent.Manager
	tmux         *tmux.Manager
	watcher      *watcher.Watcher
	notifier     *notification.Emitter
	audit        *audit.Logger
	tasksDir     string
	skillsDir    string
	repoDir      string
	worktreesDir string
	logger       *slog.Logger
	logDir       string
	auditDir     string
	prTracker    *github.IssueTracker
}

func NewApp(logger *slog.Logger, cfg *config.Config) *App {
	return &App{
		tasksDir:     cfg.TasksDir,
		skillsDir:    cfg.SkillsDir,
		repoDir:      cfg.RepoDir,
		worktreesDir: cfg.WorktreesDir,
		logger:       logger,
		logDir:       cfg.Logging.Dir,
		auditDir:     cfg.AuditDir(),
	}
}

func (a *App) startup(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.logger.Info("app.starting")

	al, err := audit.NewLogger(a.auditDir)
	if err != nil {
		a.logger.Error("audit.init", "err", err)
	}
	a.audit = al
	if err := audit.Cleanup(a.auditDir, 30); err != nil {
		a.logger.Error("audit.cleanup", "err", err)
	}

	store, err := task.NewStore(a.tasksDir)
	if err != nil {
		a.logger.Error("task.store.init", "err", err)
		runtime.Quit(ctx)
		return
	}
	a.tasks = store

	projStore, err := project.NewStore(
		filepath.Join(config.HomeDir(), "projects"),
		filepath.Join(config.HomeDir(), "clones"),
	)
	if err != nil {
		a.logger.Error("project.store.init", "err", err)
		runtime.Quit(ctx)
		return
	}
	a.projects = projStore

	a.tmux = tmux.NewManager()
	emit := func(event string, data any) {
		runtime.EventsEmit(ctx, event, data)
	}
	a.notifier = notification.New(emit)
	a.agents = agent.NewManager(ctx, a.tmux, emit, a.logger, a.logDir)
	a.agents.SetOnComplete(a.handleAgentComplete)

	w := watcher.New(a.tasksDir, emit, a.logger)
	a.watcher = w
	if err := w.Start(ctx); err != nil {
		a.logger.Error("watcher.start", "err", err)
	}

	a.prTracker = github.NewIssueTracker(30 * time.Minute)
	a.reconnectAgents()
	a.cleanStaleRuns()
	a.restartStaleInProgress()
	a.syncSkills()
	a.RegisterSpotlightHotkey()
	a.wg.Go(func() { a.orchestratorLoop(ctx) })
	a.wg.Go(func() { a.prPollLoop(ctx) })
	a.logger.Info("app.started")
}

func (a *App) shutdown(_ context.Context) {
	a.logger.Info("app.stopping")
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.agents.Shutdown()
	if a.audit != nil {
		_ = a.audit.Close()
	}
	a.logger.Info("app.stopped")
}

func (a *App) logAudit(eventType, taskID, agentID string, data map[string]any) {
	if a.audit == nil {
		return
	}
	if err := a.audit.Log(audit.Event{
		Type:    eventType,
		TaskID:  taskID,
		AgentID: agentID,
		Data:    data,
	}); err != nil {
		a.logger.Error("audit.log", "type", eventType, "err", err)
	}
}

func (a *App) reconnectAgents() {
	tasks, err := a.tasks.List()
	if err != nil {
		a.logger.Warn("reconnect.tasks", "err", err)
		return
	}

	var infos []agent.TaskInfo
	for i := range tasks {
		if tasks[i].Status == task.StatusInProgress {
			infos = append(infos, agent.TaskInfo{ID: tasks[i].ID, Title: tasks[i].Title})
		}
	}

	n := a.agents.ReconnectSessions(infos)
	if n > 0 {
		a.logger.Info("reconnect.done", "count", n)
	}
}

// cleanStaleRuns marks agent_runs still showing "running" as "stopped" if no
// matching in-memory agent exists. Fixes leftover state from crashes/restarts.
func (a *App) cleanStaleRuns() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		for j := range tasks[i].AgentRuns {
			run := &tasks[i].AgentRuns[j]
			if run.State != string(agent.StateRunning) {
				continue
			}
			if a.agents.HasRunningAgentForTask(tasks[i].ID) {
				continue
			}
			a.logger.Info("stale-run.cleanup", "task_id", tasks[i].ID, "agent_id", run.AgentID)
			_ = a.tasks.UpdateRun(tasks[i].ID, run.AgentID, map[string]any{
				"state":  string(agent.StateStopped),
				"result": "stale: marked stopped on startup",
			})
		}
	}
}

// restartStaleInProgress finds in-progress tasks with no running agent and
// restarts them. Covers headless agents lost during app restart.
func (a *App) restartStaleInProgress() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Status != task.StatusInProgress {
			continue
		}
		if a.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		if slices.Contains(t.Tags, "review") {
			continue
		}
		a.logger.Info("restart-stale.start", "task_id", t.ID, "title", t.Title)
		taskID := t.ID
		mode := t.AgentMode
		a.wg.Go(func() {
			if _, err := a.StartAgent(taskID, mode, "Continue implementing this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
				a.logger.Error("restart-stale.failed", "task_id", taskID, "err", err)
			}
		})
	}
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
	a.logAudit(audit.EventTaskCreated, t.ID, "", map[string]any{"title": title, "mode": mode})
	if t.Status == task.StatusTodo {
		a.logger.Info("auto-triage.start", "task_id", t.ID, "title", t.Title)
		a.wg.Go(func() {
			if triageErr := a.TriageTask(t.ID); triageErr != nil {
				a.logger.Error("auto-triage.failed", "task_id", t.ID, "err", triageErr)
			}
		})
	}
	return t, nil
}

func (a *App) UpdateTask(id string, updates map[string]any) (task.Task, error) {
	var prevStatus string
	if newStatus, ok := updates["status"].(string); ok {
		if prev, getErr := a.tasks.Get(id); getErr == nil {
			prevStatus = string(prev.Status)
			if prevStatus != newStatus {
				a.logAudit(audit.EventTaskStatusChanged, id, "", map[string]any{"from": prevStatus, "to": newStatus})
			}
		}
	}
	t, err := a.tasks.Update(id, updates)
	if err != nil {
		return t, err
	}
	if t.Status == task.StatusPlanning {
		a.logger.Info("auto-plan.start", "task_id", t.ID, "title", t.Title)
		a.wg.Go(func() {
			if planErr := a.PlanTask(t.ID); planErr != nil {
				a.logger.Error("auto-plan.failed", "task_id", t.ID, "err", planErr)
			}
		})
	}
	if t.Status == task.StatusInProgress && !a.agents.HasRunningAgentForTask(t.ID) && !slices.Contains(t.Tags, "review") {
		a.logger.Info("auto-implement.start", "task_id", t.ID, "title", t.Title)
		a.wg.Go(func() {
			if _, err := a.StartAgent(t.ID, t.AgentMode, "Implement this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
				a.logger.Error("auto-implement.failed", "task_id", t.ID, "err", err)
			}
		})
	}
	if t.Status == task.StatusDone {
		a.wg.Go(func() { a.cleanupWorktree(t.ID) })
	}
	return t, nil
}

func (a *App) DeleteTask(id string) error {
	a.logger.Info("task.delete", "task_id", id)
	a.logAudit(audit.EventTaskDeleted, id, "", nil)
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

	t = a.autoAssignProject(t)

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			return nil, fmt.Errorf("worktree required for project task: %w", wtErr)
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s\n\n---\n\n%s", t.Title, t.Body, prompt)
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       taskID,
		Name:         t.Title,
		Mode:         mode,
		Prompt:       fullPrompt,
		AllowedTools: t.AllowedTools,
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return nil, err
	}
	a.logAudit(audit.EventAgentStarted, taskID, ag.ID, map[string]any{"mode": mode, "title": t.Title})
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

func (a *App) autoAssignProject(t task.Task) task.Task {
	if t.ProjectID != "" || a.projects == nil {
		return t
	}
	projects, err := a.projects.List()
	if err != nil || len(projects) != 1 {
		return t
	}
	t.ProjectID = projects[0].ID
	if _, err := a.tasks.Update(t.ID, map[string]any{"project_id": t.ProjectID}); err != nil {
		a.logger.Error("auto-assign-project", "task_id", t.ID, "err", err)
	} else {
		a.logger.Info("auto-assign-project", "task_id", t.ID, "project", t.ProjectID)
	}
	return t
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
	wtBranch := "synapse/" + t.DirName()
	if _, statErr := os.Stat(wtPath); statErr == nil {
		if t.Branch == "" {
			if _, err := a.tasks.Update(t.ID, map[string]any{"branch": wtBranch}); err != nil {
				a.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
			}
		}
		return wtPath, nil
	}
	if err := project.CreateWorktree(proj.ClonePath, wtPath, wtBranch, "refs/remotes/origin/"+branch); err != nil {
		return "", fmt.Errorf("create worktree: %w", err)
	}

	a.logger.Info("worktree.created", "task_id", t.ID, "path", wtPath)

	if t.Branch == "" {
		if _, err := a.tasks.Update(t.ID, map[string]any{"branch": wtBranch}); err != nil {
			a.logger.Error("worktree.set-branch", "task_id", t.ID, "err", err)
		}
	}

	return wtPath, nil
}

func (a *App) cleanupWorktree(taskID string) {
	t, err := a.tasks.Get(taskID)
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

func (a *App) CreateProject(url, ptype string) (project.Project, error) {
	a.logger.Info("project.create", "url", url, "type", ptype)
	p, err := a.projects.Create(url, project.ProjectType(ptype))
	if err != nil {
		a.logger.Error("project.create.failed", "url", url, "err", err)
		return p, err
	}
	a.logger.Info("project.created", "id", p.ID, "url", url)
	return p, nil
}

func (a *App) UpdateProject(id, ptype string) (project.Project, error) {
	a.logger.Info("project.update", "id", id, "type", ptype)
	p, err := a.projects.Update(id, project.ProjectType(ptype))
	if err != nil {
		a.logger.Error("project.update.failed", "id", id, "err", err)
		return p, err
	}
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
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.agents.CheckInteractiveSessions()
			a.maybeStartOrchestrator()
			a.maybeDispatchTasks()
			a.maybeResumePlanning()
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
		default:
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
		if a.agents.HasRunningAgentForTask(tasks[i].ID) {
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
		if slices.Contains(t.Tags, "review") {
			if _, err := a.tasks.Update(t.ID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
				a.logger.Error("auto-dispatch.review.status", "task_id", t.ID, "err", err)
				continue
			}
			if err := a.startReviewAgent(t); err != nil {
				a.logger.Error("auto-dispatch.review.failed", "task_id", t.ID, "err", err)
			}
			continue
		}
		if _, err := a.UpdateTask(t.ID, map[string]any{"status": string(task.StatusInProgress)}); err != nil {
			a.logger.Error("auto-dispatch.failed", "task_id", t.ID, "err", err)
		}
	}
}

func (a *App) maybeResumePlanning() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		if tasks[i].Status != task.StatusPlanning {
			continue
		}
		if a.agents.HasRunningAgentForTask(tasks[i].ID) {
			continue
		}
		a.logger.Info("plan.resume", "task_id", tasks[i].ID, "title", tasks[i].Title)
		go func(id string) {
			if err := a.PlanTask(id); err != nil {
				a.logger.Error("plan.resume.failed", "task_id", id, "err", err)
			}
		}(tasks[i].ID)
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
	a.logAudit(audit.EventOrchestratorStart, "", "", nil)
	runtime.EventsEmit(a.ctx, "orchestrator:state", "running")
	return nil
}

func (a *App) StopOrchestrator() error {
	if err := a.tmux.KillSession(orchestratorSession); err != nil {
		return fmt.Errorf("stop orchestrator: %w", err)
	}
	a.logger.Info("orchestrator.stopped")
	a.logAudit(audit.EventOrchestratorStop, "", "", nil)
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

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       t.ID,
		Name:         "triage:" + t.Title,
		Mode:         "headless",
		Prompt:       prompt,
		AllowedTools: []string{"Bash", "Read", "Skill"},
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "triage", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logger.Info("triage.agent_started", "task_id", t.ID, "agent_id", ag.ID)
	return nil
}

func (a *App) createReviewTask(pr github.PullRequest, projectID string) {
	title := "Review: " + pr.Title
	body := fmt.Sprintf("%s\n\nAuthor: @%s", pr.URL, pr.Author)

	t, err := a.tasks.Create(title, body, "headless")
	if err != nil {
		a.logger.Error("review.create-task", "pr", pr.Number, "err", err)
		return
	}

	if _, err := a.tasks.Update(t.ID, map[string]any{
		"tags":       "review",
		"project_id": projectID,
		"pr_number":  pr.Number,
		"status":     string(task.StatusTodo),
	}); err != nil {
		a.logger.Error("review.update-task", "task_id", t.ID, "err", err)
		return
	}
	a.logger.Info("review.task-created", "task_id", t.ID, "pr", pr.Number, "project", projectID)
}

func (a *App) startReviewAgent(t task.Task) error {
	prompt := fmt.Sprintf("Run /staff-code-review on https://github.com/%s/pull/%d", t.ProjectID, t.PRNumber)

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   "review:" + t.Title,
		Mode:   "headless",
		Prompt: prompt,
		Dir:    config.HomeDir(),
		Model:  "opus",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "review", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("task.add-run", "task_id", t.ID, "err", err)
	}
	a.logAudit(audit.EventReviewStarted, t.ID, ag.ID, map[string]any{"pr": t.PRNumber})
	a.logger.Info("review.agent-started", "task_id", t.ID, "agent_id", ag.ID, "pr", t.PRNumber)
	return nil
}

func (a *App) resolveReviewStatus(taskID string) {
	t, err := a.tasks.Get(taskID)
	if err != nil {
		return
	}
	if t.PRNumber == 0 || t.ProjectID == "" {
		if _, err := a.tasks.Update(taskID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			a.logger.Error("review.status-update", "task_id", taskID, "err", err)
		}
		return
	}

	pending, err := github.HasPendingReview(t.ProjectID, t.PRNumber)
	if err != nil {
		a.logger.Warn("review.pending-check", "task_id", taskID, "err", err)
		if _, err := a.tasks.Update(taskID, map[string]any{"status": string(task.StatusInReview)}); err != nil {
			a.logger.Error("review.status-update", "task_id", taskID, "err", err)
		}
		return
	}

	nextStatus := task.StatusInReview
	if pending {
		nextStatus = task.StatusHumanRequired
	}
	if _, err := a.tasks.Update(taskID, map[string]any{"status": string(nextStatus)}); err != nil {
		a.logger.Error("review.status-update", "task_id", taskID, "err", err)
	}
}

func (a *App) handleAgentComplete(ag *agent.Agent) {
	var resultContent string
	for _, ev := range ag.Output() {
		if ev.Type == "result" {
			resultContent = ev.Content
		}
	}

	duration := time.Since(ag.StartedAt).Seconds()
	agentData := map[string]any{
		"mode":       ag.Mode,
		"cost_usd":   ag.CostUSD,
		"duration_s": duration,
		"state":      string(ag.State),
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

	// Notify for non-system agents
	if !strings.HasPrefix(ag.Name, "triage:") && !strings.HasPrefix(ag.Name, "eval:") {
		level := notification.LevelSuccess
		title := "Agent completed"
		if ag.State == agent.StateStopped && !hasResultEvent(ag) {
			level = notification.LevelError
			title = "Agent failed"
		}
		a.notifier.Send(level, title, ag.Name, ag.TaskID, ag.ID)
	}

	if strings.HasPrefix(ag.Name, "triage:") || strings.HasPrefix(ag.Name, "eval:") {
		a.logger.Info("eval.skip", "agent_id", ag.ID, "name", ag.Name, "reason", "system_agent")
		a.logAudit(audit.EventTriageCompleted, ag.TaskID, ag.ID, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "pr-fix:") {
		a.logger.Info("pr-fix.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		a.logAudit(audit.EventPRFixAgentStarted, ag.TaskID, ag.ID, agentData)
		go func() {
			if err := a.EvaluateTask(ag.TaskID, resultContent); err != nil {
				a.logger.Error("pr-fix.eval", "task_id", ag.TaskID, "err", err)
			}
		}()
		return
	}

	if strings.HasPrefix(ag.Name, "plan:") {
		a.completePlanAgent(ag, resultContent, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "eval:") {
		a.completeEvalAgent(ag, agentData)
		return
	}

	if strings.HasPrefix(ag.Name, "review:") {
		a.logger.Info("review.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
		a.logAudit(audit.EventReviewStarted, ag.TaskID, ag.ID, agentData)
		go a.resolveReviewStatus(ag.TaskID)
		return
	}

	eventType := audit.EventAgentCompleted
	if ag.State != agent.StateStopped {
		eventType = audit.EventAgentFailed
	}
	a.logAudit(eventType, ag.TaskID, ag.ID, agentData)

	a.wg.Go(func() {
		if err := a.EvaluateTask(ag.TaskID, resultContent); err != nil {
			a.logger.Error("auto-evaluate.failed", "task_id", ag.TaskID, "agent_id", ag.ID, "err", err)
		}
	})
}

func (a *App) completePlanAgent(ag *agent.Agent, resultContent string, agentData map[string]any) {
	a.logger.Info("plan.complete", "agent_id", ag.ID, "task_id", ag.TaskID)
	a.logAudit(audit.EventPlanCompleted, ag.TaskID, ag.ID, agentData)
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
}

func (a *App) completeEvalAgent(ag *agent.Agent, agentData map[string]any) {
	a.logger.Info("eval.done", "agent_id", ag.ID, "name", ag.Name)
	a.logAudit(audit.EventEvalCompleted, ag.TaskID, ag.ID, agentData)
	t, err := a.tasks.Get(ag.TaskID)
	if err != nil {
		return
	}
	if t.Status == task.StatusDone {
		a.logger.Warn("eval.reverted_done", "agent_id", ag.ID, "task_id", ag.TaskID)
		if _, uerr := a.tasks.Update(ag.TaskID, map[string]any{
			"status": string(task.StatusInReview),
		}); uerr != nil {
			a.logger.Error("eval.revert_status", "task_id", ag.TaskID, "err", uerr)
		}
	}
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
		"Evaluate task %s. Do NOT read source code or review diffs.\n\n"+
			"1. Run: synapse-cli --json get %s\n"+
			"2. Check agent result below for PR URLs (github.com/.../pull/N). "+
			"If found, link: synapse-cli --json update %s --pr <number>\n"+
			"3. Set status based on agent result:\n"+
			"   - Work done, PR created/pushed → in-review\n"+
			"   - Failed, errors, incomplete → human-required\n"+
			"   - Never set done or todo\n"+
			"   Run: synapse-cli --json update %s --status <status>\n\n"+
			"## Agent Result\n\n%s",
		t.ID, t.ID, t.ID, t.ID, truncated)

	a.logger.Info("eval.start", "task_id", t.ID, "title", t.Title, "dir", dir)

	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       t.ID,
		Name:         "eval:" + t.Title,
		Mode:         "headless",
		Prompt:       prompt,
		AllowedTools: []string{"Bash"},
		Dir:          dir,
		Model:        "sonnet",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "eval", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
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

	t = a.autoAssignProject(t)

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

	planDir := dir
	if planDir == "" {
		planDir = config.HomeDir()
	}
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID:       t.ID,
		Name:         "plan:" + t.Title,
		Mode:         "headless",
		Prompt:       prompt,
		AllowedTools: []string{"Bash", "Read", "Glob", "Grep"},
		Dir:          planDir,
		Model:        "opus",
	})
	if err != nil {
		return err
	}
	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "plan", Mode: "headless", State: string(agent.StateRunning), StartedAt: ag.StartedAt,
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
	a.logAudit(audit.EventPlanApproved, id, "", map[string]any{"title": t.Title})
	return a.UpdateTask(id, map[string]any{
		"status": string(task.StatusInProgress),
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
	a.logAudit(audit.EventPlanRejected, id, "", map[string]any{"title": t.Title, "has_feedback": feedback != ""})
	body := t.Body
	if feedback != "" {
		body += "\n\n## Plan Feedback\n\n" + feedback
	}
	updated, err := a.tasks.Update(id, map[string]any{
		"status": string(task.StatusPlanning),
		"body":   body,
	})
	if err != nil {
		return updated, err
	}
	go func() {
		if planErr := a.PlanTask(id); planErr != nil {
			a.logger.Error("plan.reject.replan", "task_id", id, "err", planErr)
		}
	}()
	return updated, nil
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

func (a *App) ListNotifications() []notification.Notification {
	return a.notifier.List()
}

func (a *App) SetDesktopNotifications(enabled bool) {
	a.notifier.SetDesktop(enabled)
}

func hasResultEvent(ag *agent.Agent) bool {
	for _, ev := range ag.Output() {
		if ev.Type == "result" {
			return true
		}
	}
	return false
}

func (a *App) MarkPRReady(repo string, number int) error {
	return github.MarkReady(repo, number)
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

const (
	prPollFast = 1 * time.Minute
	prPollSlow = 5 * time.Minute
)

func (a *App) prPollLoop(ctx context.Context) {
	timer := time.NewTimer(10 * time.Second) // initial fetch shortly after startup
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			next := a.pollAndMonitorPRs()
			a.logger.Debug("pr-poll.next", "interval", next)
			timer.Reset(next)
		}
	}
}

func (a *App) pollAndMonitorPRs() time.Duration {
	summary, err := github.FetchReviews()
	if err != nil {
		a.logger.Warn("pr-monitor.fetch", "err", err)
		return prPollSlow
	}

	runtime.EventsEmit(a.ctx, "reviews:updated", summary)

	tasks, err := a.tasks.List()
	if err != nil {
		return prPollSlow
	}

	// Monitor PRs created by the user (conflicts, CI, merged/closed)
	var matchers []github.TaskMatcher
	for i := range tasks {
		if tasks[i].Status != task.StatusInReview {
			continue
		}
		if tasks[i].PRNumber == 0 && tasks[i].Branch == "" {
			continue
		}
		matchers = append(matchers, github.TaskMatcher{
			ID:        tasks[i].ID,
			PRNumber:  tasks[i].PRNumber,
			Branch:    tasks[i].Branch,
			ProjectID: tasks[i].ProjectID,
		})
	}

	if len(matchers) > 0 {
		issues := github.MatchTaskPRs(summary.CreatedByMe, matchers)
		a.prTracker.Cleanup()

		for i := range issues {
			if a.agents.HasRunningAgentForTask(issues[i].TaskID) {
				continue
			}
			if !a.prTracker.ShouldHandle(issues[i].TaskID, issues[i].Kind) {
				continue
			}
			a.handlePRIssue(issues[i])
		}

		closedPRs := github.DetectClosedTaskPRs(summary.CreatedByMe, matchers, github.FetchPRState)
		for _, c := range closedPRs {
			if _, err := a.tasks.Update(c.TaskID, map[string]any{"status": string(task.StatusDone)}); err != nil {
				a.logger.Error("pr-monitor.closed-update", "task_id", c.TaskID, "err", err)
				continue
			}
			eventType := audit.EventPRMerged
			if c.State == "CLOSED" {
				eventType = audit.EventPRClosed
			}
			a.logAudit(eventType, c.TaskID, "", map[string]any{"pr": c.PRNumber, "state": c.State})
			a.logger.Info("pr-monitor.auto-done", "task_id", c.TaskID, "pr", c.PRNumber, "state", c.State)
		}
	}

	// Auto-create review tasks from review-requested PRs
	a.maybeCreateReviewTasks(tasks, summary.ReviewRequested)

	// Detect published reviews (human-required → in-review)
	a.detectPublishedReviews(tasks)

	if prNeedsAttention(summary.CreatedByMe) {
		return prPollFast
	}
	return prPollSlow
}

func prNeedsAttention(prs []github.PullRequest) bool {
	for i := range prs {
		if prs[i].CIStatus == "PENDING" || prs[i].CIStatus == "FAILURE" {
			return true
		}
		if prs[i].Mergeable == "CONFLICTING" || prs[i].Mergeable == "UNKNOWN" {
			return true
		}
	}
	return false
}

func (a *App) maybeCreateReviewTasks(tasks []task.Task, reviewPRs []github.PullRequest) {
	projects, err := a.projects.List()
	if err != nil || len(projects) == 0 {
		return
	}

	projectMatchers := make([]github.ProjectMatcher, 0, len(projects))
	for i := range projects {
		projectMatchers = append(projectMatchers, github.ProjectMatcher{
			ID:         projects[i].Owner + "/" + projects[i].Repo,
			Repository: projects[i].Owner + "/" + projects[i].Repo,
		})
	}

	matches := github.MatchReviewPRs(reviewPRs, projectMatchers)
	for i := range matches {
		if a.hasReviewTask(tasks, matches[i].PR.Number) {
			continue
		}
		a.createReviewTask(matches[i].PR, matches[i].ProjectID)
	}
}

func (a *App) hasReviewTask(tasks []task.Task, prNumber int) bool {
	for i := range tasks {
		if tasks[i].PRNumber == prNumber && slices.Contains(tasks[i].Tags, "review") {
			return true
		}
	}
	return false
}

func (a *App) detectPublishedReviews(tasks []task.Task) {
	for i := range tasks {
		if tasks[i].Status != task.StatusHumanRequired {
			continue
		}
		if !slices.Contains(tasks[i].Tags, "review") {
			continue
		}
		if tasks[i].PRNumber == 0 || tasks[i].ProjectID == "" {
			continue
		}

		pending, err := github.HasPendingReview(tasks[i].ProjectID, tasks[i].PRNumber)
		if err != nil {
			a.logger.Warn("review.poll-pending", "task_id", tasks[i].ID, "err", err)
			continue
		}
		if !pending {
			if _, err := a.tasks.Update(tasks[i].ID, map[string]any{
				"status": string(task.StatusInReview),
			}); err != nil {
				a.logger.Error("review.published-update", "task_id", tasks[i].ID, "err", err)
				continue
			}
			a.logAudit(audit.EventReviewPublished, tasks[i].ID, "", map[string]any{"pr": tasks[i].PRNumber})
			a.logger.Info("review.published", "task_id", tasks[i].ID, "pr", tasks[i].PRNumber)
		}
	}
}

func (a *App) handlePRIssue(issue github.PRIssue) {
	t, err := a.tasks.Get(issue.TaskID)
	if err != nil {
		return
	}

	if _, err := a.tasks.Update(t.ID, map[string]any{
		"status": string(task.StatusInProgress),
	}); err != nil {
		a.logger.Error("pr-monitor.status-update", "task_id", t.ID, "err", err)
		return
	}

	var prompt string
	switch issue.Kind {
	case github.PRIssueConflict:
		prompt = fmt.Sprintf(
			"Fix merge conflicts on branch `%s` (PR #%d). "+
				"Do NOT investigate — go straight to fixing.\n\n"+
				"```bash\n"+
				"git fetch origin main\n"+
				"git rebase origin/main\n"+
				"# resolve each conflict, git add, git rebase --continue\n"+
				"git push --force-with-lease\n"+
				"```\n\n"+
				"Resolve conflicts to keep BOTH sides' changes. Push when done.",
			issue.PR.HeadRefName, issue.PR.Number,
		)
		a.logAudit(audit.EventPRConflictDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})

	case github.PRIssueCIFailure:
		prompt = fmt.Sprintf(
			"Fix failing CI on branch `%s` (PR #%d). "+
				"Do NOT investigate git state — go straight to the failure.\n\n"+
				"```bash\n"+
				"gh run list --branch %s --limit 3\n"+
				"gh run view <FAILED_RUN_ID> --log-failed\n"+
				"```\n\n"+
				"Read the failure, fix the code, commit and push. No unrelated changes.",
			issue.PR.HeadRefName, issue.PR.Number,
			issue.PR.HeadRefName,
		)
		a.logAudit(audit.EventPRCIFailureDetected, t.ID, "", map[string]any{
			"pr": issue.PR.Number, "repo": issue.PR.Repository,
		})
	}

	dir := ""
	if t.ProjectID != "" {
		d, wtErr := a.prepareWorktree(t)
		if wtErr != nil {
			a.logger.Error("pr-monitor.worktree", "task_id", t.ID, "err", wtErr)
			return
		}
		dir = d
	}

	fullPrompt := fmt.Sprintf("# Task: %s\n\n%s", t.Title, prompt)
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: t.ID,
		Name:   "pr-fix:" + t.Title,
		Mode:   "headless",
		Prompt: fullPrompt,
		Dir:    dir,
		Model:  "sonnet",
	})
	if err != nil {
		a.logger.Error("pr-monitor.agent-start", "task_id", t.ID, "err", err)
		return
	}

	a.prTracker.MarkHandled(t.ID, issue.Kind)
	a.logAudit(audit.EventPRFixAgentStarted, t.ID, ag.ID, map[string]any{
		"issue": string(issue.Kind), "pr": issue.PR.Number,
	})

	if err := a.tasks.AddRun(t.ID, task.AgentRun{
		AgentID: ag.ID, Role: "pr-fix", Mode: "headless",
		State: string(agent.StateRunning), StartedAt: ag.StartedAt,
	}); err != nil {
		a.logger.Error("pr-monitor.add-run", "task_id", t.ID, "err", err)
	}

	a.logger.Info("pr-monitor.fix-started",
		"task_id", t.ID, "issue", string(issue.Kind),
		"pr", issue.PR.Number, "agent_id", ag.ID,
	)
}
