package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Automaat/sybra/internal/project"
)

// newPRWorktree sets up a bare "origin" clone plus a worktree checked out on
// branch, mirroring how a real task worktree is prepared — pushing into it
// (unlike a plain non-bare "self origin") is always safe, since bare repos
// have no checked-out branch to refuse updating.
func newPRWorktree(t *testing.T, branch string) (bare, wtPath string) {
	t.Helper()
	src := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(src, "init", "-b", "main")
	run(src, "config", "user.email", "test@test.com")
	run(src, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", "README.md")
	run(src, "commit", "-m", "init")

	bare = filepath.Join(t.TempDir(), "bare.git")
	if err := project.CloneBare(context.Background(), src, bare); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	wtPath = filepath.Join(t.TempDir(), "wt")
	if err := project.CreateWorktree(context.Background(), bare, wtPath, branch, "main"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	run(wtPath, "config", "user.email", "test@test.com")
	run(wtPath, "config", "user.name", "Test")
	return bare, wtPath
}

func commitFile(t *testing.T, wtPath, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wtPath, name), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", name},
		{"commit", "-m", msg},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = wtPath
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func headSHA(t *testing.T, wtPath string) string {
	t.Helper()
	sha, err := project.CurrentCommit(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("CurrentCommit: %v", err)
	}
	return sha
}

func newPushBranchStep() *Step { return &Step{ID: "push_existing_pr", Type: StepPushBranch} }
func newCreatePRStep() *Step   { return &Step{ID: "create_pr", Type: StepCreatePR} }

type fakePRHeadFetcher struct {
	sha string
	err error
}

func (f *fakePRHeadFetcher) FetchPRHeadSHA(context.Context, string, int) (string, error) {
	return f.sha, f.err
}

type fakePRCreator struct {
	number  int
	headSHA string
	err     error
	gotReq  PRCreateRequest
}

func (f *fakePRCreator) CreatePR(_ context.Context, _ string, req PRCreateRequest) (number int, headSHA string, err error) {
	f.gotReq = req
	if f.err != nil {
		return 0, "", f.err
	}
	return f.number, f.headSHA, nil
}

type fakePRFinder struct {
	number int
	found  bool
	err    error
	calls  int
}

func (f *fakePRFinder) FindPRForBranch(context.Context, string, string) (number int, found bool, err error) {
	f.calls++
	return f.number, f.found, f.err
}

type fakePRContentGenerator struct {
	title, body string
	err         error
}

func (f *fakePRContentGenerator) GeneratePRContent(context.Context, string, string, []string) (title, body string, err error) {
	return f.title, f.body, f.err
}

func TestExecPushBranch_Success(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")
	commitFile(t, wtPath, "change.txt", "feat: task work")
	local := headSHA(t, wtPath)

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5, ProjectID: "acme/widgets"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRHeadFetcher(&fakePRHeadFetcher{sha: local})

	out, err := engine.execPushBranch("t1", newPushBranchStep(), &Execution{Variables: map[string]string{}}, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5, ProjectID: "acme/widgets"})
	if err != nil {
		t.Fatalf("execPushBranch: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("task status = %q, want unchanged ready-pr", ti.Status)
	}
}

func TestExecPushBranch_NoWorktreeFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "main", PRNumber: 5})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{ok: false})

	out, err := engine.execPushBranch("t1", newPushBranchStep(), &Execution{Variables: map[string]string{}}, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "main", PRNumber: 5})
	if err != nil {
		t.Fatalf("execPushBranch: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

func TestExecPushBranch_DivergedFlipsHumanRequired(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")

	// Establish a baseline remote tracking ref for feat/existing-pr, then
	// rewrite local history so it diverges from what's on the remote —
	// mirrors project.TestPushSync_DivergenceReturnsErrorNoForce. A second
	// clone isn't needed: pushing directly into a bare repo's own branch that
	// a linked worktree has checked out is itself rejected by git, the same
	// "cannot push into a currently checked-out branch" protection this test
	// would otherwise be fighting.
	commitFile(t, wtPath, "one.txt", "one")
	commitFile(t, wtPath, "two.txt", "two")
	runGit(t, wtPath, "push", "-u", "origin", "feat/existing-pr")

	runGit(t, wtPath, "reset", "--hard", "HEAD~1")
	commitFile(t, wtPath, "two-prime.txt", "two-prime")

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})

	out, err := engine.execPushBranch("t1", newPushBranchStep(), &Execution{Variables: map[string]string{}}, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5})
	if err != nil {
		t.Fatalf("execPushBranch: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); reason == "" {
		t.Error("expected a non-empty human-required reason")
	}
}

func TestExecPushBranch_DivergedRecoveredParksInsteadOfHumanRequired(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")

	commitFile(t, wtPath, "one.txt", "one")
	commitFile(t, wtPath, "two.txt", "two")
	runGit(t, wtPath, "push", "-u", "origin", "feat/existing-pr")

	runGit(t, wtPath, "reset", "--hard", "HEAD~1")
	commitFile(t, wtPath, "two-prime.txt", "two-prime")

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})

	var recoveredTaskID string
	engine.SetConflictRecovery(func(taskID string) bool {
		recoveredTaskID = taskID
		return true
	})

	out, err := engine.execPushBranch("t1", newPushBranchStep(), &Execution{Variables: map[string]string{}}, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if out != (StepOutput{}) {
		t.Fatalf("out = %+v, want zero value", out)
	}
	if recoveredTaskID != "t1" {
		t.Errorf("conflictRecovery called with %q, want t1", recoveredTaskID)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Errorf("task status = %q, want unchanged ready-pr (recovery owns the transition)", ti.Status)
	}
}

func TestExecPushBranch_DivergedRecoveryDeclinesFallsBackToHumanRequired(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")

	commitFile(t, wtPath, "one.txt", "one")
	commitFile(t, wtPath, "two.txt", "two")
	runGit(t, wtPath, "push", "-u", "origin", "feat/existing-pr")

	runGit(t, wtPath, "reset", "--hard", "HEAD~1")
	commitFile(t, wtPath, "two-prime.txt", "two-prime")

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetConflictRecovery(func(string) bool { return false })

	out, err := engine.execPushBranch("t1", newPushBranchStep(), &Execution{Variables: map[string]string{}}, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5})
	if err != nil {
		t.Fatalf("execPushBranch: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

// TestConflictRecovery_DeferredWhileMarkerHeld locks the reentrancy fix: when a
// per-task starting/dispatching marker is held (push_branch/create_pr reached
// via DispatchEvent/startWorkflowLocked or a resume re-dispatch), the recovery
// callback must NOT run inline — doing so re-enters StartWorkflow*/DispatchEvent
// against the held marker and is silently rejected. It must be queued and run
// only once drainPendingConflictRecovery fires after the marker releases.
func TestConflictRecovery_DeferredWhileMarkerHeld(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	var calls int
	engine.SetConflictRecovery(func(string) bool { calls++; return true })

	// Simulate running inside DispatchEvent: the dispatching marker is held.
	engine.mu.Lock()
	engine.dispatching["t1"] = struct{}{}
	engine.mu.Unlock()

	if parked := engine.tryConflictRecovery("t1"); !parked {
		t.Fatal("tryConflictRecovery = false, want true (park while marker held)")
	}
	if calls != 0 {
		t.Fatalf("recovery invoked inline under held marker (calls=%d); want deferred", calls)
	}

	// Release the marker the way real callers do (alongside fireComplete)
	// before draining — drainPendingConflictRecovery trusts the caller to
	// have released it and does not re-check, so draining while still held
	// would re-enter the very reentrancy trap this test guards against.
	engine.mu.Lock()
	delete(engine.dispatching, "t1")
	engine.mu.Unlock()

	engine.drainPendingConflictRecovery("t1")
	if calls != 1 {
		t.Fatalf("recovery calls=%d after drain; want exactly 1", calls)
	}
}

// TestDrainPendingConflictRecovery_DeclineEscalates verifies the deferred path's
// fallback: when the queued recovery declines, the task lands human-required and
// its parked workflow is terminated — the same terminal outcome the inline path
// produces.
func TestDrainPendingConflictRecovery_DeclineEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:     "t1",
		Status: "ready-pr",
		Workflow: &Execution{
			WorkflowID:  "simple-task-pr",
			CurrentStep: "push_branch",
			State:       ExecRunning,
		},
	})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetConflictRecovery(func(string) bool { return false })

	engine.mu.Lock()
	engine.pendingRecovery["t1"] = struct{}{}
	engine.mu.Unlock()

	engine.drainPendingConflictRecovery("t1")

	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required", ti.Status)
	}
	if ti.Workflow == nil || ti.Workflow.State != ExecCompleted {
		t.Errorf("workflow state = %v, want ExecCompleted (parked workflow terminated)", ti.Workflow)
	}
}

// TestTryConflictRecovery_ExportedWrapperQueuesWhileMarkerHeld locks the fix
// for callers outside this package (agentorch's worktree-prep rebase-failure
// path, the fix-review push-divergence handler) that invoke the same
// recovery callback as push_branch/create_pr but from outside a workflow
// step — e.g. a rebase conflict discovered deep inside execRunAgent's own
// worktree prep, still within the same synchronous StartWorkflow call that
// holds the starting marker. TryConflictRecovery must defer exactly like the
// unexported tryConflictRecovery push_branch/create_pr already uses.
func TestTryConflictRecovery_ExportedWrapperQueuesWhileMarkerHeld(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	var calls int
	engine.SetConflictRecovery(func(string) bool { calls++; return true })

	// Simulate running inside the same synchronous StartWorkflow call that
	// holds the starting marker (e.g. restart-stale's raw
	// StartWorkflow(implement) reentering via a worktree-prep rebase failure).
	engine.mu.Lock()
	engine.starting["t1"] = struct{}{}
	engine.mu.Unlock()

	if recovered := engine.TryConflictRecovery("t1"); !recovered {
		t.Fatal("TryConflictRecovery = false, want true (queued while marker held)")
	}
	if calls != 0 {
		t.Fatalf("recovery invoked inline under held marker (calls=%d); want deferred", calls)
	}

	engine.mu.Lock()
	delete(engine.starting, "t1")
	engine.mu.Unlock()

	engine.drainPendingConflictRecovery("t1")
	if calls != 1 {
		t.Fatalf("recovery calls=%d after drain; want exactly 1", calls)
	}
}

// TestTryConflictRecovery_NilSafe mirrors RecoverStaleBranchConflict's own
// nil-receiver guard: wiring TryConflictRecovery as a callback ahead of a
// possibly-nil *Engine during degraded init must not panic.
func TestTryConflictRecovery_NilSafe(t *testing.T) {
	var engine *Engine
	if engine.TryConflictRecovery("t1") {
		t.Fatal("TryConflictRecovery on nil engine = true, want false")
	}
}

// TestTryConflictRecovery_NoRecoveryWiredReturnsFalse ensures a nil
// conflictRecovery callback (recovery never wired) is reported as declined,
// not queued or panicked.
func TestTryConflictRecovery_NoRecoveryWiredReturnsFalse(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	if engine.TryConflictRecovery("t1") {
		t.Fatal("TryConflictRecovery with no recovery wired = true, want false")
	}
}

// TestQueueConflictRecoveryRetry_DrainedByLaterMarkerRelease locks the fix
// for dispatchBranchConflictRecovery's own re-dispatch call: when its
// StartWorkflowWithVars hits ErrWorkflowAlreadyActive because a concurrent
// StartWorkflow call grabbed the marker sometime during the caller's own
// (multi-second) worktree-prep work — a TOCTOU window TryConflictRecovery's
// upfront check alone cannot close — QueueConflictRecoveryRetry must defer a
// retry that drainPendingConflictRecovery picks up once that call's marker
// releases, exactly like a push_branch/create_pr divergence would.
func TestQueueConflictRecoveryRetry_DrainedByLaterMarkerRelease(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	var calls int
	engine.SetConflictRecovery(func(string) bool { calls++; return true })

	engine.QueueConflictRecoveryRetry("t1")
	if calls != 0 {
		t.Fatalf("recovery invoked before any marker released (calls=%d); want deferred", calls)
	}

	engine.drainPendingConflictRecovery("t1")
	if calls != 1 {
		t.Fatalf("recovery calls=%d after drain; want exactly 1", calls)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestExecCreatePR_Success(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	creator := &fakePRCreator{number: 42, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)
	engine.SetPRContentGenerator(&fakePRContentGenerator{title: "feat(x): y", body: "## Motivation\n\nz\n\n## Implementation information\n\nw"})

	out, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, output = %q", out.Status, out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", ti.PRNumber)
	}
	if ti.Status == "human-required" {
		t.Errorf("task should not be human-required: %s", tasks.Reason("t1"))
	}
	if creator.gotReq.Repo != "acme/widgets" {
		t.Errorf("CreatePR repo = %q, want acme/widgets", creator.gotReq.Repo)
	}
	if creator.gotReq.Head != "feat/my-branch" {
		t.Errorf("CreatePR head = %q, want feat/my-branch", creator.gotReq.Head)
	}
	if creator.gotReq.Draft {
		t.Errorf("CreatePR draft = true, want false for pet project")
	}
	if creator.gotReq.Title != "feat(x): y" {
		t.Errorf("CreatePR title = %q", creator.gotReq.Title)
	}
}

func TestExecCreatePR_ExistingPRShortCircuitsWithoutCreating(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	finder := &fakePRFinder{number: 77, found: true}
	engine.SetPRFinder(finder)
	// A creator IS wired so the test proves the guard short-circuits BEFORE
	// reaching it, not that creation was merely unconfigured.
	creator := &fakePRCreator{number: 999, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)

	out, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, output = %q", out.Status, out.Output)
	}
	if finder.calls != 1 {
		t.Errorf("finder calls = %d, want 1", finder.calls)
	}
	if creator.gotReq.Repo != "" {
		t.Error("CreatePR must not be called when an existing PR is found")
	}
	ti, _ := tasks.GetTask("t1")
	if ti.PRNumber != 77 {
		t.Errorf("PRNumber = %d, want 77 (linked from existing PR)", ti.PRNumber)
	}
	if ti.Status == "human-required" {
		t.Errorf("task should not be human-required: %s", tasks.Reason("t1"))
	}
}

func TestExecCreatePR_FinderErrorFallsThroughToCreate(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	// A lookup failure must be treated as "no PR found" so create_pr proceeds
	// rather than getting stuck — matching the best-effort docstring.
	engine.SetPRFinder(&fakePRFinder{err: errors.New("gh unreachable")})
	creator := &fakePRCreator{number: 42, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)

	out, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, output = %q", out.Status, out.Output)
	}
	if creator.gotReq.Repo == "" {
		t.Error("CreatePR must be called when the finder errors (best-effort)")
	}
	ti, _ := tasks.GetTask("t1")
	if ti.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", ti.PRNumber)
	}
}

func TestExecCreatePR_DraftForNonPetProject(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "work", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	creator := &fakePRCreator{number: 7, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)
	engine.SetPRContentGenerator(&fakePRContentGenerator{title: "feat(x): y", body: "## Motivation\n\nz\n\n## Implementation information\n\nw"})

	if _, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task); err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if !creator.gotReq.Draft {
		t.Errorf("CreatePR draft = false, want true for non-pet project")
	}
}

func TestExecCreatePR_ContentFallbackWhenGeneratorUnset(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body text"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	creator := &fakePRCreator{number: 9, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)
	// No SetPRContentGenerator call — engine must fall back.

	if _, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task); err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if creator.gotReq.Title != "feat(x): y" {
		t.Errorf("fallback title = %q, want task title", creator.gotReq.Title)
	}
	if creator.gotReq.Body == "" {
		t.Error("fallback body must not be empty")
	}
}

func TestExecCreatePR_NoCreatorFlipsHumanRequired(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	// No PRCreator wired.

	if _, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task); err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

func TestExecCreatePR_NoProjectFlipsHumanRequired(t *testing.T) {
	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	if _, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task); err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
}

func TestExecCreatePR_CreateErrorRateLimitParksForRetry(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRCreator(&fakePRCreator{err: errors.New("GitHub API rate limit exceeded")})
	engine.SetPRContentGenerator(&fakePRContentGenerator{title: "t", body: "## Motivation\n\n## Implementation information\n"})

	wfExec := &Execution{Variables: map[string]string{}}
	_, err := engine.execCreatePR("t1", newCreatePRStep(), wfExec, task)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Errorf("rate-limited create should retry, not escalate: %s", tasks.Reason("t1"))
	}
}

func TestClassifyPRGitError_AuthFailureEscalatesAfterMaxRetries(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	step := newCreatePRStep()
	task := TaskInfo{ID: "t1", Status: "ready-pr"}

	wfExec := &Execution{Variables: map[string]string{}}
	authErr := errors.New("gh: 401 Unauthorized (bad credentials)")
	for i := range maxPRCreateAuthRetries {
		_, err := engine.classifyPRGitError("t1", step, wfExec, task, authErr, "create_pr")
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d: err = %v, want errStepParked", i, err)
		}
	}
	if _, err := engine.classifyPRGitError("t1", step, wfExec, task, authErr, "create_pr"); err != nil {
		t.Fatalf("final attempt: unexpected error %v", err)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required after exhausting auth retries", ti.Status)
	}
}
