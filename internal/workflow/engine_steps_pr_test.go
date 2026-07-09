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
