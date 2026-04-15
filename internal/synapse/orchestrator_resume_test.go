//go:build !short

package synapse

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/synapse/internal/agent"
	"github.com/Automaat/synapse/internal/project"
	"github.com/Automaat/synapse/internal/task"
	"github.com/Automaat/synapse/internal/workflow"
	"github.com/Automaat/synapse/internal/worktree"
)

// TestOrchestrator_StartAgent_DoesNotResumeStaleSessionFromPriorWorkflow
// reproduces the production failure where an `implement` agent inherited
// a session_id from a long-finished prior workflow execution.
//
// Production timeline (task e1b18401):
//  1. Workflow ran triage → set_in_progress → implement. The implement agent
//     was launched via AgentOrchestrator.StartAgent, which records its
//     agent_run with an empty Role and a session_id.
//  2. The first implement attempt produced no commits and the workflow
//     stalled. ~25 minutes later the workflow was restarted from triage and
//     ran all the way through plan → critique → address → review → implement.
//  3. On the second implement step, pickImplementationResumeSession walked
//     AgentRuns newest-first looking for runs with empty/implementation Role
//     plus a session_id. It picked up the original empty-Role run from step 1
//     and passed `--resume <stale-session>` to claude.
//  4. Claude stderr: `No conversation found with session ID: bb7ebc41-...`,
//     exit status 1, cost $0, prompt never sent. verify_commits flipped the
//     task to human-required.
//
// The fix has two parts:
//   - AgentOrchestrator.StartAgent now records implementation agent runs
//     with Role explicitly set, so the empty-Role escape hatch in
//     pickImplementationResumeSession can be removed.
//   - pickImplementationResumeSession only considers runs whose StartedAt
//     is at or after the current workflow execution's StartedAt, so a
//     stale run from a prior workflow run cannot leak in even when the
//     role check would otherwise match.
//
// This test sets up a real bare git repo, a real project store, a real
// worktree manager, and the real AgentOrchestrator — then exercises the
// exact code path that misbehaved in production. It pre-seeds an aborted
// prior-execution agent_run, calls AgentOrchestrator.StartAgent, and
// asserts that the resulting fake-claude argv does NOT carry --resume
// pointing at the stale session.
func TestOrchestrator_StartAgent_DoesNotResumeStaleSessionFromPriorWorkflow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	binDir := buildTestBinaries(t)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_SCENARIO", "interactive_implement")

	argsLog := filepath.Join(t.TempDir(), "claude-args.log")
	t.Setenv("FAKE_CLAUDE_ARGS_LOG", argsLog)

	home, err := os.MkdirTemp("", "synapse-orch-resume-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("SYNAPSE_HOME", home)

	tasksDir := filepath.Join(home, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	taskStore, err := task.NewStore(tasksDir)
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(taskStore, nil)

	projStore, err := project.NewStore(
		filepath.Join(home, "projects"),
		filepath.Join(home, "clones"),
	)
	if err != nil {
		t.Fatal(err)
	}

	src := initSourceRepo(t)
	barePath := filepath.Join(home, "clones", "testowner", "testrepo.git")
	if err := os.MkdirAll(filepath.Dir(barePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.CloneBare(src, barePath); err != nil {
		t.Fatalf("clone bare: %v", err)
	}

	projYAML := `id: testowner/testrepo
name: testrepo
owner: testowner
repo: testrepo
url: ` + src + `
clone_path: ` + barePath + `
type: pet
created_at: 2025-01-01T00:00:00Z
updated_at: 2025-01-01T00:00:00Z
`
	if err := os.WriteFile(filepath.Join(home, "projects", "testowner--testrepo.yaml"), []byte(projYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	logDir := filepath.Join(home, "logs")
	_ = os.MkdirAll(logDir, 0o755)

	agentMgr := agent.NewManager(t.Context(), func(string, any) {}, logger, logDir)
	agentMgr.SetDefaultProvider("claude")

	wm := worktree.New(worktree.Config{
		WorktreesDir: filepath.Join(home, "worktrees"),
		Projects:     projStore,
		Tasks:        taskMgr,
		Logger:       logger,
		AgentChecker: agentMgr.HasRunningAgentForTask,
	})

	orch := newAgentOrchestrator(taskMgr, projStore, agentMgr, nil, logger, wm, nil)

	// Create the task with the project pre-assigned so PrepareForTask succeeds.
	created, err := taskMgr.Create("stale-resume guard", "", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskMgr.Update(created.ID, task.Update{
		ProjectID: task.Ptr("testowner/testrepo"),
	}); err != nil {
		t.Fatal(err)
	}

	// Park the task in a workflow execution that started "now". Anything
	// before that timestamp is by definition from a prior execution and
	// must not be eligible for --resume.
	workflowStart := time.Now()
	if _, err := taskMgr.UpdateMap(created.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "simple-task",
			CurrentStep: "implement",
			State:       workflow.ExecRunning,
			StartedAt:   workflowStart,
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Seed a prior-execution implementation run: empty Role, session_id
	// set, started 24 hours before the current workflow execution. This
	// is exactly what the bf0c5e34 task in production looked like before
	// the second implement step picked up bb7ebc41-... by mistake.
	const staleSession = "stale-session-from-prior-workflow"
	if err := taskMgr.AddRun(created.ID, task.AgentRun{
		AgentID:   "ancient-impl",
		Mode:      "interactive",
		Provider:  "claude",
		State:     "stopped",
		StartedAt: workflowStart.Add(-24 * time.Hour),
		SessionID: staleSession,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := orch.StartAgent(created.ID, "interactive", "Implement the feature.", true); err != nil {
		t.Fatalf("orchestrator StartAgent: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, statErr := os.Stat(argsLog); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake-claude args log never written")
		}
		time.Sleep(50 * time.Millisecond)
	}

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	args := string(data)
	if !strings.Contains(args, "--input-format") {
		t.Fatalf("args log does not look like an interactive claude invocation; got:\n%s", args)
	}
	if strings.Contains(args, staleSession) {
		t.Fatalf("implement agent resumed stale cross-workflow session %q. argv:\n%s", staleSession, args)
	}
}
