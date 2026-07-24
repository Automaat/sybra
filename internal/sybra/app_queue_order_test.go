package sybra

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Automaat/sybra/internal/agentqueue"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

type recordingAgentLauncher struct {
	calls []string
}

func (l *recordingAgentLauncher) StartAgent(taskID, role, mode, model, provider, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	l.calls = append(l.calls, taskID)
	return taskID + "-agent", dir, "", nil
}

func (*recordingAgentLauncher) TryClaimDispatch(string) (workflow.DispatchClaim, bool) {
	return staticDispatchClaim{}, true
}

func (*recordingAgentLauncher) HasRunningAgent(string) bool                     { return false }
func (*recordingAgentLauncher) HasOtherRunningAgentForTask(string, string) bool { return false }
func (*recordingAgentLauncher) FindRunningAgentForRole(string, string) (string, bool) {
	return "", false
}
func (*recordingAgentLauncher) StopAgentsForTask(string, string) {}
func (*recordingAgentLauncher) SendPrompt(string, string) error  { return nil }
func (*recordingAgentLauncher) DefaultProvider() string          { return "claude" }
func (*recordingAgentLauncher) ProviderRateLimited(string) bool  { return false }
func (*recordingAgentLauncher) ProviderCanFailover(string) bool  { return false }
func (*recordingAgentLauncher) ProviderHealthy(string) bool      { return true }
func (*recordingAgentLauncher) IsDispatching(string) bool        { return false }
func (*recordingAgentLauncher) AdmitDispatch(string, string, string) (admit bool, reason string) {
	return true, ""
}

type staticDispatchClaim struct{}

func (staticDispatchClaim) Release() {}

// TestInitWorkflowEngine_QueueComparatorPrefersQueuedTask reproduces the
// ae75c205 failure mode: ResumeStalled compares a real queued item against an
// unqueued stopped blocker using agentqueue.Less via app wiring. The queued
// task must dispatch first so the blocker cannot immediately reclaim the slot.
func TestInitWorkflowEngine_QueueComparatorPrefersQueuedTask(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SYBRA_HOME", home)

	store, err := task.NewStore(filepath.Join(home, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	taskMgr := task.NewManager(store, nil)

	q, err := agentqueue.New(filepath.Join(home, "agentqueue"), agentqueue.Options{}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	launcher := &recordingAgentLauncher{}

	engine := workflow.NewEngine(
		mustWorkflowStoreWithTestSimple(t, home),
		&taskAdapter{tasks: taskMgr},
		launcher,
		discardLogger(),
	)
	engine.SetDispatchComparator(func() func(x, y workflow.TaskInfo) int {
		snap := q.Snapshot()
		queued := make(map[string]agentqueue.Item, len(snap))
		for _, it := range snap {
			queued[it.TaskID] = it
		}
		toItem := func(t workflow.TaskInfo) agentqueue.Item {
			it := agentqueue.Item{TaskID: t.ID, Priority: task.Priority(t.Priority), Status: task.Status(t.Status)}
			if qit, ok := queued[t.ID]; ok {
				it.Manual = qit.Manual
				it.Enqueued = qit.Enqueued
			}
			return it
		}
		return func(x, y workflow.TaskInfo) int {
			ai, bi := toItem(x), toItem(y)
			switch {
			case agentqueue.Less(ai, bi):
				return -1
			case agentqueue.Less(bi, ai):
				return 1
			default:
				return 0
			}
		}
	})

	blocker := queueOrderTask(t, taskMgr, "blocker")
	queuedTask := queueOrderTask(t, taskMgr, "queued")
	if !q.Offer(agentqueue.Item{TaskID: queuedTask.ID, Priority: task.PriorityNone, Status: task.StatusTodo}) {
		t.Fatal("Offer(queued task) = false, want true")
	}

	engine.ResumeStalled()

	want := []string{queuedTask.ID, blocker.ID}
	if !slices.Equal(launcher.calls, want) {
		t.Fatalf("dispatch order = %v, want %v", launcher.calls, want)
	}
}

func mustWorkflowStoreWithTestSimple(t *testing.T, home string) *workflow.Store {
	t.Helper()
	wfDir := filepath.Join(home, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.NewStore(wfDir)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("../../internal/workflow/testdata/test-simple.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "test-simple.yaml"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	return store
}

func queueOrderTask(t *testing.T, mgr *task.Manager, title string) task.Task {
	t.Helper()
	created, err := mgr.Create(title, "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := mgr.UpdateMap(created.ID, map[string]any{
		"workflow": &workflow.Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "implement",
			State:       workflow.ExecWaiting,
			Variables:   map[string]string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
