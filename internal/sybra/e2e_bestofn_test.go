//go:build e2e

package sybra

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/sybra/agentorch"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// bestOfNE2EBareRepo creates a bare git repo (with one commit on `main`) that
// best-of-n attempts branch off, mirroring
// internal/worktree.initBareWithCommitReturnSrc.
func bestOfNE2EBareRepo(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", "-b", "main", src},
		{"git", "-C", src, "config", "user.email", "test@test.com"},
		{"git", "-C", src, "config", "user.name", "Test"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# best-of-n e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", src, "add", "."},
		{"git", "-C", src, "commit", "-m", "init"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}

	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "clone", "--bare", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*").CombinedOutput(); err != nil {
		t.Fatalf("git fetch: %v: %s", err, out)
	}
	return bare
}

// bestOfNTestWorkflow is a minimal best_of_n -> judge -> promote definition,
// analogous to the builtin simple-task-best-of-n-implement.yaml but trimmed
// to what this smoke test needs to observe (skips verify_commits/tamper/
// checks — those are exercised by the existing non-best-of-n e2e suite).
func bestOfNTestWorkflowDef() workflow.Definition {
	return workflow.Definition{
		ID:      "e2e-best-of-n",
		Trigger: workflow.Trigger{On: "manual"},
		Steps: []workflow.Step{
			{
				ID:   "attempts",
				Type: workflow.StepBestOfN,
				Config: workflow.StepConfig{
					Role:     "implementation",
					Mode:     "headless",
					Prompt:   "implement {{.Task.ID}}",
					Attempts: 2,
				},
				Next: []workflow.Transition{
					{When: &workflow.Condition{Field: "task.status", Operator: "equals", Value: "human-required"}, GoTo: ""},
					{GoTo: "judge"},
				},
			},
			{
				ID:   "judge",
				Type: workflow.StepRunAgent,
				Config: workflow.StepConfig{
					Role:   "review",
					Mode:   "headless",
					Prompt: "judge {{.Task.ID}}",
				},
				Next: []workflow.Transition{
					{When: &workflow.Condition{Field: "task.status", Operator: "equals", Value: "human-required"}, GoTo: ""},
					{GoTo: "promote"},
				},
			},
			{
				ID:   "promote",
				Type: workflow.StepPromoteBestOfN,
				Config: workflow.StepConfig{
					JudgeStep:   "judge",
					BestOfNStep: "attempts",
				},
				Next: []workflow.Transition{
					{When: &workflow.Condition{Field: "task.status", Operator: "equals", Value: "human-required"}, GoTo: ""},
					{GoTo: ""},
				},
			},
		},
	}
}

// TestE2E_BestOfN_PromotesWinnerCleansUpLosers is the fake-provider smoke
// test for the whole best-of-N pipeline, driven through the real production
// wiring (agent.Manager, agentorch.Orchestrator, worktree.Manager,
// workflow.Engine, the real agentAdapter/attemptWorktreeAdapter) rather than
// engine-level fakes. It proves the three end-to-end guarantees unit tests
// can't: (a) only the judged winner's commit lands on the canonical branch,
// (b) the loser's commit is absent from canonical history, (c) both attempt
// worktree directories are gone from disk after promotion.
func TestE2E_BestOfN_PromotesWinnerCleansUpLosers(t *testing.T) {
	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	// Both attempts share the "best_of_n_attempt" scenario (it derives a
	// unique filename from its own cwd, so dispatch order between the two
	// concurrently-running attempt processes never matters); the judge only
	// ever dispatches once both attempts have terminated, so it's guaranteed
	// to pop the 3rd line. A scenario FILE (not a mid-test env var swap) is
	// required here — a mid-test os.Setenv would race with the attempt
	// subprocesses' concurrent os.Environ() reads under -race.
	scenarioFile := filepath.Join(t.TempDir(), "scenarios.txt")
	if err := os.WriteFile(scenarioFile, []byte("best_of_n_attempt\nbest_of_n_attempt\nbest_of_n_judge\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_SCENARIO_FILE", scenarioFile)
	t.Setenv("FAKE_CODEX_SCENARIO_FILE", scenarioFile)

	taskDir, err := os.MkdirTemp("", "sybra-e2e-bestofn-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(taskDir) })
	t.Setenv("SYBRA_HOME", taskDir)
	tasksDir := filepath.Join(taskDir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	logger := e2eLogger()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	logDir, err := os.MkdirTemp("", "sybra-e2e-bestofn-logs-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })

	bare := bestOfNE2EBareRepo(t)
	projectsDir, err := os.MkdirTemp("", "sybra-e2e-bestofn-projects-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(projectsDir) })
	projStore, err := project.NewStore(filepath.Join(projectsDir, "meta"), filepath.Join(projectsDir, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	proj := project.Project{
		ID: "test/bestofn", Name: "bestofn", Owner: "test", Repo: "bestofn",
		URL: bare, ClonePath: bare, Type: project.ProjectTypePet, Status: project.ProjectStatusReady,
	}
	seedExperienceProject(t, filepath.Join(projectsDir, "meta"), proj)
	proj, err = projStore.Get(proj.ID)
	if err != nil {
		t.Fatalf("reload seeded project: %v", err)
	}

	// engine is assigned below, after the worktree/agentorch wiring it depends
	// on; onAgentComplete's closure captures the variable, not its (not yet
	// set) value, so wiring OnComplete here before engine exists is safe.
	var engine *workflow.Engine
	onAgentComplete := func(ag *agent.Agent) {
		var result string
		var hasResult bool
		output := ag.Output()
		for i := range output {
			if output[i].Type == "result" {
				result = output[i].Content
				hasResult = true
			}
		}
		if hasResult && result == "" {
			result = lastAssistantText(ag)
		}
		engine.HandleAgentComplete(ag.TaskID, workflow.AgentCompletion{
			AgentID:  ag.ID,
			Result:   result,
			Success:  ag.GetExitErr() == nil,
			Provider: ag.Provider,
		})
	}
	agentMgr := newTestAgentManager(t, ctx, func(string, any) {}, logger, logDir, agent.ManagerConfig{
		Runtime:     agent.ManagerRuntimeConfig{DefaultProvider: "claude"},
		ControlHome: taskDir,
		OnComplete:  onAgentComplete,
	})

	wtDir, err := os.MkdirTemp("", "sybra-e2e-bestofn-wt-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wtDir) })
	wm := worktree.New(worktree.Config{
		WorktreesDir: wtDir,
		Projects:     projStore,
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})
	agentOrch := agentorch.New(taskMgr, projStore, agentMgr, nil, logger, wm, nil)

	wfDir, err := os.MkdirTemp("", "sybra-e2e-bestofn-wf-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wfDir) })
	wfStore, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := wfStore.Save(bestOfNTestWorkflowDef()); err != nil {
		t.Fatalf("save workflow: %v", err)
	}

	artifactsDir, err := os.MkdirTemp("", "sybra-e2e-bestofn-artifacts-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(artifactsDir) })
	artifactStore := artifact.New(artifactsDir)

	ta := &taskAdapter{tasks: taskMgr, projects: projStore}
	aa := &agentAdapter{agents: agentMgr, agentOrch: agentOrch, tasks: taskMgr, projects: projStore}
	awa := &attemptWorktreeAdapter{tasks: taskMgr, mgr: wm}
	ara := &artifactRecorderAdapter{store: artifactStore}

	engine = workflow.NewEngine(wfStore, ta, aa, logger)
	engine.SetWorktreeGetter(&worktreeGetterAdapter{tasks: taskMgr, mgr: wm})
	engine.SetAttemptWorktreeManager(awa)
	engine.SetCostBudgetChecker(aa)
	engine.SetArtifactRecorder(ara)
	engine.SetOnComplete(func(info workflow.CompletionInfo) {
		t, gErr := taskMgr.Get(info.TaskID)
		if gErr != nil || task.IsTerminalStatus(t.Status) {
			return
		}
		_, _ = engine.DispatchEvent(
			info.TaskID,
			"task.status_changed",
			map[string]string{"task.status": string(t.Status)},
			map[string]string{workflow.WorkflowVarDir: ""},
		)
	})

	tk, err := taskMgr.Store().Create("best-of-n e2e task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	trueBool := true
	if _, err := taskMgr.Update(tk.ID, task.Update{
		ProjectID: task.Ptr(proj.ID),
		Tags:      task.Ptr([]string{"best-of-n"}),
		Reviewed:  &trueBool,
	}); err != nil {
		t.Fatal(err)
	}

	if err := engine.StartWorkflow(tk.ID, "e2e-best-of-n"); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	waitFor(t, 20*time.Second, "workflow terminates", func() bool {
		got, gErr := taskMgr.Get(tk.ID)
		if gErr != nil {
			return false
		}
		return got.Workflow == nil || got.Workflow.CurrentStep == "" || got.Status == "human-required"
	})

	final, err := taskMgr.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status == "human-required" {
		t.Fatalf("workflow escalated to human-required: %s", final.StatusReason)
	}

	canonicalDir := wm.PathFor(final)
	if _, statErr := os.Stat(canonicalDir); statErr != nil {
		t.Fatalf("canonical worktree missing after promotion: %v", statErr)
	}

	// (a) + (b): only attempt_2 (the judged winner)'s committed file is on
	// the canonical branch; attempt_1's is absent, both from the working
	// tree and from history.
	winnerFile := "attempt-" + tk.DirName() + "-attempt_2.txt"
	loserFile := "attempt-" + tk.DirName() + "-attempt_1.txt"

	if _, statErr := os.Stat(filepath.Join(canonicalDir, winnerFile)); statErr != nil {
		t.Errorf("winner file %q missing from canonical worktree: %v", winnerFile, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(canonicalDir, loserFile)); statErr == nil {
		t.Errorf("loser file %q present in canonical worktree — losing attempt leaked onto canonical branch", loserFile)
	}

	out, err := exec.Command("git", "-C", canonicalDir, "log", "--name-only", "--format=").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	history := string(out)
	if !strings.Contains(history, winnerFile) {
		t.Fatalf("winner file %q missing from canonical branch history:\n%s", winnerFile, history)
	}
	if strings.Contains(history, loserFile) {
		t.Fatalf("loser file %q appears in canonical branch history:\n%s", loserFile, history)
	}

	// (c): both attempt worktree dirs are gone from disk.
	for _, id := range []string{"attempt_1", "attempt_2"} {
		attemptDir := filepath.Join(wtDir, tk.DirName()+"-"+id)
		if _, statErr := os.Stat(attemptDir); !os.IsNotExist(statErr) {
			t.Errorf("attempt dir %q still exists after promotion (stat err=%v)", attemptDir, statErr)
		}
	}
}
