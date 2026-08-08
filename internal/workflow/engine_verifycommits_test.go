package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/taskstatus"
)

func TestResolveOriginBase_FallsBackFromDanglingOriginHEAD(t *testing.T) {
	wtPath := makeGitRepo(t, false)
	refPath := filepath.Join(wtPath, ".git", "refs", "remotes", "origin", "HEAD")
	if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refPath, []byte(strings.Repeat("f", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveOriginBase(context.Background(), wtPath); got != "origin/main" {
		t.Fatalf("resolveOriginBase() = %q, want origin/main when origin/HEAD is dangling", got)
	}
}

func TestResolveOriginBase_UsesLinkedRepositoryDefaultBranch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, seed, "init", "-b", "trunk")
	runGitAt(t, seed, "config", "user.email", "test@test.com")
	runGitAt(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init\\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, seed, "add", "README.md")
	runGitAt(t, seed, "commit", "-m", "init")
	runGitAt(t, seed, "remote", "add", "origin", remote)
	runGitAt(t, seed, "push", "origin", "trunk")

	// Sybra worktrees are linked to a bare clone. Its HEAD, unlike a normal
	// checkout's HEAD, identifies the default branch for all linked worktrees.
	runGitAt(t, "", "-C", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	runGitAt(t, "", "-C", remote, "update-ref", "refs/remotes/origin/trunk", "refs/heads/trunk")
	wtPath := filepath.Join(t.TempDir(), "worktree")
	runGitAt(t, "", "-C", remote, "worktree", "add", wtPath, "trunk")

	refPath := filepath.Join(remote, "refs", "remotes", "origin", "HEAD")
	if err := os.WriteFile(refPath, []byte(strings.Repeat("f", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveOriginBase(context.Background(), wtPath); got != "origin/trunk" {
		t.Fatalf("resolveOriginBase() = %q, want origin/trunk from linked repository HEAD", got)
	}
}

// TestRecoverVerifyCommitsRefs_FailedFetchPreservesDefaultBase verifies that
// recovery treats remote-tracking refs as a cache: a failed refresh must leave
// the real default branch available for the next commit-range comparison.
func TestRecoverVerifyCommitsRefs_FailedFetchPreservesDefaultBase(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, seed, "init", "-b", "trunk")
	runGitAt(t, seed, "config", "user.email", "test@test.com")
	runGitAt(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, seed, "add", "README.md")
	runGitAt(t, seed, "commit", "-m", "init")
	runGitAt(t, seed, "remote", "add", "origin", remote)
	runGitAt(t, seed, "push", "origin", "trunk")
	runGitAt(t, "", "-C", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	runGitAt(t, "", "-C", remote, "update-ref", "refs/remotes/origin/trunk", "refs/heads/trunk")

	wtPath := filepath.Join(t.TempDir(), "worktree")
	runGitAt(t, "", "-C", remote, "worktree", "add", wtPath, "trunk")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "fetch" ]; then
  echo "fatal: simulated unreachable origin" >&2
  exit 128
fi
exec "{{REAL_GIT}}" "$@"
`)

	engine := NewTestEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	if recovered := engine.recoverVerifyCommitsRefs("t1", wtPath, TaskInfo{Branch: "fix/not-pushed"}); recovered {
		t.Fatal("recoverVerifyCommitsRefs() = true, want false after failed fetch")
	}
	if got := resolveOriginBase(context.Background(), wtPath); got != "origin/trunk" {
		t.Fatalf("resolveOriginBase() = %q, want origin/trunk after failed recovery", got)
	}
	if !gitOK(context.Background(), wtPath, "rev-parse", "--verify", "origin/trunk^{commit}") {
		t.Fatal("origin/trunk was removed by failed ref recovery")
	}
}

// TestExecVerifyCommits_ParksWhileSiblingRunning asserts the step re-arms the
// implement run_agent step and parks the workflow in ExecWaiting (returning
// errStepParked) when another agent is still working the task — instead of
// flipping a task with live work to human-required OR completing the workflow
// (whose status-change cascade would re-dispatch over the sibling).
func TestExecVerifyCommits_ParksWhileSiblingRunning(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID: "t1", Status: "in-progress",
		Workflow: &Execution{WorkflowID: "x", CurrentStep: "verify_commits", State: ExecRunning},
	})
	agents := newMockAgents()
	agents.mu.Lock()
	agents.running["t1"] = "sibling" // a different agent than the completer
	agents.mu.Unlock()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false /* no commits */), ok: true})

	wfExec := &Execution{WorkflowID: "x", CurrentStep: "verify_commits", State: ExecRunning}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "completer"})

	_, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, TaskInfo{ID: "t1"})
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.State != ExecWaiting {
		t.Errorf("State = %q, want ExecWaiting", wfExec.State)
	}
	if wfExec.CurrentStep != "implement" {
		t.Errorf("CurrentStep = %q, want implement (re-armed run_agent step)", wfExec.CurrentStep)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Errorf("status = %q, want unchanged in-progress (no human-required flip)", ti.Status)
	}
}

// TestExecVerifyCommits_ExcludesCompletingAgent guards against the self-deadlock
// the deferral could otherwise cause: the agent whose completion triggered the
// step still reads as running (its done channel closes after onComplete), so it
// must be excluded. With no genuine sibling, the normal no-commits verdict fires.
func TestExecVerifyCommits_ExcludesCompletingAgent(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	agents.mu.Lock()
	agents.running["t1"] = "completer" // the same agent whose completion drives this step
	agents.mu.Unlock()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: makeGitRepo(t, false /* no commits */), ok: true})

	wfExec := &Execution{}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "failed", AgentID: "completer"})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, TaskInfo{ID: "t1"})
	if errors.Is(err, errStepParked) {
		t.Fatal("must not park when only the completing agent appears running")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "human-required") {
		t.Errorf("Output = %q, want the human-required verdict", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Errorf("status = %q, want human-required (genuine crash, no sibling)", ti.Status)
	}
}

func TestExecVerifyCommits_NoGetterSkips(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	// worktrees nil by default

	ti := TaskInfo{ID: "t1"}
	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, ti)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "skipped") {
		t.Errorf("Output = %q, want skipped", out.Output)
	}
}

// TestExecVerifyCommits_SkipsAndEscalations tables the execVerifyCommits
// outcomes that only depend on worktree/git state and step history — no
// worktree, a clean ahead-of-base branch, a broken (non-git) worktree, a
// branch sitting at or behind base, and a failed upstream agent run. Cases
// needing their own bespoke fixtures (retry-timer state, a real remote
// clone) stay as standalone tests below.
func TestExecVerifyCommits_SkipsAndEscalations(t *testing.T) {
	cases := []struct {
		name              string
		getterOK          bool
		pathFn            func(t *testing.T) string
		wfExec            *Execution
		wantOutputSubstr  string
		wantTaskStatus    taskstatus.Status
		wantReasonSubstrs []string
	}{
		{
			name:             "NoWorktreeSkips",
			getterOK:         false,
			wantOutputSubstr: "skipped",
			wantTaskStatus:   "in-progress",
		},
		{
			name:     "WithCommitsVerified",
			getterOK: true,
			pathFn: func(t *testing.T) string {
				t.Helper()
				return makeGitRepo(t, true /* withExtraCommit */)
			},
			wantOutputSubstr: "commits verified",
			wantTaskStatus:   "in-progress",
		},
		{
			// Path exists but is not a git repo — `git log` and `git status`
			// both fail with the same fatal, simulating the broken-worktree
			// scenario from the synapse→sybra rename; the reason surfaces
			// the `git status` diagnosis.
			name:     "GitErrorFlipsHumanRequired",
			getterOK: true,
			pathFn: func(t *testing.T) string {
				t.Helper()
				return t.TempDir()
			},
			wantOutputSubstr:  "git error",
			wantTaskStatus:    "human-required",
			wantReasonSubstrs: []string{"worktree git error", "git status"},
		},
		{
			name:     "BranchAtBaseFlipsHumanRequired",
			getterOK: true,
			pathFn: func(t *testing.T) string {
				t.Helper()
				return makeGitRepo(t, false /* HEAD == origin/main */)
			},
			wantOutputSubstr:  "no commits",
			wantTaskStatus:    "human-required",
			wantReasonSubstrs: []string{"no commits"},
		},
		{
			name:             "BranchAncestorOfBaseFlipsHumanRequired",
			getterOK:         true,
			pathFn:           makeGitRepoBehindOrigin,
			wantOutputSubstr: "no commits",
			wantTaskStatus:   "human-required",
		},
		{
			// Same git state as BranchAtBaseFlipsHumanRequired (HEAD ==
			// origin/main), but with a failed implement step in history:
			// the false-positive auto-close guard must still flip to
			// human-required (agent crashed before committing), not
			// silently mark done.
			name:     "AgentFailedFlipsHumanRequired",
			getterOK: true,
			pathFn: func(t *testing.T) string {
				t.Helper()
				return makeGitRepo(t, false /* HEAD == origin/main */)
			},
			wfExec: &Execution{StepHistory: []StepRecord{
				{StepID: "implement", Status: "failed", AgentID: "a1", Provider: "claude"},
			}},
			wantOutputSubstr:  "agent failed before commit",
			wantTaskStatus:    "human-required",
			wantReasonSubstrs: []string{"agent failed before committing"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			tasks := newMemTasks()
			tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
			agents := newMockAgents()
			engine := NewTestEngine(store, tasks, agents, discardLogger())
			var path string
			if tc.pathFn != nil {
				path = tc.pathFn(t)
			}
			engine.SetWorktreeGetter(&fakeWorktreeGetter{path: path, ok: tc.getterOK})

			wfExec := tc.wfExec
			if wfExec == nil {
				wfExec = &Execution{}
			}
			out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, TaskInfo{ID: "t1"})
			if err != nil {
				t.Fatal(err)
			}
			if out.Status != "completed" {
				t.Errorf("Status = %q, want completed", out.Status)
			}
			if !strings.Contains(out.Output, tc.wantOutputSubstr) {
				t.Errorf("Output = %q, want %q", out.Output, tc.wantOutputSubstr)
			}
			ti, _ := tasks.GetTask("t1")
			if ti.Status != tc.wantTaskStatus {
				t.Errorf("task status = %q, want %q", ti.Status, tc.wantTaskStatus)
			}
			reason := tasks.Reason("t1")
			for _, sub := range tc.wantReasonSubstrs {
				if !strings.Contains(reason, sub) {
					t.Errorf("status reason = %q, want %q", reason, sub)
				}
			}
		})
	}
}

func TestExecVerifyCommits_RetriesAfterTransientFailure(t *testing.T) {
	prev := verifyCommitsRetrySleep
	t.Cleanup(func() { verifyCommitsRetrySleep = prev })

	wtDir := makeGitRepo(t, true /* withExtraCommit */)

	// Simulate transient lock: place .git/index.lock that will be removed
	// during the retry sleep, so the second `git log` succeeds. The lock
	// path differs between linked and primary worktrees; here makeGitRepo
	// returns a primary repo, so .git is a directory.
	lockPath := filepath.Join(wtDir, ".git", "index.lock")
	if err := os.WriteFile(lockPath, []byte("locked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	verifyCommitsRetrySleep = func(_ time.Duration) {
		_ = os.Remove(lockPath)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	// First call may or may not actually be blocked by the lock depending
	// on git behavior — but the retry path always runs once per error,
	// and we just need the final outcome to be "commits verified".
	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress", ti.Status)
	}
}

func TestExecVerifyCommits_RetriesTransientBadHEAD(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})
	verifyCommitsRetrySleep = func(time.Duration) {}
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond, time.Nanosecond}

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	countFile := filepath.Join(t.TempDir(), "git-log-count")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "log" ]; then
  count=0
  if [ -f "`+countFile+`" ]; then
    count=$(cat "`+countFile+`")
  fi
  count=$((count + 1))
  printf "%s" "$count" > "`+countFile+`"
  if [ "$count" = "1" ]; then
    echo "fatal: bad object HEAD" >&2
    exit 128
  fi
fi
exec "{{REAL_GIT}}" "$@"
`)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress", ti.Status)
	}
	if got := strings.TrimSpace(readFile(t, countFile)); got != "2" {
		t.Errorf("git log calls = %q, want 2", got)
	}
}

func TestExecVerifyCommits_DurableBadHEADEscalatesAfterRetries(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})
	verifyCommitsRetrySleep = func(time.Duration) {}
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond, time.Nanosecond}

	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	countFile := filepath.Join(t.TempDir(), "git-log-count")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "log" ]; then
  count=0
  if [ -f "`+countFile+`" ]; then
    count=$(cat "`+countFile+`")
  fi
  count=$((count + 1))
  printf "%s" "$count" > "`+countFile+`"
  echo "fatal: bad object HEAD" >&2
  exit 128
fi
exec "{{REAL_GIT}}" "$@"
`)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "git error") {
		t.Errorf("Output = %q, want git error", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Errorf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "bad object HEAD") {
		t.Errorf("status reason = %q, want bad object HEAD", reason)
	}
	if got := strings.TrimSpace(readFile(t, countFile)); got != "3" {
		t.Errorf("git log calls = %q, want 3", got)
	}
}

func TestExecVerifyCommits_FetchesMissingLocalHeadObject(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})
	verifyCommitsRetrySleep = func(time.Duration) {}
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond}

	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)

	wtDir := t.TempDir()
	runGitAt(t, wtDir, "init", "-b", "main")
	runGitAt(t, wtDir, "config", "user.email", "test@test.com")
	runGitAt(t, wtDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "README.md")
	runGitAt(t, wtDir, "commit", "-m", "init")
	runGitAt(t, wtDir, "remote", "add", "origin", remote)
	runGitAt(t, wtDir, "push", "-u", "origin", "main")
	runGitAt(t, wtDir, "checkout", "-b", "fix/missing-object")
	if err := os.WriteFile(filepath.Join(wtDir, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "change.txt")
	runGitAt(t, wtDir, "commit", "-m", "feat: task work")
	head := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "HEAD"))
	runGitAt(t, wtDir, "push", "-u", "origin", "HEAD:fix/missing-object")
	runGitAt(t, wtDir, "fetch", "origin",
		"+refs/heads/fix/missing-object:refs/remotes/origin/fix/missing-object",
		"+refs/heads/main:refs/remotes/origin/main")
	if got := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "refs/remotes/origin/fix/missing-object")); got != head {
		t.Fatalf("origin/fix/missing-object = %q, want %q", got, head)
	}

	badSiblingRef := filepath.Join(wtDir, ".git", "refs", "heads", "feat", "bad-sibling")
	if err := os.MkdirAll(filepath.Dir(badSiblingRef), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badSiblingRef, []byte(strings.Repeat("f", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	objectPath := filepath.Join(wtDir, ".git", "objects", head[:2], head[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove local head object %s: %v", objectPath, err)
	}
	if out, err := gitCombinedAt(wtDir, "status", "--short", "--branch"); err == nil || !strings.Contains(out, "bad object HEAD") {
		t.Fatalf("git status after object removal err=%v out=%q, want bad object HEAD", err, out)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Branch: "fix/missing-object"})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: "fix/missing-object"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Fatalf("Output = %q, reason = %q, want commits verified after fetch recovery", out.Output, tasks.Reason("t1"))
	}
	if got := strings.TrimSpace(runGitAt(t, wtDir, "cat-file", "-t", head)); got != "commit" {
		t.Fatalf("recovered object type = %q, want commit", got)
	}
	if _, err := os.Stat(badSiblingRef); !os.IsNotExist(err) {
		t.Fatalf("broken sibling ref still exists after recovery: stat err=%v", err)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Fatalf("task status = %q, want unchanged in-progress", ti.Status)
	}
}

// TestExecVerifyCommits_BranchAtBaseFlipsHumanRequired covers the case where
// the agent reported success but committed nothing: HEAD == origin/main. This
// used to mark the task done on the theory that the fix might already be on
// origin/main via a different branch, but that check (branchMergedIntoBase)
// can never actually distinguish "already merged elsewhere" from "nothing was
// committed" at this call site — an empty baseRef..HEAD log range and "HEAD
// is an ancestor of baseRef" are the same git fact, so it was true on every
// call. Confirmed live: two foundational tasks landed `done` with prNumber 0
// and a branch byte-identical to origin/main — zero code shipped. A human
// must see this instead.
func TestExecVerifyCommits_NoCommitAuthorRunRetriesOnce(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation"}},
	}

	_, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	if wfExec.CurrentStep != "implement" || wfExec.State != ExecWaiting {
		t.Fatalf("workflow = %+v, want rearmed implement/ExecWaiting", wfExec)
	}
	if got := wfExec.Variables["step.verify_commits.no_commit_retry"]; got != "1" {
		t.Fatalf("no_commit_retry = %q, want 1", got)
	}
	if got := wfExec.Variables[verifyReaskNoteVar]; !strings.Contains(got, "without producing commits") {
		t.Fatalf("verify reask note = %q, want no-commit guidance", got)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress retry", ti.Status)
	} else if ti.StatusReason != "retrying implementation once after no commits were produced" {
		t.Fatalf("status reason = %q, want no-commit retry reason", ti.StatusReason)
	}
}

func TestExecVerifyCommits_NoCommitAuthorRunRetryPersistFailureDoesNotPartiallyRearm(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:           "t1",
		Status:       "in-progress",
		StatusReason: "original reason",
		Workflow: &Execution{
			WorkflowID:  "test-simple",
			CurrentStep: "verify",
			State:       ExecRunning,
			Variables:   map[string]string{},
		},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation"}},
	}

	tasks.failSetWorkflow = true
	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Status != "completed" || !strings.Contains(out.Output, "no commits") {
		t.Fatalf("output = %+v, want completed no-commit escalation after failed retry persist", out)
	}

	got := tasks.mustGetTask(t, "t1")
	if got.Status != "human-required" {
		t.Fatalf("status = %q after failed retry persist, want fallback human-required escalation", got.Status)
	}
	if !strings.Contains(got.StatusReason, "no commits") {
		t.Fatalf("status reason = %q after failed retry persist, want no-commits escalation", got.StatusReason)
	}
	if got.Workflow == nil || got.Workflow.CurrentStep != "verify" || got.Workflow.State != ExecRunning {
		t.Fatalf("workflow = %+v after failed retry persist, want retry workflow not partially persisted", got.Workflow)
	}
}

func TestExecVerifyCommits_NoCommitAuthorRunRetriesOnceWithSubagentDiagnosis(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation", SubagentCallCount: 2}},
	}

	_, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}
	got := wfExec.Variables[verifyReaskNoteVar]
	if !strings.Contains(got, "background subagent") {
		t.Fatalf("verify reask note = %q, want background-subagent diagnosis", got)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "background subagent handoff") {
		t.Fatalf("status reason = %q, want background subagent diagnosis", reason)
	}
}

func TestExecVerifyCommits_NoCommitAuthorRunEscalatesAfterRetry(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit; HEAD == origin/main */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{"step.verify_commits.no_commit_retry": "1"}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation"}},
	}

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Fatalf("Output = %q, want no commits", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Fatalf("status = %q, want human-required after one retry", ti.Status)
	}
}

// TestExecVerifyCommits_BranchAncestorOfBaseFlipsHumanRequired covers the
// regression from issue #670: HEAD is an ancestor of origin/main (branch tip
// equals an older commit on main, with newer commits on top — typical of
// squash-merge followed by additional PRs). `git log origin/main..HEAD` is
// empty AND HEAD != base.tip. #670 originally fixed this by marking the task
// done outright (the theory: the work must already be on origin). That
// theory cannot be verified from ancestry alone — "HEAD is an ancestor of
// base because its own commits already landed" and "HEAD is an ancestor of
// base because it never had any commits to begin with" are the same git
// fact, indistinguishable by `merge-base --is-ancestor`. Silently marking
// done was proven live to misfire on the second case (issues #2658, #2659
// landed `done` with zero code). A human must confirm either way now.
// TestExecVerifyCommits_AutoCommitsUncommittedWork verifies that a worktree
// with no commits ahead of base but dirty (uncommitted) files is recovered by
// auto-committing rather than escalated to human-required — the scenario
// where an implementation agent finished its work but was interrupted (or
// simply forgot) before running `git commit`.
func TestExecVerifyCommits_AutoCommitsUncommittedWork(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	// HEAD == origin/main (no commits ahead), but leave uncommitted work
	// sitting dirty in the worktree.
	wtDir := makeGitRepo(t, false /* no extra commit */)
	if err := os.WriteFile(filepath.Join(wtDir, "uncommitted.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed" {
		t.Errorf("Status = %q, want completed", out.Status)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Errorf("Output = %q, want 'commits verified'", out.Output)
	}
	// Task must not be escalated — the dirty work was recovered.
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "in-progress" {
		t.Errorf("task status = %q, want in-progress (not escalated)", ti.Status)
	}
	// The file must now be committed on the branch.
	cmd := exec.Command("git", "log", "origin/main..HEAD", "--name-only")
	cmd.Dir = wtDir
	log, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(log), "uncommitted.txt") {
		t.Errorf("git log = %q, want it to contain the auto-committed file", log)
	}
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = wtDir
	statusOut, err := statusCmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("worktree still dirty after auto-commit: %q", statusOut)
	}
}

func TestExecVerifyCommits_AutoCommitRemoteReconcileGitErrorEscalates(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "read-tree-count")
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "read-tree" ] && [ "$2" = "HEAD" ]; then
  count=0
  if [ -f "`+countFile+`" ]; then
    count=$(cat "`+countFile+`")
  fi
  count=$((count + 1))
  printf "%s" "$count" > "`+countFile+`"
  if [ "$count" = "2" ]; then
    echo "fatal: synthetic read-tree failure" >&2
    exit 128
  fi
fi
exec "{{REAL_GIT}}" "$@"
`)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	wtDir := makeGitRepo(t, false /* no extra commit */)
	if err := os.WriteFile(filepath.Join(wtDir, "uncommitted.txt"), []byte("finished work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "git error") {
		t.Fatalf("Output = %q, want git error after remote reconcile failure", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "human-required" {
		t.Fatalf("task status = %q, want human-required", ti.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "after auto-commit remote reconcile") {
		t.Fatalf("status reason = %q, want auto-commit remote reconcile context", reason)
	}
	if got := strings.TrimSpace(readFile(t, countFile)); got != "2" {
		t.Fatalf("read-tree calls = %q, want 2 (pre- and post-auto-commit probes)", got)
	}
}

func TestExecVerifyCommits_AutoCommitAdoptsEquivalentRemoteCommitAfterRetry(t *testing.T) {
	prevSleep := verifyCommitsRetrySleep
	prevBackoffs := verifyCommitsRetryBackoffs
	t.Cleanup(func() {
		verifyCommitsRetrySleep = prevSleep
		verifyCommitsRetryBackoffs = prevBackoffs
	})

	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)

	wtDir := t.TempDir()
	runGitAt(t, wtDir, "init", "-b", "main")
	runGitAt(t, wtDir, "config", "user.email", "test@test.com")
	runGitAt(t, wtDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "README.md")
	runGitAt(t, wtDir, "commit", "-m", "init")
	runGitAt(t, wtDir, "remote", "add", "origin", remote)
	runGitAt(t, wtDir, "push", "-u", "origin", "main")

	const branch = "feat/verify-commits-race"
	runGitAt(t, wtDir, "checkout", "-b", branch)
	runGitAt(t, wtDir, "push", "-u", "origin", branch)

	const (
		fileName = "verify-commits-race.txt"
		content  = "verify_commits race\n"
	)
	if err := os.WriteFile(filepath.Join(wtDir, fileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	stageDir := t.TempDir()
	var pushed sync.Once
	verifyCommitsRetryBackoffs = []time.Duration{time.Nanosecond}
	verifyCommitsRetrySleep = func(time.Duration) {
		pushed.Do(func() {
			repoDir := filepath.Join(stageDir, "repo")
			runGitAt(t, "", "clone", remote, repoDir)
			runGitAt(t, repoDir, "checkout", "-B", branch, "origin/"+branch)
			if err := os.WriteFile(filepath.Join(repoDir, fileName), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitAt(t, repoDir, "add", fileName)
			runGitAt(t, repoDir, "-c", "user.email=fake-claude@test.local", "-c", "user.name=Fake Claude", "commit", "-m", "feat: remote race")
			runGitAt(t, repoDir, "push", "origin", "HEAD:refs/heads/"+branch)
		})
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "commits verified") {
		t.Fatalf("Output = %q, want commits verified", out.Output)
	}
	if ti, _ := tasks.GetTask("t1"); ti.Status != "in-progress" {
		t.Fatalf("task status = %q, want in-progress", ti.Status)
	}

	runGitAt(t, wtDir, "fetch", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	localHead := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "HEAD"))
	remoteHead := strings.TrimSpace(runGitAt(t, wtDir, "rev-parse", "refs/remotes/origin/"+branch))
	if localHead != remoteHead {
		t.Fatalf("local HEAD %q != remote HEAD %q; want verify_commits to adopt the equivalent remote commit", localHead, remoteHead)
	}
	if got := strings.TrimSpace(runGitAt(t, wtDir, "rev-list", "--count", "origin/main..HEAD")); got != "1" {
		t.Fatalf("origin/main..HEAD commit count = %q, want 1 implementation lineage", got)
	}
	if status := strings.TrimSpace(runGitAt(t, wtDir, "status", "--porcelain")); status != "" {
		t.Fatalf("worktree dirty after reconcile: %q", status)
	}
}

// TestExecVerifyCommits_EmptyRemoteBranchFlipsHumanRequired covers the
// equivalent-tree remote-adopt bug: a task branch is pushed to origin but is
// byte-identical to base (zero commits ahead), e.g. because the
// implementation agent handed off to a background subagent and exited
// without producing any work. verify_commits must not treat the pushed,
// empty branch as completed work just because its tree matches the local
// (also-empty) worktree tree.
func TestExecVerifyCommits_EmptyRemoteBranchFlipsHumanRequired(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "origin.git")
	runGitAt(t, "", "init", "--bare", remote)

	wtDir := t.TempDir()
	runGitAt(t, wtDir, "init", "-b", "main")
	runGitAt(t, wtDir, "config", "user.email", "test@test.com")
	runGitAt(t, wtDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wtDir, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtDir, "add", "README.md")
	runGitAt(t, wtDir, "commit", "-m", "init")
	runGitAt(t, wtDir, "remote", "add", "origin", remote)
	runGitAt(t, wtDir, "push", "-u", "origin", "main")

	// Task branch pushed to origin with no extra commits — byte-identical to
	// base on both ends.
	const branch = "feat/verify-commits-empty-remote"
	runGitAt(t, wtDir, "checkout", "-b", branch)
	runGitAt(t, wtDir, "push", "-u", "origin", branch)

	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	out, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), &Execution{}, TaskInfo{ID: "t1", Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Output, "no commits") {
		t.Fatalf("Output = %q, want 'no commits' (empty pushed branch must not be adopted)", out.Output)
	}
	ti, _ := tasks.GetTask("t1")
	if ti.Status != "human-required" {
		t.Fatalf("task status = %q, want human-required", ti.Status)
	}
}

func TestRecordFinalCommitState_ContextCancelDoesNotHang(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1"}},
	})
	engine := NewTestEngine(store, tasks, newMockAgents(), discardLogger())

	marker := filepath.Join(t.TempDir(), "rev-parse-started")
	t.Setenv("WORKFLOW_TEST_MARKER", marker)
	wtDir := makeGitRepo(t, true /* withExtraCommit */)
	withFakeGit(t, `#!/bin/sh
if [ "$1" = "rev-parse" ] && [ "$2" = "--verify" ] && [ "$3" = "HEAD^{commit}" ]; then
  touch "$WORKFLOW_TEST_MARKER"
  sleep 30
fi
exec "{{REAL_GIT}}" "$@"
`)

	parentCtx, cancel := context.WithCancel(context.Background())
	engine.SetContext(parentCtx)
	go func() {
		if pollUntil(5*time.Second, 10*time.Millisecond, func() bool {
			_, err := os.Stat(marker)
			return err == nil
		}) {
			cancel()
		}
	}()

	wfExec := &Execution{StepHistory: []StepRecord{{StepID: "implement", Status: "completed", AgentID: "a1"}}}

	start := time.Now()
	engine.recordFinalCommitState("t1", wfExec, wtDir, finalCommitSourceAgent)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("recordFinalCommitState took %v after ctx cancel; want prompt return", elapsed)
	}
	ti, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if ti.AgentRuns[0].HeadSHA != "" {
		t.Fatalf("head_sha = %q, want empty when rev-parse is canceled", ti.AgentRuns[0].HeadSHA)
	}
}

// A code-author run that exits cleanly but produces no commits is not a
// success, and the completion handler cannot know that — it derives the
// outcome from the exit alone. Leaving the record at "success" is what let one
// task accumulate 18 runs over 4 days: an agent delegated to a subagent, ended
// its turn with "I'll just wait for the notification instead", exited 0, and
// every downstream consumer of Outcome believed the implementation had landed.
func TestExecVerifyCommits_NoCommitAuthorRunMarkedIncomplete(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false /* HEAD == origin/main, so no commits */)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "implement", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "implementation"}},
	}

	if _, err := engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti); !errors.Is(err, errStepParked) {
		t.Fatalf("err = %v, want errStepParked", err)
	}

	if got := tasks.IncompleteRuns(); len(got) != 1 || got[0] != "t1/a1" {
		t.Errorf("incomplete runs = %v, want [t1/a1] — the success record was left uncorrected", got)
	}
}

// A verifier role that produces no commits is doing its job; only code-author
// roles are downgraded.
func TestExecVerifyCommits_VerifierRunNotMarkedIncomplete(t *testing.T) {
	store := newTestStore(t)
	tasks := newMemTasks()
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	agents := newMockAgents()
	engine := NewTestEngine(store, tasks, agents, discardLogger())

	wtDir := makeGitRepo(t, false)
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtDir, ok: true})

	wfExec := &Execution{Variables: map[string]string{}}
	wfExec.RecordStep(StepRecord{StepID: "review", Status: "completed", AgentID: "a1", Provider: "claude"})
	ti := TaskInfo{
		ID: "t1", Status: "in-progress",
		AgentRuns: []AgentRunInfo{{AgentID: "a1", Role: "review"}},
	}

	_, _ = engine.execVerifyCommits("t1", newVerifyCommitsStep(), wfExec, ti)

	if got := tasks.IncompleteRuns(); len(got) != 0 {
		t.Errorf("incomplete runs = %v, want none for a verifier role", got)
	}
}
