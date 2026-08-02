package workflow

import "testing"

// TestExecRunAgentSeedsSidecarDirBeforeRendering pins the ordering that makes
// the sidecar dir usable at all. Seeding it only after dispatch — which is
// where the route/dir bookkeeping otherwise lives — leaves the *first* run of
// a verifier role resolving {{sidecardir .Vars}} to the worktree, which that
// role cannot write. The sandbox denies the write and the artifact simply
// comes back empty, so this regresses silently rather than loudly.
//
// Dispatch is refused here so the run stops immediately after the seeding,
// which isolates the ordering from everything downstream.
func TestExecRunAgentSeedsSidecarDirBeforeRendering(t *testing.T) {
	t.Parallel()

	tasks := newMemTasks()
	agents := newMockAgents()
	agents.SetAdmitDispatch("pool busy")
	engine := NewEngine(newTestStore(t), tasks, agents, discardLogger())
	engine.SetSidecarDirResolver(func(string) (string, error) { return "/sandbox/t1", nil })

	step := &Step{ID: "review", Type: StepRunAgent}
	wfExec := &Execution{
		WorkflowID:  "wf",
		CurrentStep: "review",
		Variables:   map[string]string{WorkflowVarDir: "/worktree"},
	}
	ti := TaskInfo{ID: "t1", Status: "in-review", Workflow: wfExec}
	tasks.Put(ti)

	if err := engine.execRunAgent("t1", step, wfExec, TemplateContext{Task: ti, Step: *step, Workflow: wfExec}); err != nil {
		t.Fatalf("execRunAgent: %v", err)
	}

	got := wfExec.Variables[WorkflowVarSidecarDir]
	if got != "/sandbox/t1" {
		t.Fatalf("%s = %q, want %q — templates render before dispatch, so the sidecar dir has to be seeded first",
			WorkflowVarSidecarDir, got, "/sandbox/t1")
	}
	// The whole point is that it does not resolve to the worktree.
	if rendered := sidecarDirVar(wfExec.Variables); rendered == "/worktree" {
		t.Fatal("sidecardir resolved to the worktree; a read-only verifier role cannot write there")
	}
}
