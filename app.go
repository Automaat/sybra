package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/audit"
	"github.com/Automaat/synapse/internal/config"
	"github.com/Automaat/synapse/internal/github"
	"github.com/Automaat/synapse/internal/notification"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/spotlight"
	"github.com/Automaat/synapse/internal/stats"
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
	stats        *stats.Store
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

	statsStore, err := stats.NewStore(config.StatsFile())
	if err != nil {
		a.logger.Error("stats.init", "err", err)
	} else {
		a.stats = statsStore
		if err := statsStore.Backfill(a.auditDir); err != nil {
			a.logger.Error("stats.backfill", "err", err)
		}
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
	a.syncSkills()
	a.reconnectAgents()
	a.cleanupOrphanedWorktrees()
	a.cleanStaleRuns()
	a.restartStaleInProgress()
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

// restartStaleInProgress re-dispatches headless in-progress tasks that lost
// their agent due to a crash or restart. Interactive tasks are handled by
// reconnectAgents (tmux sessions survive restarts).
func (a *App) restartStaleInProgress() {
	tasks, err := a.tasks.List()
	if err != nil {
		return
	}
	for i := range tasks {
		t := tasks[i]
		if t.Status != task.StatusInProgress {
			continue
		}
		if t.AgentMode != "headless" {
			continue
		}
		if a.agents.HasRunningAgentForTask(t.ID) {
			continue
		}
		if slices.Contains(t.Tags, "review") {
			continue
		}
		// Tasks whose last agent was a pr-fix should not be re-implemented.
		// Move them back to in-review so prPollLoop can re-detect and fix.
		if lastRun := lastAgentRun(&t); lastRun != nil && lastRun.Role == "pr-fix" {
			a.logger.Info("restart-stale.revert-to-review", "task_id", t.ID)
			if _, err := a.tasks.Update(t.ID, map[string]any{
				"status": string(task.StatusInReview),
			}); err != nil {
				a.logger.Error("restart-stale.revert", "task_id", t.ID, "err", err)
			}
			continue
		}
		if t.ProjectID == "" {
			a.logger.Warn("restart-stale.skip", "task_id", t.ID, "reason", "no project_id")
			continue
		}
		a.logger.Info("restart.stale-in-progress", "task_id", t.ID, "run_role", t.RunRole)
		taskID := t.ID
		runRole := t.RunRole
		if runRole == "pr-fix" {
			a.wg.Go(func() {
				if err := a.startPRFixReviewAgent(taskID); err != nil {
					a.logger.Error("restart.pr-fix.failed", "task_id", taskID, "err", err)
				}
			})
		} else {
			a.wg.Go(func() {
				if _, err := a.StartAgent(taskID, "headless", "Continue implementing this task. When done, create a draft PR with `gh pr create --draft`."); err != nil {
					a.logger.Error("restart.implement.failed", "task_id", taskID, "err", err)
				}
			})
		}
	}
}

func lastAgentRun(t *task.Task) *task.AgentRun {
	if len(t.AgentRuns) == 0 {
		return nil
	}
	return &t.AgentRuns[len(t.AgentRuns)-1]
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

	a.logger.Info("skills.sync.start", "src", repoDir, "dst", a.skillsDir)

	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	a.syncDir(skillsSrc, a.skillsDir)

	claudeSrc := filepath.Join(repoDir, "orchestrator", "CLAUDE.md")
	claudeDst := filepath.Join(config.HomeDir(), "CLAUDE.md")
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

// ListNotifications returns pending in-app notifications.
func (a *App) ListNotifications() []notification.Notification {
	return a.notifier.List()
}

// SetDesktopNotifications enables or disables macOS desktop notifications.
func (a *App) SetDesktopNotifications(enabled bool) {
	a.notifier.SetDesktop(enabled)
}

// RegisterSpotlightHotkey binds Ctrl+Space to the Spotlight quick-add panel.
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
