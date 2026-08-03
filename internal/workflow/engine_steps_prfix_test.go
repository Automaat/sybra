package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
)

// scriptedPRStateFetcher returns a fixed state or error for every probe,
// regardless of repo/number, so tests can pin the "remote already resolved"
// signal without shelling out to `gh`.
type scriptedPRStateFetcher struct {
	state github.PRState
	err   error
}

func (f scriptedPRStateFetcher) FetchPRState(string, int) (github.PRState, error) {
	return f.state, f.err
}

func TestClassifyPRFixResult(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		output          string
		wantVerdict     PRFixVerdict
		wantReason      string
		wantEmptyReason bool
	}{
		{
			name: "sentinel human required with reason",
			output: "Aborted rebase.\nSYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: 5 conflicting files exceed the auto-resolve limit\n",
			wantVerdict: PRFixHuman,
			wantReason:  "5 conflicting files exceed the auto-resolve limit",
		},
		{
			name:        "sentinel continue",
			output:      "Pushed fixes.\nSYBRA_PR_FIX_RESULT: continue\n",
			wantVerdict: PRFixContinue,
		},
		{
			name: "sentinel flake with reason",
			output: "The failing job also fails on main.\nSYBRA_PR_FIX_RESULT: flake\n" +
				"SYBRA_PR_FIX_REASON: e2e provisioning timeout, reproduces on base\n",
			wantVerdict: PRFixFlake,
			wantReason:  "e2e provisioning timeout, reproduces on base",
		},
		{
			name:        "sentinel no-op alias maps to flake",
			output:      "Nothing to change.\nSYBRA_PR_FIX_RESULT: no-op\n",
			wantVerdict: PRFixFlake,
		},
		{
			name:            "flake without a reason sentinel reports no reason",
			output:          "Nothing to change.\nSYBRA_PR_FIX_RESULT: flake\n",
			wantVerdict:     PRFixFlake,
			wantEmptyReason: true,
		},
		{
			name: "legacy conflict abort text",
			output: "The rebase produced 5 conflicting files, which exceeds the limit of 3. " +
				"As instructed, I ran git rebase --abort. This task requires human review.",
			wantVerdict: PRFixHuman,
			wantReason:  "pr-fix agent requested human review: The rebase produced 5 conflicting files, which exceeds the limit of 3. As instructed, I ran git rebase --abort. This task requires human review.",
		},
		{
			name:        "negative human phrase",
			output:      "The conflict is resolved; no human review is required.\nSYBRA_PR_FIX_RESULT: continue\n",
			wantVerdict: PRFixContinue,
		},
		{
			name: "last sentinel wins",
			output: "Example contract:\nSYBRA_PR_FIX_RESULT: human-required\n\nActual result:\n" +
				"SYBRA_PR_FIX_RESULT: continue\n",
			wantVerdict: PRFixContinue,
		},
		{
			name: "flake sentinel beats an earlier contract echo",
			output: "Contract says:\nSYBRA_PR_FIX_RESULT: human-required\n\nActual:\n" +
				"SYBRA_PR_FIX_RESULT: flake\n",
			wantVerdict: PRFixFlake,
		},
		{
			name: "last reason wins",
			output: "SYBRA_PR_FIX_REASON: example only\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: real blocker\n",
			wantVerdict: PRFixHuman,
			wantReason:  "real blocker",
		},
		{
			// Regression for a real production incident: an agent's own diagnosis
			// ran past the old 200-char truncate() cap and got cut mid-sentence
			// before it reached the actual root cause, leaving the task
			// undiagnosable. The reason must survive in full, however long.
			name: "sentinel reason longer than the old 200-char cap survives in full",
			output: "SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: " + strings.Repeat("environment-specific detail ", 20) + "root cause at the end\n",
			wantVerdict: PRFixHuman,
			wantReason:  strings.Repeat("environment-specific detail ", 20) + "root cause at the end",
		},
		{
			name: "legacy free-text reason longer than the old 200-char cap survives in full",
			output: strings.Repeat("investigation detail. ", 20) +
				"As instructed, I ran git rebase --abort. This task requires human review.",
			wantVerdict: PRFixHuman,
			wantReason: "pr-fix agent requested human review: " + strings.Repeat("investigation detail. ", 20) +
				"As instructed, I ran git rebase --abort. This task requires human review.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotVerdict, gotReason := classifyPRFixResult(tc.output)
			if gotVerdict != tc.wantVerdict {
				t.Fatalf("verdict = %v, want %v", gotVerdict, tc.wantVerdict)
			}
			if tc.wantReason != "" && gotReason != tc.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tc.wantReason)
			}
			if tc.wantEmptyReason && gotReason != "" {
				t.Errorf("reason = %q, want empty; a non-human verdict must not inherit the human-required default text", gotReason)
			}
		})
	}
}

func TestExtractPRFixFailingTests(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		output string
		want   []string
	}{
		{
			name: "collects every failing-test line in order",
			output: "SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: targeted tests still fail after the merge\n" +
				"SYBRA_PR_FIX_FAILING_TEST: pkg/foo/bar_test.go:113 TestBootstrap/gateway\n" +
				"SYBRA_PR_FIX_FAILING_TEST: pkg/foo/baz_test.go:49 TestSnapshot\n",
			want: []string{
				"pkg/foo/bar_test.go:113 TestBootstrap/gateway",
				"pkg/foo/baz_test.go:49 TestSnapshot",
			},
		},
		{
			name:   "none reported yields nil",
			output: "SYBRA_PR_FIX_RESULT: human-required\nSYBRA_PR_FIX_REASON: missing credential\n",
			want:   nil,
		},
		{
			name:   "empty output yields nil",
			output: "",
			want:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractPRFixFailingTests(tc.output)
			if len(got) != len(tc.want) {
				t.Fatalf("extractPRFixFailingTests(%q) = %v, want %v", tc.output, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("test[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A second recordPRFixVars call for the same step ID (e.g. a re-armed retry)
// whose output has no failing-test sentinels must clear any failing-tests
// var a prior completion of that same step left behind — otherwise
// prFixFailingTests would resurface a stale, unrelated attempt's tests as if
// they belonged to this one.
func TestRecordPRFixVars_ClearsStaleFailingTestsOnRetry(t *testing.T) {
	t.Parallel()

	wf := &Execution{}
	wf.StepHistory = append(wf.StepHistory, StepRecord{StepID: "fix", Status: "completed", AgentID: "agent-1"})
	recordPRFixVars(wf, "fix", "SYBRA_PR_FIX_RESULT: human-required\n"+
		"SYBRA_PR_FIX_REASON: first attempt found a failing test\n"+
		"SYBRA_PR_FIX_FAILING_TEST: pkg/a_test.go:1 TestA\n")
	if got := prFixFailingTests(wf); len(got) != 1 || got[0] != "pkg/a_test.go:1 TestA" {
		t.Fatalf("after first attempt: prFixFailingTests = %v, want [pkg/a_test.go:1 TestA]", got)
	}

	// A re-armed retry of the same step ID: a second StepRecord for "fix"
	// whose output has no failing-test sentinels this time.
	wf.StepHistory = append(wf.StepHistory, StepRecord{StepID: "fix", Status: "completed", AgentID: "agent-2"})
	recordPRFixVars(wf, "fix", "SYBRA_PR_FIX_RESULT: human-required\n"+
		"SYBRA_PR_FIX_REASON: missing deploy credential this time\n")
	if got := prFixFailingTests(wf); len(got) != 0 {
		t.Fatalf("after second attempt (no tests reported): prFixFailingTests = %v, want none — stale value from the first attempt leaked", got)
	}
}

func TestExecRoutePRFixResult_HumanRequiredStopsBeforeRelink(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Aborted - the rebase hit 5 conflicting files. Human review is required.",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{
		ID:       "t1",
		Status:   "in-progress",
		PRNumber: 1178,
		Workflow: wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1178})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want human-required reason")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "Human review is required") {
		t.Errorf("reason = %q, want agent output excerpt", reason)
	}
}

func TestExecRoutePRFixResult_RecoversResolvedUnmergedConflict(t *testing.T) {
	t.Parallel()

	bare, wtPath := newResolvedUnmergedPRFixWorktree(t, "feat/conflict-recovery")

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})
	engine.SetPushCredentialPreflighter(&fakePushPreflighter{})

	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Conflict resolved on disk but merge not finalized.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: git still reports an unmerged path\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2230,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-recovery",
		Workflow:  wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2230,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-recovery",
	})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output != "continue" {
		t.Fatalf("output = %q, want continue", out.Output)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged in-progress", got.Status)
	}

	statusOut, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("worktree not clean after recovery: %s", statusOut)
	}

	subject, err := exec.Command("git", "-C", wtPath, "log", "-1", "--format=%s").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, subject)
	}
	if got := strings.TrimSpace(string(subject)); got != "fix(recovery): finalize merge resolution" {
		t.Fatalf("last subject = %q, want recovery commit", got)
	}

	localSHA := headSHA(t, wtPath)
	remoteSHAOut, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "rev-parse", "refs/heads/feat/conflict-recovery").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse remote branch: %v: %s", err, remoteSHAOut)
	}
	if remoteSHA := strings.TrimSpace(string(remoteSHAOut)); remoteSHA != localSHA {
		t.Fatalf("remote SHA = %q, want pushed local SHA %q", remoteSHA, localSHA)
	}
}

// TestExecRoutePRFixResult_RecoversResolvedButUnstagedConflict is the exact
// repro from #2232: an agent edited the conflicted file to a marker-free
// resolution but never ran `git add`, so the path is still unmerged in the
// index (`UU`). Unlike TestExecRoutePRFixResult_RecoversResolvedUnmergedConflict,
// this test deliberately skips staging the resolved file to prove the
// recovery path also fires from that unstaged state, not just once the file
// is already staged.
func TestExecRoutePRFixResult_RecoversResolvedButUnstagedConflict(t *testing.T) {
	t.Parallel()

	bare, wtPath := newResolvedUnmergedPRFixWorktree(t, "feat/conflict-recovery-unstaged")

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})
	engine.SetPushCredentialPreflighter(&fakePushPreflighter{})

	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Conflict resolved on disk but merge not finalized.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: git still reports an unmerged path\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2232,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-recovery-unstaged",
		Workflow:  wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2232,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-recovery-unstaged",
	})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output != "continue" {
		t.Fatalf("output = %q, want continue", out.Output)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want unchanged in-progress", got.Status)
	}

	statusOut, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, statusOut)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Fatalf("worktree not clean after recovery: %s", statusOut)
	}

	subject, err := exec.Command("git", "-C", wtPath, "log", "-1", "--format=%s").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v: %s", err, subject)
	}
	if got := strings.TrimSpace(string(subject)); got != "fix(recovery): finalize merge resolution" {
		t.Fatalf("last subject = %q, want recovery commit", got)
	}

	localSHA := headSHA(t, wtPath)
	remoteSHAOut, err := exec.Command("git", "-c", "safe.bareRepository=all", "-C", bare, "rev-parse", "refs/heads/feat/conflict-recovery-unstaged").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse remote branch: %v: %s", err, remoteSHAOut)
	}
	if remoteSHA := strings.TrimSpace(string(remoteSHAOut)); remoteSHA != localSHA {
		t.Fatalf("remote SHA = %q, want pushed local SHA %q", remoteSHA, localSHA)
	}
}

func TestExecRoutePRFixResult_ResolvedMergePushRetryKeepsCheckpointContext(t *testing.T) {
	t.Parallel()

	bare, wtPath := newResolvedUnmergedPRFixWorktree(t, "feat/conflict-retry")
	runGitAt(t, wtPath, "add", filepath.Join("internal", "workflow", "engine_advance.go"))

	// A preflight-only failure now falls through to a real push attempt
	// (#2386) instead of parking, so this must drive the retry with a real
	// push failure: break the remote for the first attempt, restore it
	// before the resumed attempt.
	runGitAt(t, wtPath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "does-not-exist.git"))

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})
	preflight := &fakePushPreflighter{}
	engine.SetPushCredentialPreflighter(preflight)

	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Conflict resolved on disk but merge not finalized.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: git still reports an unmerged path\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{},
	}
	task := TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2234,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-retry",
		Workflow:  wf,
	}
	tasks.Put(task)

	_, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, task)
	if !errors.Is(err, errStepParked) {
		t.Fatalf("first execRoutePRFixResult err = %v, want errStepParked", err)
	}
	if got := wf.Variables[resolvedMergeCheckpointVar("route_pr_fix_result")]; got != "true" {
		t.Fatalf("checkpoint marker = %q, want true", got)
	}
	resolved, err := project.ResolvedUnmergedPaths(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("ResolvedUnmergedPaths after checkpoint: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("resolved paths after checkpoint = %v, want none", resolved)
	}

	runGitAt(t, wtPath, "remote", "set-url", "origin", bare)

	resumedTask, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, resumedTask.Workflow, resumedTask)
	if err != nil {
		t.Fatalf("resumed execRoutePRFixResult: %v", err)
	}
	if out.Output != "continue" {
		t.Fatalf("resumed output = %q, want continue", out.Output)
	}
	if preflight.calls != 2 {
		t.Fatalf("preflight calls = %d, want 2", preflight.calls)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "human-required" {
		t.Fatalf("status = human-required after retry: %s", tasks.Reason("t1"))
	}
}

func TestExecRoutePRFixResult_ResolvedMergeRejectsUnexpectedDirtyPath(t *testing.T) {
	t.Parallel()

	_, wtPath := newResolvedUnmergedPRFixWorktree(t, "feat/conflict-unexpected-dirty")
	runGitAt(t, wtPath, "add", filepath.Join("internal", "workflow", "engine_advance.go"))
	if err := os.WriteFile(filepath.Join(wtPath, "scratch.txt"), []byte("unverified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})
	engine.SetPushCredentialPreflighter(&fakePushPreflighter{})

	beforeSHA := headSHA(t, wtPath)
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Conflict resolved on disk but merge not finalized.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: git still reports an unmerged path\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2233,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-unexpected-dirty",
		Workflow:  wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2233,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-unexpected-dirty",
	})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want human-required dirty-path stop")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "scratch.txt") {
		t.Fatalf("reason = %q, want unexpected dirty path", reason)
	}
	if got := headSHA(t, wtPath); got != beforeSHA {
		t.Fatalf("HEAD = %q, want unchanged %q", got, beforeSHA)
	}
}

func TestExecRoutePRFixResult_HumanRequiredApprovalStopSkipsResolvedMergeRecovery(t *testing.T) {
	t.Parallel()

	_, wtPath := newResolvedUnmergedPRFixWorktree(t, "feat/approved-noop")
	runGitAt(t, wtPath, "add", filepath.Join("internal", "workflow", "engine_advance.go"))

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})
	engine.SetPushCredentialPreflighter(&fakePushPreflighter{})

	beforeSHA := headSHA(t, wtPath)
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Conflict resolved locally, but this approved PR needs no substantive fix.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: already approved; no substantive fix needed; clean/base-only merge would only change the merge-base\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2232,
		ProjectID: "acme/widgets",
		Branch:    "feat/approved-noop",
		Workflow:  wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2232,
		ProjectID: "acme/widgets",
		Branch:    "feat/approved-noop",
	})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want explicit human-required approval stop")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got := headSHA(t, wtPath); got != beforeSHA {
		t.Fatalf("HEAD = %q, want unchanged %q; approval-preservation stop must not checkpoint/push", got, beforeSHA)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "no substantive fix needed") {
		t.Fatalf("reason = %q, want agent's approval-preservation reason", reason)
	}
}

func TestExecRoutePRFixResult_ResolvedUnmergedWithFailingTestsRoutesToTestFix(t *testing.T) {
	t.Parallel()

	_, wtPath := newResolvedUnmergedPRFixWorktree(t, "feat/conflict-with-tests")
	runGitAt(t, wtPath, "add", filepath.Join("internal", "workflow", "engine_advance.go"))

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetWorktreeGetter(&fakeWorktreeGetter{path: wtPath, ok: true})
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})
	engine.SetPushCredentialPreflighter(&fakePushPreflighter{})

	beforeSHA := headSHA(t, wtPath)
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Conflict resolved on disk but targeted tests still fail.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: targeted tests still fail after resolving the merge\n" +
				"SYBRA_PR_FIX_FAILING_TEST: internal/workflow/engine_steps_prfix_test.go:1 TestStillFailing\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{},
	}
	tasks.Put(TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2231,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-with-tests",
		Workflow:  wf,
	})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{
		ID:        "t1",
		Status:    "in-progress",
		PRNumber:  2231,
		ProjectID: "acme/widgets",
		Branch:    "feat/conflict-with-tests",
	})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want scoped test-fix before resolved-merge recovery")
	}
	if v := wf.Variables["step.route_pr_fix_result."+prFixTestFixEligibleVar]; v != "true" {
		t.Errorf("pr_fix_test_fix_eligible = %q, want \"true\"", v)
	}
	if got := headSHA(t, wtPath); got != beforeSHA {
		t.Fatalf("HEAD = %q, want unchanged %q; route must not checkpoint before test_fix", got, beforeSHA)
	}
}

func TestResolvedMergeFocusedCommandsReturnsChangedFilesError(t *testing.T) {
	t.Parallel()

	engine := NewEngine(newTestStore(t), newMemTasks(), newMockAgents(), discardLogger())
	engine.SetCheckConfigGetter(&fakeCheckGetter{focused: []project.FocusedCheck{{
		Name:     "workflow",
		Paths:    []string{"internal/workflow/**"},
		Commands: []string{"true"},
	}}})

	cmds, files, err := engine.resolvedMergeFocusedCommands(
		context.Background(),
		"t1",
		t.TempDir(),
		[]string{"internal/workflow/engine_advance.go"},
	)
	if err == nil {
		t.Fatalf("resolvedMergeFocusedCommands err = nil, cmds = %v; want changed-file discovery error", cmds)
	}
	if len(cmds) != 0 {
		t.Fatalf("cmds = %v, want none on changed-file discovery error", cmds)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none on changed-file discovery error", files)
	}
}

func newResolvedUnmergedPRFixWorktree(t *testing.T, branch string) (bare, wtPath string) {
	t.Helper()

	bare, wtPath = newPRWorktree(t, branch)
	conflictPath := filepath.Join("internal", "workflow", "engine_advance.go")
	if err := os.MkdirAll(filepath.Join(wtPath, "internal", "workflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, conflictPath), []byte("feature branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, wtPath, "add", conflictPath)
	runGitAt(t, wtPath, "commit", "-m", "feat: branch side")
	if err := project.PushSync(context.Background(), wtPath, branch); err != nil {
		t.Fatalf("PushSync seed: %v", err)
	}

	baseWT := filepath.Join(t.TempDir(), "base")
	if err := project.CreateWorktreeExisting(context.Background(), bare, baseWT, "main"); err != nil {
		t.Fatalf("CreateWorktreeExisting(main): %v", err)
	}
	runGitAt(t, baseWT, "config", "user.email", "test@test.com")
	runGitAt(t, baseWT, "config", "user.name", "Test")
	if err := os.MkdirAll(filepath.Join(baseWT, "internal", "workflow"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseWT, conflictPath), []byte("main branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitAt(t, baseWT, "add", conflictPath)
	runGitAt(t, baseWT, "commit", "-m", "feat: base side")
	runGitAt(t, wtPath, "update-ref", "refs/remotes/origin/main", "refs/heads/main")

	cmd := exec.Command("git", "merge", "refs/heads/main")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git merge unexpectedly succeeded: %s", out)
	}
	if err := os.WriteFile(filepath.Join(wtPath, conflictPath), []byte("feature branch\nmain branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bare, wtPath
}

// A human-required verdict carrying SYBRA_PR_FIX_FAILING_TEST: lines must
// append the full, untruncated list to the task body — not just the
// 200-char-truncated reason — so a human or a future scoped follow-up gets
// exact repro info instead of having to rediscover the failing tests.
// The original router (route_pr_fix_result) must redirect to the scoped
// test_fix follow-up instead of parking immediately, when the pr-fix agent
// named specific non-merge test failures. It must NOT touch task status or
// append the failing-tests note itself — that's test_fix's own router's job
// once test_fix has actually had its one bounded attempt.
func TestExecRoutePRFixResult_HumanRequiredWithFailingTestsRoutesToTestFix(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Merged cleanly but targeted tests still fail.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: branch merge is local-only; targeted tests still fail\n" +
				"SYBRA_PR_FIX_FAILING_TEST: pkg/xds/bootstrap/generator_test.go:113 TestBootstrap/gateway_settings\n" +
				"SYBRA_PR_FIX_FAILING_TEST: pkg/xds/sync/proxy_builder_test.go:49 panics during spec construction\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 1178, Workflow: wf})

	if _, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1178}); err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress (must not park before test_fix's own attempt)", got.Status)
	}
	if strings.Contains(got.Body, "PR-Fix: Failing Tests") {
		t.Errorf("must not append the failing-tests note before test_fix has attempted a fix; got body:\n%s", got.Body)
	}
	if v := wf.Variables["step.route_pr_fix_result."+prFixTestFixEligibleVar]; v != "true" {
		t.Errorf("pr_fix_test_fix_eligible = %q, want \"true\"", v)
	}
}

// route_test_fix_result reuses execRoutePRFixResult unchanged, but is never
// itself eligible to redirect back to test_fix — a step ID other than
// route_pr_fix_result must always park on a human-required verdict, bounding
// the follow-up to exactly one attempt even if test_fix's own output also
// names still-failing tests.
func TestExecRoutePRFixResult_TestFixOwnHumanRequiredParksImmediately(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_test_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{
			{StepID: "fix", Status: "completed", AgentID: "agent-1", Output: "SYBRA_PR_FIX_RESULT: human-required\nSYBRA_PR_FIX_REASON: first pass\nSYBRA_PR_FIX_FAILING_TEST: pkg/a_test.go:1 TestA\n"},
			{
				StepID: "test_fix",
				Status: "completed",
				Output: "Tried the narrow fix but it still fails.\n" +
					"SYBRA_PR_FIX_RESULT: human-required\n" +
					"SYBRA_PR_FIX_REASON: fix attempt did not resolve TestA\n" +
					"SYBRA_PR_FIX_FAILING_TEST: pkg/a_test.go:1 TestA (still failing after fix attempt)\n",
				AgentID:   "agent-2",
				StartedAt: time.Now(),
			},
		},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 1178, Workflow: wf})

	if _, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_test_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1178}); err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (test_fix's own attempt must be the last one)", got.Status)
	}
	if !strings.Contains(got.Body, "pkg/a_test.go:1 TestA (still failing after fix attempt)") {
		t.Errorf("expected test_fix's own still-failing test in the note; got body:\n%s", got.Body)
	}
	if v := wf.Variables["step.route_test_fix_result."+prFixTestFixEligibleVar]; v != "" {
		t.Errorf("route_test_fix_result must never set pr_fix_test_fix_eligible; got %q", v)
	}
}

// A human-required verdict with no SYBRA_PR_FIX_FAILING_TEST: lines (e.g. a
// missing-credential or ambiguous-scope reason) must not append a failing
// tests section at all — the note would be actively misleading if empty.
func TestExecRoutePRFixResult_HumanRequiredWithoutFailingTestsNoNote(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: missing deploy credential, cannot verify\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 1178, Workflow: wf})

	if _, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1178}); err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Body, "PR-Fix: Failing Tests") {
		t.Errorf("expected no failing-tests section without any reported tests; got body:\n%s", got.Body)
	}
}

func TestExecRoutePRFixResult_NoPRRemoteOutageResumesRecovery(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "branch-conflict-fix",
		CurrentStep: "route_result",
		State:       ExecRunning,
		Variables: map[string]string{
			resumeStatusVar:       "in-progress",
			resumeStatusReasonVar: "",
			resumeWorkflowIDVar:   "simple-task-implement",
			resumeWorkflowStepVar: "implement",
		},
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Focused regression tests passed.\n" +
				"SYBRA_PR_FIX_RESULT: human-required\n" +
				"SYBRA_PR_FIX_REASON: GitHub remote unreachable from this environment; fetch/push blocked by HTTPS transport failure to github.com:443: Failed to connect to github.com port 443\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "Automaat/sybra", PRNumber: 0, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_result"}, wf, TaskInfo{ID: "t1", ProjectID: "Automaat/sybra"})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if !strings.Contains(out.Output, "retryable no-PR remote outage") {
		t.Fatalf("output = %q, want retryable no-PR remote outage", out.Output)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "human-required" {
		t.Fatalf("status = %q, want recovery to continue toward resume_original", got.Status)
	}
	if got.Status != "in-progress" {
		t.Fatalf("status = %q, want in-progress", got.Status)
	}
}

// A flake verdict must not park a human and must not reach verify_commits,
// which would fail the task for the missing commit the honest answer implies.
func TestExecRoutePRFixResult_FlakeRoutesToInReviewWithoutCommit(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Same job fails on base.\nSYBRA_PR_FIX_RESULT: flake\nSYBRA_PR_FIX_REASON: e2e provisioning timeout, reproduces on base\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "acme/widgets", PRNumber: 1178, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", ProjectID: "acme/widgets", PRNumber: 1178})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want the flake message so verify_commits is skipped")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-review" {
		t.Fatalf("status = %q, want in-review (a flake must never park a human)", got.Status)
	}
	if reason := tasks.Reason("t1"); !strings.Contains(reason, "reproduces on base") {
		t.Errorf("reason = %q, want the agent's flake evidence", reason)
	}
}

// A review-hold park must beat a flake sentinel: the drafted pending review
// still needs a human to submit it regardless of why CI failed.
func TestExecRoutePRFixResult_ReviewHoldParkBeatsFlake(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "SYBRA_PR_FIX_RESULT: flake\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", Workflow: wf})

	if _, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1"}); err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (review hold must beat a flake sentinel)", got.Status)
	}
}

// TestExecRoutePRFixResult_ReProbesResolvedRemotePR pins the bug report's
// scenario: the pr-fix agent correctly declined to push because its local
// worktree was stale/diverged, but the remote PR is already green and
// mergeable (an external bot fixed it out from under the task). The step
// must re-probe and route to in-review instead of parking human-required.
func TestExecRoutePRFixResult_ReProbesResolvedRemotePR(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetPRStateFetcher(scriptedPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}})
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Local worktree is diverged from origin; declining to push.\nSYBRA_PR_FIX_RESULT: human-required\n",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "acme/widgets", PRNumber: 1178, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", ProjectID: "acme/widgets", PRNumber: 1178})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue, want resolved-on-remote message")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in-review" {
		t.Fatalf("status = %q, want in-review", got.Status)
	}
}

// TestExecRoutePRFixResult_ReviewHoldParkIgnoresResolvedRemotePR asserts the
// re-probe never overrides a review-hold park: that park exists because a
// pending review draft needs a human to submit it, which is orthogonal to
// whether CI is green.
func TestExecRoutePRFixResult_ReviewHoldParkIgnoresResolvedRemotePR(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	engine.SetPRStateFetcher(scriptedPRStateFetcher{state: github.PRState{State: "OPEN", Mergeable: "MERGEABLE"}})
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Pushed the fix.\nSYBRA_PR_FIX_RESULT: continue",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", ProjectID: "acme/widgets", PRNumber: 1446, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", ProjectID: "acme/widgets", PRNumber: 1446})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue; review-hold park must force human-required")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required (review-hold must not be waved through)", got.Status)
	}
}

func TestExecRoutePRFixResult_ReviewHoldParkWinsOverContinue(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	// The agent pushed in review-hold push mode and reported `continue`; the
	// deterministic park var must still route the task to human-required so the
	// drafted pending review isn't silently left unsubmitted.
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID:    "fix",
			Status:    "completed",
			Output:    "Pushed the fix.\nSYBRA_PR_FIX_RESULT: continue",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 1446, Workflow: wf})

	out, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1446})
	if err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	if out.Output == "continue" {
		t.Fatal("route output = continue; review-hold park must force human-required")
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
}

// A review-hold park is forced from a `continue` verdict — the agent already
// pushed successfully. Any SYBRA_PR_FIX_FAILING_TEST: line in that output
// describes a test the agent already dealt with, not the reason for this
// park, and must never be attributed to it via the failing-tests note.
func TestExecRoutePRFixResult_ReviewHoldParkIgnoresFailingTestLines(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	tasks := newMemTasks()
	engine := NewEngine(store, tasks, newMockAgents(), discardLogger())
	wf := &Execution{
		WorkflowID:  "pr-fix",
		CurrentStep: "route_pr_fix_result",
		State:       ExecRunning,
		StepHistory: []StepRecord{{
			StepID: "fix",
			Status: "completed",
			Output: "Pushed the fix.\n" +
				"SYBRA_PR_FIX_FAILING_TEST: pkg/foo_test.go:42 TestBar (was failing before my fix)\n" +
				"SYBRA_PR_FIX_RESULT: continue",
			AgentID:   "agent-1",
			StartedAt: time.Now(),
		}},
		Variables: map[string]string{ReviewHoldParkVar: "true"},
	}
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress", PRNumber: 1446, Workflow: wf})

	if _, err := engine.execRoutePRFixResult("t1", &Step{ID: "route_pr_fix_result"}, wf, TaskInfo{ID: "t1", PRNumber: 1446}); err != nil {
		t.Fatalf("execRoutePRFixResult: %v", err)
	}
	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if strings.Contains(got.Body, "PR-Fix: Failing Tests") {
		t.Errorf("review-hold park must not carry a failing-tests note misattributed from a continue verdict; got body:\n%s", got.Body)
	}
}

func TestPRFixRequiresHuman_UsesLastAgentStepVars(t *testing.T) {
	t.Parallel()

	wf := &Execution{
		StepHistory: []StepRecord{
			{
				StepID:  "old_fix",
				Status:  "completed",
				Output:  "SYBRA_PR_FIX_RESULT: human-required\nSYBRA_PR_FIX_REASON: stale\n",
				AgentID: "agent-1",
			},
			{
				StepID:  "repair_conflicts",
				Status:  "completed",
				Output:  "SYBRA_PR_FIX_RESULT: human-required\nSYBRA_PR_FIX_REASON: ignored because explicit false var wins\n",
				AgentID: "agent-2",
			},
		},
		Variables: map[string]string{
			"step.old_fix.pr_fix_requires_human":          "true",
			"step.old_fix.pr_fix_reason":                  "stale",
			"step.repair_conflicts.pr_fix_requires_human": "false",
			"step.repair_conflicts.pr_fix_reason":         "",
			"step.repair_conflicts.output":                "SYBRA_PR_FIX_RESULT: human-required\n",
		},
	}

	gotVerdict, gotReason := prFixVerdict(wf)
	if gotVerdict != PRFixContinue {
		t.Fatalf("verdict = %v, reason %q; want continue from latest agent step's explicit false var", gotVerdict, gotReason)
	}
}

func TestPRFixVerdict_VerdictVarWinsOverLegacyBool(t *testing.T) {
	t.Parallel()

	wf := &Execution{
		StepHistory: []StepRecord{{
			StepID:  "fix",
			Status:  "completed",
			Output:  "SYBRA_PR_FIX_RESULT: flake\n",
			AgentID: "agent-1",
		}},
		Variables: map[string]string{
			"step.fix." + PRFixVerdictVar:    string(PRFixFlake),
			"step.fix.pr_fix_requires_human": "false",
			"step.fix.pr_fix_reason":         "unrelated e2e provisioning failure",
		},
	}

	gotVerdict, gotReason := prFixVerdict(wf)
	if gotVerdict != PRFixFlake {
		t.Fatalf("verdict = %v, want %v", gotVerdict, PRFixFlake)
	}
	if gotReason != "unrelated e2e provisioning failure" {
		t.Errorf("reason = %q, want the flake reason", gotReason)
	}
}

func TestPRFixVerdict_LegacyExecutionWithoutVerdictVarParksHuman(t *testing.T) {
	t.Parallel()

	wf := &Execution{
		StepHistory: []StepRecord{{
			StepID:  "fix",
			Status:  "completed",
			Output:  "SYBRA_PR_FIX_RESULT: human-required\n",
			AgentID: "agent-1",
		}},
		Variables: map[string]string{
			"step.fix.pr_fix_requires_human": "true",
			"step.fix.pr_fix_reason":         "needs a human",
		},
	}

	gotVerdict, gotReason := prFixVerdict(wf)
	if gotVerdict != PRFixHuman {
		t.Fatalf("verdict = %v, want %v for a pre-flake execution", gotVerdict, PRFixHuman)
	}
	if gotReason != "needs a human" {
		t.Errorf("reason = %q, want the legacy reason", gotReason)
	}
}

func TestAdvanceStep_PRFixHumanRequiredUsesUntruncatedOutput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "pr-fix-route-test",
		Name: "PR fix route test",
		Steps: []Step{
			{
				ID:   "fix",
				Type: StepRunAgent,
				Config: StepConfig{
					Role:   "pr-fix",
					Prompt: "fix",
				},
				Next: []Transition{{GoTo: "route_pr_fix_result"}},
			},
			{
				ID:   "route_pr_fix_result",
				Type: StepRoutePRFixResult,
				Next: []Transition{
					{
						When: &Condition{Field: "task.status", Operator: "equals", Value: "human-required"},
						GoTo: "",
					},
					{GoTo: ""},
				},
			},
		},
	}); err != nil {
		t.Fatalf("save workflow: %v", err)
	}

	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	if err := engine.StartWorkflow("t1", "pr-fix-route-test"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	longOutput := strings.Repeat("progress details\n", 400) +
		"SYBRA_PR_FIX_RESULT: human-required\n" +
		"SYBRA_PR_FIX_REASON: 5 conflicts exceed the limit\n"
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "fix",
		Status:  "completed",
		Output:  longOutput,
		AgentID: "agent-1",
	}); err != nil {
		t.Fatalf("advance: %v", err)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if reason := tasks.Reason("t1"); reason != "5 conflicts exceed the limit" {
		t.Errorf("reason = %q, want sentinel reason", reason)
	}
	if got.Workflow == nil || got.Workflow.State != ExecCompleted {
		t.Fatalf("workflow state = %+v, want completed", got.Workflow)
	}
}

// recordPRFixVars must also classify a completed test-fix (not just pr-fix)
// step — otherwise prFixFailingTests falls back to re-parsing the step's
// recorded output, which RecordStep truncates to 4000 chars, silently
// reintroducing the truncation issue #2223 fixed for the original pr-fix
// step. Mirrors TestAdvanceStep_PRFixHumanRequiredUsesUntruncatedOutput but
// for role test-fix, with the failing-test sentinel placed past the 4000-
// char boundary so a truncating code path would lose it.
func TestAdvanceStep_TestFixHumanRequiredUsesUntruncatedOutput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	if err := store.Save(Definition{
		ID:   "test-fix-route-test",
		Name: "test-fix route test",
		Steps: []Step{
			{
				ID:   "test_fix",
				Type: StepRunAgent,
				Config: StepConfig{
					Role:   "test-fix",
					Prompt: "test_fix",
				},
				Next: []Transition{{GoTo: "route_test_fix_result"}},
			},
			{
				ID:   "route_test_fix_result",
				Type: StepRoutePRFixResult,
				Next: []Transition{
					{
						When: &Condition{Field: "task.status", Operator: "equals", Value: "human-required"},
						GoTo: "",
					},
					{GoTo: ""},
				},
			},
		},
	}); err != nil {
		t.Fatalf("save workflow: %v", err)
	}

	tasks := newMemTasks()
	agents := newMockAgents()
	engine := NewEngine(store, tasks, agents, discardLogger())
	tasks.Put(TaskInfo{ID: "t1", Status: "in-progress"})
	if err := engine.StartWorkflow("t1", "test-fix-route-test"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	longOutput := strings.Repeat("progress details\n", 400) +
		"SYBRA_PR_FIX_RESULT: human-required\n" +
		"SYBRA_PR_FIX_REASON: fix attempt did not resolve the test\n" +
		"SYBRA_PR_FIX_FAILING_TEST: pkg/a_test.go:1 TestA (still failing after fix attempt)\n"
	if err := engine.AdvanceStep("t1", StepOutput{
		StepID:  "test_fix",
		Status:  "completed",
		Output:  longOutput,
		AgentID: "agent-1",
	}); err != nil {
		t.Fatalf("advance: %v", err)
	}

	got, err := tasks.GetTask("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "human-required" {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if !strings.Contains(got.Body, "pkg/a_test.go:1 TestA (still failing after fix attempt)") {
		t.Errorf("expected the untruncated failing test in body; got:\n%s", got.Body)
	}
}

func TestIsCodeAuthorRole_IncludesTestFix(t *testing.T) {
	t.Parallel()
	if !isCodeAuthorRole("test-fix") {
		t.Error("isCodeAuthorRole(\"test-fix\") = false, want true")
	}
}

func TestTamperCodeAuthorRole_IncludesTestFix(t *testing.T) {
	t.Parallel()
	if !tamperCodeAuthorRole("test-fix") {
		t.Error("tamperCodeAuthorRole(\"test-fix\") = false, want true")
	}
}

func TestIsCodeAuthorRun_IncludesTestFix(t *testing.T) {
	t.Parallel()
	if !isCodeAuthorRun(AgentRunInfo{Role: "test-fix"}) {
		t.Error("isCodeAuthorRun({Role: \"test-fix\"}) = false, want true")
	}
}
