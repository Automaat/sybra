package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

type fakePRAnyStateFinder struct {
	number int
	state  string
	found  bool
	err    error
	calls  int
}

func (f *fakePRAnyStateFinder) FindPRForBranchAnyState(context.Context, string, string) (number int, state string, found bool, err error) {
	f.calls++
	return f.number, f.state, f.found, f.err
}

type fakePRCloser struct {
	err      error
	calls    int
	repo     string
	number   int
	comments []string
}

func (f *fakePRCloser) ClosePR(_ context.Context, repo string, number int, comment string) error {
	f.calls++
	f.repo = repo
	f.number = number
	f.comments = append(f.comments, comment)
	return f.err
}

type fakePRContentGenerator struct {
	title, body string
	err         error
}

func (f *fakePRContentGenerator) GeneratePRContent(context.Context, string, string, []string) (title, body string, err error) {
	return f.title, f.body, f.err
}

type fakePushPreflighter struct {
	err   error
	errs  []error
	calls int
	paths []string
}

func (f *fakePushPreflighter) PreflightPushCredentials(_ context.Context, wtPath string) error {
	f.calls++
	f.paths = append(f.paths, wtPath)
	if f.calls <= len(f.errs) {
		return f.errs[f.calls-1]
	}
	return f.err
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

// TestExecPushBranch_PreflightFailureFallsThroughToSuccessfulPush covers the
// #2386 false-block: the dry-run preflight probe can fail transiently (e.g. a
// stale app-token cache) even though the real push would succeed. The step
// must attempt the real push instead of parking on the probe alone.
func TestExecPushBranch_PreflightFailureFallsThroughToSuccessfulPush(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5, ProjectID: "acme/widgets"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	preflight := &fakePushPreflighter{err: errors.New("github push credential preflight failed: gh auth status: Bad credentials")}
	engine.SetPushCredentialPreflighter(preflight)

	wfExec := &Execution{Variables: map[string]string{}}
	out, err := engine.execPushBranch("t1", newPushBranchStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5, ProjectID: "acme/widgets"})
	if err != nil {
		t.Fatalf("execPushBranch: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if preflight.calls != 1 {
		t.Fatalf("preflight calls = %d, want 1", preflight.calls)
	}
	if got := preflight.paths[0]; got != wtPath {
		t.Fatalf("preflight path = %q, want %q", got, wtPath)
	}
	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "ready-pr" {
		t.Fatalf("status = %q, want unchanged ready-pr (push succeeded despite the failed probe)", ti.Status)
	}
}

// TestExecPushBranch_PreflightAndPushBothFailParksForRetry covers the other
// half of #2386: a preflight failure must not be the last word either — it's
// only the real push's outcome that decides retry/escalation.
func TestExecPushBranch_PreflightAndPushBothFailParksForRetry(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/existing-pr")
	commitFile(t, wtPath, "change.txt", "feat: task work")
	runGit(t, wtPath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "does-not-exist.git"))

	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5, ProjectID: "acme/widgets"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	preflight := &fakePushPreflighter{err: errors.New("github push credential preflight failed: gh auth status: Bad credentials")}
	engine.SetPushCredentialPreflighter(preflight)

	wfExec := &Execution{Variables: map[string]string{}}
	_, err := engine.execPushBranch("t1", newPushBranchStep(), wfExec, TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/existing-pr", PRNumber: 5, ProjectID: "acme/widgets"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if preflight.calls != 1 {
		t.Fatalf("preflight calls = %d, want 1", preflight.calls)
	}
	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.Status != "ready-pr" {
		t.Fatalf("status = %q, want ready-pr (parked)", ti.Status)
	}
	// Classification must be driven by the real push's error, not the
	// preflight probe's — the push-retry counter increments, not the
	// preflight-specific auth-retry counter.
	if wfExec.Variables[prPushAttemptsVar] != "1" {
		t.Fatalf("%s = %q, want 1", prPushAttemptsVar, wfExec.Variables[prPushAttemptsVar])
	}
	if wfExec.Variables[prCreateAuthAttemptsVar] != "" {
		t.Fatalf("%s = %q, want empty (classification came from the push error)", prCreateAuthAttemptsVar, wfExec.Variables[prCreateAuthAttemptsVar])
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
// per-task starting marker is held (push_branch/create_pr reached via
// DispatchEvent/startWorkflowLocked), the recovery
// callback must NOT run inline — doing so re-enters StartWorkflow*/DispatchEvent
// against the held marker and is silently rejected. It must be queued and run
// only once drainPendingConflictRecovery fires after the marker releases.
func TestConflictRecovery_DeferredWhileMarkerHeld(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())

	var calls int
	engine.SetConflictRecovery(func(string) bool { calls++; return true })

	// Simulate running inside DispatchEvent: the starting marker is held.
	engine.mu.Lock()
	engine.starting["t1"] = struct{}{}
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
	delete(engine.starting, "t1")
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
	engine.pendingRecovery["t1"] = pendingRecovery{}
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
	reviewer := &fakePRReviewRequester{}
	engine.SetPRReviewRequester(reviewer)
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
	if reviewer.copilotCalls != 1 || reviewer.copilotRepo != "acme/widgets" || reviewer.copilotPRNumber != 42 {
		t.Fatalf("Copilot request = calls:%d repo:%q pr:%d", reviewer.copilotCalls, reviewer.copilotRepo, reviewer.copilotPRNumber)
	}
}

func TestExecCreatePR_CopilotReviewFailureDoesNotBlockPR(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRCreator(&fakePRCreator{number: 42, headSHA: headSHA(t, wtPath)})
	engine.SetPRReviewRequester(&fakePRReviewRequester{copilotErr: errors.New("copilot unavailable")})

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
}

func TestExecCreatePR_ClosesSupersededLinkedPR(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch-recovered")
	commitFile(t, wtPath, "change.txt", "feat: recovered task work")

	tasks := newMemTasks()
	task := TaskInfo{
		ID:          "t1",
		Status:      "ready-pr",
		Branch:      "feat/my-branch-recovered",
		ProjectID:   "acme/widgets",
		ProjectType: "pet",
		PRNumber:    41,
		Title:       "feat(x): y",
		Body:        "body",
	}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRCreator(&fakePRCreator{number: 42, headSHA: headSHA(t, wtPath)})
	closer := &fakePRCloser{}
	engine.SetPRCloser(closer)

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
	if closer.calls != 1 {
		t.Fatalf("ClosePR calls = %d, want 1", closer.calls)
	}
	if closer.repo != "acme/widgets" || closer.number != 41 {
		t.Fatalf("ClosePR target = %s#%d, want acme/widgets#41", closer.repo, closer.number)
	}
	if len(closer.comments) != 1 || !strings.Contains(closer.comments[0], "#42") || !strings.Contains(closer.comments[0], "t1") {
		t.Fatalf("ClosePR comment = %q, want superseded-by PR and task", closer.comments)
	}
}

func TestExecCreatePR_SupersededCloseFailureDoesNotBlockRelink(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch-recovered")
	commitFile(t, wtPath, "change.txt", "feat: recovered task work")

	tasks := newMemTasks()
	task := TaskInfo{
		ID:          "t1",
		Status:      "ready-pr",
		Branch:      "feat/my-branch-recovered",
		ProjectID:   "acme/widgets",
		ProjectType: "pet",
		PRNumber:    41,
		Title:       "feat(x): y",
		Body:        "body",
	}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRCreator(&fakePRCreator{number: 42, headSHA: headSHA(t, wtPath)})
	engine.SetPRCloser(&fakePRCloser{err: errors.New("github unavailable")})

	out, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, output = %q", out.Status, out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42 even when superseded close fails", ti.PRNumber)
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

func TestExecCreatePR_MergedSameBranchWithAppliedPatchMarksDoneWithoutCreating(t *testing.T) {
	bare, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	upstream := filepath.Join(t.TempDir(), "upstream")
	runGit(t, t.TempDir(), "clone", bare, upstream)
	runGit(t, upstream, "config", "user.email", "test@test.com")
	runGit(t, upstream, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(upstream, "change.txt"), []byte("feat: task work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstream, "unrelated.txt"), []byte("newer main work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, upstream, "add", "change.txt", "unrelated.txt")
	runGit(t, upstream, "commit", "-m", "squash equivalent plus unrelated")
	runGit(t, upstream, "push", "origin", "main")
	runGit(t, wtPath, "fetch", bare, "main:refs/remotes/origin/main")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	anyState := &fakePRAnyStateFinder{number: 77, state: "MERGED", found: true}
	engine.SetPRAnyStateFinder(anyState)
	creator := &fakePRCreator{number: 999, headSHA: headSHA(t, wtPath)}
	engine.SetPRCreator(creator)

	out, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, output = %q", out.Status, out.Output)
	}
	if anyState.calls != 1 {
		t.Fatalf("any-state finder calls = %d, want 1", anyState.calls)
	}
	if creator.gotReq.Repo != "" {
		t.Fatal("CreatePR must not be called when the branch patch already landed via a merged PR")
	}
	if out.TerminalStatus != "done" {
		t.Fatalf("TerminalStatus = %q, want done", out.TerminalStatus)
	}
	if !strings.Contains(out.TerminalReason, "merged PR #77") {
		t.Fatalf("TerminalReason = %q, want merged PR reference", out.TerminalReason)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "ready-pr" {
		t.Fatalf("status = %q, want unchanged ready-pr before AdvanceStep handles terminal output", ti.Status)
	}
}

func TestExecCreatePR_MergedSameBranchWithRemainingPatchStillCreates(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRAnyStateFinder(&fakePRAnyStateFinder{number: 77, state: "MERGED", found: true})
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
		t.Fatal("CreatePR must be called when the branch still contributes a patch")
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "done" {
		t.Fatal("task must not be marked done while the branch still has unapplied work")
	}
	if ti.PRNumber != 42 {
		t.Fatalf("PRNumber = %d, want 42", ti.PRNumber)
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

func TestClassifyPRGitError_GitHTTPSUsernamePromptIsAuthFailure(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	step := newCreatePRStep()
	task := TaskInfo{ID: "t1", Status: "ready-pr"}
	wfExec := &Execution{Variables: map[string]string{}}
	authErr := errors.New("github push credential preflight failed: git push --dry-run origin HEAD:refs/heads/sybra-preflight/abc: ambient env: fatal: could not read Username for 'https://github.com': No such device or address")

	_, err := engine.classifyPRGitError("t1", step, wfExec, task, authErr, "push credential preflight")
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	ti, _ := tasks.GetTask("t1")
	if !strings.HasPrefix(ti.StatusReason, prCreateAuthRetryReason+": ") {
		t.Fatalf("status reason = %q, want auth retry reason with diagnostic detail", ti.StatusReason)
	}
	if !strings.Contains(ti.StatusReason, "could not read Username") {
		t.Fatalf("status reason = %q, want git auth diagnostic", ti.StatusReason)
	}
	if wfExec.Variables[prCreateAuthAttemptsVar] != "1" {
		t.Fatalf("%s = %q, want 1", prCreateAuthAttemptsVar, wfExec.Variables[prCreateAuthAttemptsVar])
	}
	if wfExec.Variables[prPushAttemptsVar] != "" {
		t.Fatalf("%s = %q, want empty because auth retry counter should be used", prPushAttemptsVar, wfExec.Variables[prPushAttemptsVar])
	}
}

func TestPRRetryReasonPreservesTailForLongHookOutput(t *testing.T) {
	detail := strings.Repeat("ok github.com/Automaat/sybra/internal/pkg\n", 20) + "FAIL github.com/Automaat/sybra/internal/workflow\n"
	got := prRetryReason(prPushRetryStatusReason, detail)

	if !strings.HasPrefix(got, prPushRetryStatusReason+": ") {
		t.Fatalf("reason = %q, want base prefix", got)
	}
	if !strings.Contains(got, "... (truncated) ...") {
		t.Fatalf("reason = %q, want truncation marker", got)
	}
	if !strings.Contains(got, "FAIL github.com/Automaat/sybra/internal/workflow") {
		t.Fatalf("reason = %q, want failing tail preserved", got)
	}
}

func TestTruncateMiddleHonorsSmallLimit(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 5, 20} {
		got := truncateMiddle("abcdefghijklmnopqrstuvwxyz", limit)
		if len(got) > max(0, limit) {
			t.Fatalf("limit %d: len(%q) = %d, want <= %d", limit, got, len(got), max(0, limit))
		}
	}
}

// TestClassifyPRGitError_UnclassifiedFailureParksOnceThenEscalates covers a
// push rejected by a project's own pre-push hook (e.g. `go test ./...`
// failing under concurrent-agent CPU contention) — output that matches none
// of the GitHub-shaped patterns. A single retry absorbs a flake; a genuine
// regression fails again and still escalates.
func TestClassifyPRGitError_UnclassifiedFailureParksOnceThenEscalates(t *testing.T) {
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "ready-pr"})
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	step := newPushBranchStep()
	task := TaskInfo{ID: "t1", Status: "ready-pr"}

	wfExec := &Execution{Variables: map[string]string{}}
	hookErr := errors.New("git push: exit status 1: FAIL internal/sybra TestAgentAdapterStartAgentDoesNotClobberForeignClaimAfterRecovery")
	for i := range maxPRPushRetries {
		_, err := engine.classifyPRGitError("t1", step, wfExec, task, hookErr, "git push")
		if !errors.Is(err, errStepParked) {
			t.Fatalf("attempt %d: err = %v, want errStepParked", i, err)
		}
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status == "human-required" {
		t.Fatalf("status = %q after %d retries, want unchanged (still parked)", ti.Status, maxPRPushRetries)
	}
	if !strings.HasPrefix(ti.StatusReason, prPushRetryStatusReason+": ") || !strings.Contains(ti.StatusReason, "FAIL internal/sybra") {
		t.Fatalf("status reason = %q, want push retry reason with hook detail", ti.StatusReason)
	}
	if _, err := engine.classifyPRGitError("t1", step, wfExec, task, hookErr, "git push"); err != nil {
		t.Fatalf("final attempt: unexpected error %v", err)
	}
	ti, _ = tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required after exhausting push retries", ti.Status)
	}
}

type raceThenFoundFinder struct{ n int }

func (f *raceThenFoundFinder) FindPRForBranch(context.Context, string, string) (number int, found bool, err error) {
	f.n++
	if f.n == 1 {
		return 0, false, nil
	}
	return 55, true, nil
}

func TestExecCreatePR_AdoptsExistingPROnAlreadyExistsConflict(t *testing.T) {
	_, wtPath := newPRWorktree(t, "feat/my-branch")
	commitFile(t, wtPath, "change.txt", "feat: task work")

	tasks := newMemTasks()
	task := TaskInfo{ID: "t1", Status: "ready-pr", Branch: "feat/my-branch", ProjectID: "acme/widgets", ProjectType: "pet", Title: "feat(x): y", Body: "body"}
	tasks.Put(task)
	engine := NewEngine(newTestStore(t), tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetPRFinder(&raceThenFoundFinder{})
	engine.SetPRCreator(&fakePRCreator{err: errors.New(`a pull request for branch "acme:feat/my-branch" into branch "main" already exists: https://github.com/acme/widgets/pull/55`)})
	engine.SetPRContentGenerator(&fakePRContentGenerator{title: "feat(x): y", body: "## Motivation\n\nz\n\n## Implementation information\n\nw"})

	out, err := engine.execCreatePR("t1", newCreatePRStep(), &Execution{Variables: map[string]string{}}, task)
	if err != nil {
		t.Fatalf("execCreatePR: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status = %q, output = %q", out.Status, out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.PRNumber != 55 {
		t.Errorf("PRNumber = %d, want 55 (adopted existing PR)", ti.PRNumber)
	}
	if ti.Status == "human-required" {
		t.Errorf("must not escalate to human-required when the PR already exists: %s", tasks.Reason("t1"))
	}
}
