package sybra

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"

	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setupApp(t *testing.T) *App {
	t.Helper()
	// Use os.MkdirTemp instead of t.TempDir() to avoid cleanup races
	// with background goroutines (TriageTask spawned by CreateTask).
	dir, err := os.MkdirTemp("", "sybra-test-tasks-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	logger := discardLogger()
	emit := func(string, any) {}
	logDir := filepath.Join(os.TempDir(), "sybra-test-logs")
	mgr := agent.NewManager(t.Context(), emit, logger, logDir)

	wm := worktree.New(worktree.Config{
		WorktreesDir: t.TempDir(),
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: mgr.HasRunningAgentForTask,
	})
	agentOrch := newAgentOrchestrator(taskMgr, nil, mgr, nil, logger, wm, nil)

	return &App{
		tasks:     taskMgr,
		agents:    mgr,
		tasksDir:  dir,
		logger:    logger,
		worktrees: wm,
		agentOrch: agentOrch,
	}
}

func setupTaskService(t *testing.T) (*TaskService, *App) {
	t.Helper()
	a := setupApp(t)
	var wg sync.WaitGroup

	wfDir := t.TempDir()
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.SyncBuiltins(wfStore); err != nil {
		t.Fatal(err)
	}
	ta := &taskAdapter{tasks: a.tasks}
	aa := &agentAdapter{agents: a.agents, agentOrch: a.agentOrch, tasks: a.tasks}
	engine := workflow.NewEngine(wfStore, ta, aa, a.logger)

	svc := &TaskService{
		tasks:          a.tasks,
		agents:         a.agents,
		workflowEngine: engine,
		worktrees:      a.worktrees,
		wg:             &wg,
		logger:         a.logger,
	}
	return svc, a
}

func setupPlanningService(t *testing.T) (*PlanningService, *TaskService, *App) {
	t.Helper()
	taskSvc, a := setupTaskService(t)
	planSvc := &PlanningService{
		engine: taskSvc.workflowEngine,
		tasks:  a.tasks,
		agents: a.agents,
	}
	return planSvc, taskSvc, a
}

func setupAgentService(t *testing.T) (*AgentService, *App) {
	t.Helper()
	a := setupApp(t)
	svc := &AgentService{
		agents: a.agents,
		logger: a.logger,
	}
	return svc, a
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Logging:      config.LoggingConfig{Dir: t.TempDir()},
		TasksDir:     t.TempDir(),
		SkillsDir:    t.TempDir(),
		ProjectsDir:  t.TempDir(),
		ClonesDir:    t.TempDir(),
		WorktreesDir: t.TempDir(),
	}
}

func TestOnAgentComplete_EmptyTaskID_NoCrash(t *testing.T) {
	// Orchestrator brain agents run with TaskID="" — feeding that into
	// UpdateRun/HandleAgentComplete/Get used to crash the handler with
	// "open .sybra/tasks/.md: no such file or directory" because the
	// empty ID was joined to the tasks dir. Verify the short-circuit.
	a := setupApp(t)

	// Pre-existing task that must NOT be touched: an empty-id call must
	// not accidentally rewrite a task file (the original bug joined "" to
	// the tasks dir as ".md" — the path it would have hit is here too).
	other, err := a.tasks.Create("Other task", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	otherStat, err := os.Stat(other.FilePath)
	if err != nil {
		t.Fatal(err)
	}

	ag := &agent.Agent{
		ID:        "orch-agent",
		TaskID:    "",
		Mode:      "interactive",
		StartedAt: other.CreatedAt,
	}

	// Should not panic, and should not touch any task file. The historical
	// bug created/touched ".md" in tasksDir; assert no such file exists.
	a.onAgentComplete(ag)

	if _, err := os.Stat(filepath.Join(a.tasksDir, ".md")); !os.IsNotExist(err) {
		t.Errorf("expected no .md file in tasks dir, got err=%v", err)
	}
	otherStat2, err := os.Stat(other.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !otherStat.ModTime().Equal(otherStat2.ModTime()) {
		t.Errorf("unrelated task file was rewritten: mtime %v -> %v", otherStat.ModTime(), otherStat2.ModTime())
	}
}

func TestNewApp(t *testing.T) {
	cfg := testConfig(t)
	a := NewApp(discardLogger(), &slog.LevelVar{}, cfg)
	if a == nil {
		t.Fatal("NewApp returned nil")
	}
	if a.tasksDir != cfg.TasksDir {
		t.Errorf("tasksDir = %q, want %q", a.tasksDir, cfg.TasksDir)
	}
}

func TestListTasksEmpty(t *testing.T) {
	svc, _ := setupTaskService(t)
	tasks, err := svc.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("got %d tasks, want 0", len(tasks))
	}
}

func TestCreateAndGetTask(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("test title", "body", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if created.Title != "test title" {
		t.Errorf("Title = %q, want %q", created.Title, "test title")
	}

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %q, want %q", got.ID, created.ID)
	}
}

func TestUpdateTask(t *testing.T) {
	svc, _ := setupTaskService(t)

	created, err := svc.CreateTask("update me", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.UpdateTask(created.ID, map[string]any{"status": "done"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "done" {
		t.Errorf("Status = %q, want %q", updated.Status, "done")
	}
}

func TestListTasksAfterCreate(t *testing.T) {
	svc, _ := setupTaskService(t)

	for _, title := range []string{"one", "two", "three"} {
		if _, err := svc.CreateTask(title, "", "headless"); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := svc.ListTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Errorf("got %d tasks, want 3", len(tasks))
	}
}

func TestGetTaskNotFound(t *testing.T) {
	svc, _ := setupTaskService(t)
	_, err := svc.GetTask("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

// TestStartAgentRejectsMissingProject verifies the orchestrator refuses to
// spawn an agent when the task has no project_id, preventing the agent from
// mutating Sybra's own working directory (the class of bug that caused
// branch changes in the main repo).
func TestStartAgentRejectsMissingProject(t *testing.T) {
	taskSvc, a := setupTaskService(t)

	created, err := taskSvc.CreateTask("agent task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	_, err = a.StartAgent(created.ID, "headless", "test prompt")
	if err == nil {
		t.Fatal("expected error: task without project_id must be rejected")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Errorf("expected project_id error, got: %v", err)
	}
}

func TestStartAgentTaskNotFound(t *testing.T) {
	a := setupApp(t)
	_, err := a.StartAgent("nonexistent", "headless", "prompt")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

// runTestAgent bypasses the orchestrator (which requires a project) and spawns
// an agent directly in a temp dir. Used by lifecycle tests that only care
// about agent state machinery, not worktree integration.
func runTestAgent(t *testing.T, a *App, taskID, title string) *agent.Agent {
	t.Helper()
	dir, err := os.MkdirTemp("", "sybra-app-agent-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ag, err := a.agents.Run(agent.RunConfig{
		TaskID: taskID,
		Name:   title,
		Mode:   "headless",
		Prompt: "test",
		Dir:    dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func TestStopAgent(t *testing.T) {
	taskSvc, a := setupTaskService(t)
	agentSvc := &AgentService{agents: a.agents, logger: a.logger}

	created, err := taskSvc.CreateTask("stop task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag := runTestAgent(t, a, created.ID, "stop task")

	if err := agentSvc.StopAgent(ag.ID); err != nil {
		t.Fatal(err)
	}
}

func TestStopAgentNotFound(t *testing.T) {
	svc, _ := setupAgentService(t)
	err := svc.StopAgent("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListAgentsEmpty(t *testing.T) {
	svc, _ := setupAgentService(t)
	agents := svc.ListAgents()
	if len(agents) != 0 {
		t.Errorf("got %d agents, want 0", len(agents))
	}
}

func TestDiscoverAgents(t *testing.T) {
	svc, _ := setupAgentService(t)
	agents := svc.DiscoverAgents()
	_ = agents
}

func TestGetAgentOutput(t *testing.T) {
	taskSvc, a := setupTaskService(t)
	agentSvc := &AgentService{agents: a.agents, logger: a.logger}

	created, err := taskSvc.CreateTask("output task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	ag := runTestAgent(t, a, created.ID, "output task")

	events, err := agentSvc.GetAgentOutput(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if events == nil {
		events = []agent.StreamEvent{}
	}
	_ = events
}

func TestGetAgentOutputNotFound(t *testing.T) {
	svc, _ := setupAgentService(t)
	_, err := svc.GetAgentOutput("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSyncFile(t *testing.T) {
	a := setupApp(t)
	a.repoDir = t.TempDir()

	srcDir := filepath.Join(a.repoDir, "sub")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(srcDir, "test.md")
	if err := os.WriteFile(srcFile, []byte("# hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstFile := filepath.Join(t.TempDir(), "out", "test.md")
	a.syncFile(srcFile, dstFile)

	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("dst not written: %v", err)
	}
	if string(data) != "# hello" {
		t.Errorf("content = %q, want %q", string(data), "# hello")
	}
}

func TestSyncFileMissingSrc(t *testing.T) {
	a := setupApp(t)
	dstFile := filepath.Join(t.TempDir(), "should-not-exist.md")
	a.syncFile("/nonexistent/file.md", dstFile)

	if _, err := os.Stat(dstFile); !os.IsNotExist(err) {
		t.Error("dst should not be created when src missing")
	}
}

func TestSyncSkillsDir(t *testing.T) {
	a := setupApp(t)

	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	frontmatter := func(name string) []byte {
		return []byte("---\nname: " + name + "\ndescription: test\n---\n\n# " + name)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(srcDir, name+".md"), frontmatter(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-.md, and a malformed .md without frontmatter must be skipped.
	if err := os.WriteFile(filepath.Join(srcDir, "c.txt"), []byte("content-c"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "no-frontmatter.md"), []byte("# nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	a.syncSkillsDir(srcDir, dstDir)

	// Each valid flat skill must land as <name>/SKILL.md, which is the layout
	// Claude Code and Codex both require — flat .md files in the destination
	// are never discovered by the skill loader.
	for _, name := range []string{"a", "b"} {
		skillMD := filepath.Join(dstDir, name, "SKILL.md")
		if _, err := os.Stat(skillMD); err != nil {
			t.Errorf("%s/SKILL.md missing at dst: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dstDir, "no-frontmatter")); !os.IsNotExist(err) {
		t.Errorf("malformed skill should not be written: stat err=%v", err)
	}
}

func TestSyncSkillsDirRemovesOrphans(t *testing.T) {
	a := setupApp(t)

	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := []byte("---\nname: keep\ndescription: test\n---\n\n# keep")
	if err := os.WriteFile(filepath.Join(srcDir, "keep.md"), keep, 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	orphan := filepath.Join(dstDir, "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "SKILL.md"), []byte("---\nname: orphan\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a.syncSkillsDir(srcDir, dstDir)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan skill dir should be removed: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "keep", "SKILL.md")); err != nil {
		t.Errorf("keep/SKILL.md missing: %v", err)
	}
}

func TestSyncSkillsDirCopiesDirectoryStyleSkill(t *testing.T) {
	a := setupApp(t)

	srcDir := filepath.Join(t.TempDir(), "skills")
	skillDir := filepath.Join(srcDir, "plan-critic")
	refDir := filepath.Join(skillDir, "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := []byte("---\nname: plan-critic\ndescription: test\n---\n\n# plan-critic")
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), skillMD, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "checklist.md"), []byte("# checklist"), 0o644); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(srcDir, "bad-skill")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte("# no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "dst-skills")
	a.syncSkillsDir(srcDir, dstDir)

	if _, err := os.Stat(filepath.Join(dstDir, "plan-critic", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md missing at dst: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "plan-critic", "references", "checklist.md")); err != nil {
		t.Errorf("nested reference missing at dst: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "bad-skill")); !os.IsNotExist(err) {
		t.Errorf("bad skill dir should not be copied: stat err=%v", err)
	}
}

func TestSyncSkillsDirDirectoryStyleAndFrontmatter(t *testing.T) {
	a := setupApp(t)

	srcDir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid flat skill.
	flat := []byte("---\nname: good-flat\ndescription: t\n---\n\n# flat")
	if err := os.WriteFile(filepath.Join(srcDir, "good-flat.md"), flat, 0o644); err != nil {
		t.Fatal(err)
	}
	// Malformed flat skill — must NOT overwrite a valid dst skill of same name.
	if err := os.WriteFile(filepath.Join(srcDir, "bad-flat.md"), []byte("# no-fm"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Valid directory-style skill.
	dirSkill := filepath.Join(srcDir, "good-dir")
	if err := os.MkdirAll(dirSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	dirMD := []byte("---\nname: good-dir\ndescription: t\n---\n\n# dir")
	if err := os.WriteFile(filepath.Join(dirSkill, "SKILL.md"), dirMD, 0o644); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "codex-skills")
	// Pre-populate a valid bad-flat entry in the dst to verify it survives.
	preExisting := filepath.Join(dstDir, "bad-flat")
	if err := os.MkdirAll(preExisting, 0o755); err != nil {
		t.Fatal(err)
	}
	preExistingContent := []byte("---\nname: bad-flat\ndescription: valid\n---\n\n# valid")
	if err := os.WriteFile(filepath.Join(preExisting, "SKILL.md"), preExistingContent, 0o644); err != nil {
		t.Fatal(err)
	}

	a.syncSkillsDir(srcDir, dstDir)

	if _, err := os.Stat(filepath.Join(dstDir, "good-flat", "SKILL.md")); err != nil {
		t.Errorf("good-flat not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "good-dir", "SKILL.md")); err != nil {
		t.Errorf("good-dir not written: %v", err)
	}
	// bad-flat must be treated as orphan (invalid src) and removed, OR preserved
	// unchanged. The key invariant: dst must not contain the malformed payload.
	got, err := os.ReadFile(filepath.Join(dstDir, "bad-flat", "SKILL.md"))
	if err == nil && string(got) == "# no-fm" {
		t.Errorf("malformed skill overwrote valid dst: %q", got)
	}
}

func TestSyncSkillsPrefersEmbeddedWhenNoGoMod(t *testing.T) {
	a := setupApp(t)
	// Use in-memory FS stand-in: point skillsFS at a real testdata dir.
	embeddedSrc := filepath.Join(t.TempDir(), "embedded")
	dataDir := filepath.Join(embeddedSrc, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := []byte("---\nname: embedded-skill\ndescription: from embed\n---\n\n# embed")
	if err := os.WriteFile(filepath.Join(dataDir, "embedded-skill.md"), embedded, 0o644); err != nil {
		t.Fatal(err)
	}
	a.skillsFS = os.DirFS(embeddedSrc)

	// repoDir with a skills dir but NO go.mod — simulates container where
	// cwd=/home/sybra. The embedded bundle must take precedence even though
	// the skills dir already exists with a rogue file.
	repoDir := t.TempDir()
	a.repoDir = repoDir
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "rogue.md"), []byte("# no-fm"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.skillsDir = filepath.Join(t.TempDir(), "app-skills")

	a.syncSkills()

	// Embedded content lands at <name>/SKILL.md — the layout Claude Code
	// and Codex require. Flat .md files at the top are not discoverable.
	if _, err := os.Stat(filepath.Join(a.skillsDir, "embedded-skill", "SKILL.md")); err != nil {
		t.Errorf("embedded skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.skillsDir, "rogue")); !os.IsNotExist(err) {
		t.Errorf("rogue skill leaked into app skills dir")
	}
}

func TestSyncSkillsUsesRepoSourceWhenGoModPresent(t *testing.T) {
	a := setupApp(t)

	repoDir := t.TempDir()
	a.repoDir = repoDir
	// Marker that this is the actual sybra source repo.
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	repoSkill := []byte("---\nname: repo-skill\ndescription: t\n---\n")
	if err := os.WriteFile(filepath.Join(skillsSrc, "repo-skill.md"), repoSkill, 0o644); err != nil {
		t.Fatal(err)
	}

	// Even with an embedded FS available, go.mod presence selects repo source.
	embeddedSrc := filepath.Join(t.TempDir(), "embedded")
	dataDir := filepath.Join(embeddedSrc, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "embed-only.md"), []byte("---\nname: embed-only\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.skillsFS = os.DirFS(embeddedSrc)
	a.skillsDir = filepath.Join(t.TempDir(), "app-skills")

	a.syncSkills()

	if _, err := os.Stat(filepath.Join(a.skillsDir, "repo-skill", "SKILL.md")); err != nil {
		t.Errorf("repo skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.skillsDir, "embed-only")); !os.IsNotExist(err) {
		t.Errorf("embedded skill should not be written in repo-source mode")
	}
}

func TestSyncSkillsDirMissingSrc(t *testing.T) {
	a := setupApp(t)
	dstDir := filepath.Join(t.TempDir(), "should-not-exist")
	a.syncSkillsDir("/nonexistent/dir", dstDir)

	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Error("dst dir should not be created when src missing")
	}
}

func TestSyncSkillsNoRepoDir(t *testing.T) {
	a := setupApp(t)
	a.repoDir = ""
	// Should not panic; falls back to cwd
	a.syncSkills()
}

func TestSyncSkillsWithRepoDir(t *testing.T) {
	a := setupApp(t)

	repoDir := t.TempDir()
	a.repoDir = repoDir

	// Create source skills dir
	skillsSrc := filepath.Join(repoDir, ".claude", "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	skillContent := []byte("---\nname: skill\ndescription: test\n---\n\n# skill")
	if err := os.WriteFile(filepath.Join(skillsSrc, "skill.md"), skillContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// Create orchestrator CLAUDE.md
	orchDir := filepath.Join(repoDir, "orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "CLAUDE.md"), []byte("# orchestrator"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should not panic
	a.syncSkills()
}

func TestShutdown(t *testing.T) {
	a := setupApp(t)
	a.Shutdown(t.Context())
}

func TestStartup(t *testing.T) {
	a := NewApp(discardLogger(), &slog.LevelVar{}, testConfig(t))
	if a.tasksDir == "" {
		t.Error("tasksDir should not be empty")
	}
}

func TestResolvePermission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		taskPerm *bool
		cfgPerm  *bool
		want     bool
	}{
		{"task false overrides config true", task.Ptr(false), task.Ptr(true), false},
		{"task true overrides config false", task.Ptr(true), task.Ptr(false), true},
		{"task nil falls back to config false", nil, task.Ptr(false), false},
		{"task nil falls back to config true", nil, task.Ptr(true), true},
		{"task nil config nil defaults true", nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tk := task.Task{RequirePermissions: tt.taskPerm}
			var cfg *config.Config
			if tt.cfgPerm != nil {
				cfg = &config.Config{Agent: config.AgentDefaults{RequirePermissions: tt.cfgPerm}}
			}
			if got := resolvePermission(tk, cfg); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveExecutionDebugAlwaysRequiresPermissions(t *testing.T) {
	t.Parallel()
	tk := task.Task{TaskType: task.TaskTypeDebug, RequirePermissions: task.Ptr(false)}
	// TaskTypeDebug hardcodes requirePerm=true regardless of task field.
	_, _, requirePerm, _ := resolveExecution(tk, "headless", "", nil)
	if !requirePerm {
		t.Error("debug task should always require permissions")
	}
}
