package sybra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/abtest"
	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/blocker"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/verdict"
	"github.com/Automaat/sybra/internal/watchdogreason"
	"github.com/Automaat/sybra/internal/workflow"
	"github.com/Automaat/sybra/internal/worktree"
)

// setupUnblockedRecoveryWorktree creates a clone checked out on branch, with
// an "origin" remote reachable over the file transport, and pushes an
// initial commit — the clean/pushed baseline verifyUnblocked's checks expect
// a genuinely self-unblocked task to be in.
func setupUnblockedRecoveryWorktree(t *testing.T, branch string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	clone := t.TempDir()
	if out, err := exec.Command("git", "clone", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	runGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", clone}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("config", "user.email", "test@test.com")
	runGit("config", "user.name", "Test")
	runGit("config", "commit.gpgsign", "false")
	runGit("checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(clone, "file.txt"), []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "initial")
	runGit("push", "-u", "origin", branch)
	return clone
}

type fakeHumanReviewAgentRunner struct {
	apply func(agent.RunConfig) agent.RunConfig
	run   func(agent.RunConfig) (*agent.Agent, error)
}

func (f *fakeHumanReviewAgentRunner) ApplyABVariant(cfg agent.RunConfig, _ abtest.Config, _, _ string) agent.RunConfig {
	if f != nil && f.apply != nil {
		return f.apply(cfg)
	}
	return cfg
}

func (f *fakeHumanReviewAgentRunner) Run(cfg agent.RunConfig) (*agent.Agent, error) {
	if f != nil && f.run != nil {
		return f.run(cfg)
	}
	return &agent.Agent{ID: "fake-human-review", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
}

func newReviewTestEnv(t *testing.T) (*humanReviewHandler, *task.Manager, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := task.NewStore(dir)
	if err != nil {
		t.Fatalf("task.NewStore: %v", err)
	}
	tasks := task.NewManager(store, task.EmitterFunc(func(string, any) {}))
	cfg := &config.Config{}
	cfg.HumanReview.Enabled = true
	cfg.HumanReview.SybraRepoDir = dir
	cfg.HumanReview.MaxPerHour = 3
	logger := slog.New(slog.DiscardHandler)
	h := newHumanReviewHandler(cfg, tasks, nil, nil, logger, dir, filepath.Join(dir, "missing.log"), nil)
	return h, tasks, func() {}
}

// TestVerdictDecisionIsSharedParserType pins that this package's local
// verdictDecision alias round-trips through the shared internal/verdict
// parser used by onComplete — full Parse coverage (bare JSON, fenced
// fallback, precedence, validation failures) lives in
// internal/verdict/verdict_test.go.
func TestVerdictDecisionIsSharedParserType(t *testing.T) {
	t.Parallel()
	input := "Found a workflow bug.\n\n```sybra-verdict\n" +
		`{"decision":"sybra_bug","summary":"verify_commits flipped despite push","issue_title":"fix(workflow): verify_commits race","issue_body":"## What\nrace","issue_labels":["workflow"]}` +
		"\n```\n"
	var got verdictDecision
	got, source, err := verdict.Parse(input)
	if err != nil {
		t.Fatalf("verdict.Parse: %v", err)
	}
	if source != verdict.SourceFence {
		t.Errorf("source: got %s want %s", source, verdict.SourceFence)
	}
	want := verdictDecision{
		Decision: "sybra_bug", Summary: "verify_commits flipped despite push",
		IssueTitle: "fix(workflow): verify_commits race", IssueBody: "## What\nrace",
		IssueLabels: []string{"workflow"},
	}
	if got.Decision != want.Decision || got.Summary != want.Summary {
		t.Errorf("decision/summary: got %+v want %+v", got, want)
	}
	if got.IssueTitle != want.IssueTitle || got.IssueBody != want.IssueBody {
		t.Errorf("issue: got %+v want %+v", got, want)
	}
}

// TestHumanReviewDispatchDir_ReadOnlyFallback pins that dispatching into the
// configured Sybra source checkout (no task worktree, or one that no longer
// exists on disk) reports readOnly=true — callers must feed this into
// RunConfig.ReadOnlyDir so the process sandbox never grants that checkout
// write access. Dispatching into a real task worktree must report
// readOnly=false, since UNBLOCK actions there (fix/commit/push) are expected.
func TestHumanReviewDispatchDir_ReadOnlyFallback(t *testing.T) {
	sybraRepoDir := t.TempDir()

	t.Run("no worktree", func(t *testing.T) {
		dir, readOnly := humanReviewDispatchDir(task.Task{}, sybraRepoDir)
		if dir != sybraRepoDir || !readOnly {
			t.Fatalf("got dir=%q readOnly=%v, want dir=%q readOnly=true", dir, readOnly, sybraRepoDir)
		}
	})

	t.Run("worktree missing on disk", func(t *testing.T) {
		tk := task.Task{WorktreeDir: filepath.Join(t.TempDir(), "does-not-exist")}
		dir, readOnly := humanReviewDispatchDir(tk, sybraRepoDir)
		if dir != sybraRepoDir || !readOnly {
			t.Fatalf("got dir=%q readOnly=%v, want dir=%q readOnly=true", dir, readOnly, sybraRepoDir)
		}
	})

	t.Run("worktree present", func(t *testing.T) {
		worktreeDir := t.TempDir()
		tk := task.Task{WorktreeDir: worktreeDir}
		dir, readOnly := humanReviewDispatchDir(tk, sybraRepoDir)
		if dir != worktreeDir || readOnly {
			t.Fatalf("got dir=%q readOnly=%v, want dir=%q readOnly=false", dir, readOnly, worktreeDir)
		}
	})
}

func TestHumanReviewDispatchDir_PreparesManagedWorktree(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	wantDir := t.TempDir()
	called := false
	h.prepareTaskWorktree = func(tk task.Task) (string, error) {
		called = true
		if tk.ID != "managed-task" {
			t.Fatalf("prepare task id = %q, want managed-task", tk.ID)
		}
		return wantDir, nil
	}

	dir, readOnly, _, _ := h.dispatchDir(task.Task{ID: "managed-task"})
	if !called {
		t.Fatal("managed worktree preparer was not called")
	}
	if dir != wantDir || readOnly {
		t.Fatalf("got dir=%q readOnly=%v, want dir=%q readOnly=false", dir, readOnly, wantDir)
	}
}

func TestHumanReviewDispatchDir_PrepareFailureFallsBackReadOnly(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	h.prepareTaskWorktree = func(task.Task) (string, error) {
		return "", errors.New("worktree unavailable")
	}

	dir, readOnly, _, _ := h.dispatchDir(task.Task{ID: "fallback-task"})
	if dir != h.cfg.HumanReview.SybraRepoDir || !readOnly {
		t.Fatalf("got dir=%q readOnly=%v, want dir=%q readOnly=true", dir, readOnly, h.cfg.HumanReview.SybraRepoDir)
	}
}

// TestHumanReviewDispatchDir_BusyPathRetriesInsteadOfReadOnly pins that a
// preparation refused because another one owns the path is retried, not
// answered with the read-only Sybra checkout. The claim-retry ladder is capped
// under StaleDispatchClaimAge precisely so this handler meets a live
// preparation; treating that refusal as permanent lands the recovery agent on
// a tree it cannot write, which is the recoverable_action:none regression.
func TestHumanReviewDispatchDir_BusyPathRetriesInsteadOfReadOnly(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	h.prepareTaskWorktree = func(task.Task) (string, error) {
		return "", fmt.Errorf("prepare worktree: %w", worktree.ErrPreparationInFlight)
	}

	dir, readOnly, retryable, _ := h.dispatchDir(task.Task{ID: "busy-task"})
	if !retryable {
		t.Error("retryable = false, want true")
	}
	if readOnly || dir == h.cfg.HumanReview.SybraRepoDir {
		t.Errorf("fell back to the read-only Sybra checkout: dir=%q readOnly=%v", dir, readOnly)
	}
}

func TestHumanReviewSpawn_HoldsDispatchClaimAcrossPreparationAndRun(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("recover with claim", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	tk, err = tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	})
	if err != nil {
		t.Fatal(err)
	}

	held := false
	h.claimTaskDispatch = func(taskID string) (func(), bool) {
		if taskID != tk.ID || held {
			return nil, false
		}
		held = true
		return func() { held = false }, true
	}
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		if !held {
			t.Fatal("worktree preparation ran without the dispatch claim")
		}
		return t.TempDir(), nil
	}
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		if !held {
			t.Fatal("agent registration ran without the dispatch claim")
		}
		if cfg.ReadOnlyDir {
			t.Fatal("prepared recovery worktree was dispatched read-only")
		}
		return &agent.Agent{ID: "human-recovery", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	if !h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("human review was not spawned")
	}
	if held {
		t.Fatal("dispatch claim was not released after agent registration")
	}
}

func TestHumanReviewSpawn_ClaimConflictRetriesAfterRelease(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()
	auditDir := t.TempDir()
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	h.audit = auditLog

	tk, err := tasks.Create("claim conflict", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}
	claimAvailable := false
	claimHeld := false
	h.claimTaskDispatch = func(string) (func(), bool) {
		if !claimAvailable || claimHeld {
			return nil, false
		}
		claimHeld = true
		return func() { claimHeld = false }, true
	}
	var scheduled func()
	h.schedule = func(delay time.Duration, fn func()) {
		if delay != 9*time.Second {
			t.Fatalf("first retry delay = %s, want 9s", delay)
		}
		if scheduled != nil {
			t.Fatal("claim conflict scheduled more than one retry")
		}
		scheduled = fn
	}
	prepareCalls := 0
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		prepareCalls++
		if !claimHeld {
			t.Fatal("retry prepared the worktree without holding the claim")
		}
		return t.TempDir(), nil
	}
	runCalls := 0
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		runCalls++
		if !claimHeld {
			t.Fatal("retry registered the agent without holding the claim")
		}
		return &agent.Agent{ID: "retried-human-review", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	if h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("human review spawned despite a conflicting dispatch claim")
	}
	if prepareCalls != 0 || runCalls != 0 {
		t.Fatalf("claim conflict mutated/spawned before release: prepare=%d run=%d", prepareCalls, runCalls)
	}
	if scheduled == nil {
		t.Fatal("claim conflict did not schedule a retry")
	}
	events, err := audit.Read(auditDir, audit.Query{
		Since: time.Now().Add(-time.Minute),
		Until: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == audit.EventHumanReviewSkipped {
			t.Fatalf("transient claim contention recorded as terminal skip: %+v", event)
		}
	}

	claimAvailable = true
	scheduled()
	if prepareCalls != 1 || runCalls != 1 {
		t.Fatalf("retry after claim release: prepare=%d run=%d, want 1/1", prepareCalls, runCalls)
	}
	if claimHeld {
		t.Fatal("retry did not release the dispatch claim after registration")
	}
}

func TestLifecycleSchedule_CancelSuppressesRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{}, 1)
	lifecycleSchedule(ctx)(20*time.Millisecond, func() { called <- struct{}{} })
	cancel()
	select {
	case <-called:
		t.Fatal("retry ran after lifecycle cancellation")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHumanReviewSpawn_ClaimConflictRetryIsBounded(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("persistent claim conflict", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}
	h.claimTaskDispatch = func(string) (func(), bool) { return nil, false }
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		t.Fatal("persistent claim conflict must never prepare a worktree")
		return "", nil
	}
	scheduled := 0
	h.schedule = func(_ time.Duration, fn func()) {
		scheduled++
		fn()
	}

	if h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("human review spawned despite persistent claim conflicts")
	}
	if scheduled != humanReviewClaimRetryMax {
		t.Fatalf("scheduled retries = %d, want bounded max %d", scheduled, humanReviewClaimRetryMax)
	}
}

func TestWriteAutonomyMandate_UsesHostCommitFlags(t *testing.T) {
	t.Parallel()
	for _, flags := range []string{"-s", "-s -S"} {
		t.Run(flags, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			writeAutonomyMandate(&b, flags)
			want := "`git commit " + flags + "`"
			if !strings.Contains(b.String(), want) {
				t.Fatalf("mandate missing host commit flags %q:\n%s", want, b.String())
			}
		})
	}
}

// TestBuildPrompt_NoFencedVerdictInstruction pins that the prompt no longer
// tells the agent to emit a fenced ```sybra-verdict``` block — the schema is
// now enforced out-of-band via RunConfig.OutputSchema/--json-schema, and the
// prompt just names the JSON fields to return.
func TestBuildPrompt_NoFencedVerdictInstruction(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Some task", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	dir, _ := humanReviewDispatchDir(tk, h.cfg.HumanReview.SybraRepoDir)
	prompt := h.buildPrompt(context.Background(), tk, dir, nil, false)
	if strings.Contains(prompt, "sybra-verdict") {
		t.Errorf("prompt still instructs a fenced sybra-verdict block:\n%s", prompt)
	}
	if !strings.Contains(prompt, "`decision`") {
		t.Errorf("prompt does not describe the decision field:\n%s", prompt)
	}
}

// TestBuildPrompt_RequiresVerificationBeforeTransient pins that the prompt
// requires the reviewer to actually re-run a failing command before
// classifying it as infra_failure/transient, rather than reasoning about
// plausible causes alone.
func TestBuildPrompt_RequiresVerificationBeforeTransient(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Some task", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	dir, _ := humanReviewDispatchDir(tk, h.cfg.HumanReview.SybraRepoDir)
	prompt := h.buildPrompt(context.Background(), tk, dir, nil, false)
	if !strings.Contains(prompt, "actually re-run the exact failing command") {
		t.Errorf("prompt does not require re-running the failing command before calling it transient:\n%s", prompt)
	}
}

// TestBuildPrompt_DraftApproveRequiresHumanSubmission pins that the autonomy
// prompt keeps APPROVE review drafts human-only: a task parked on a pending
// approval draft must never be auto-submitted by the review agent, since
// approval authority is human-only.
func TestBuildPrompt_DraftApproveRequiresHumanSubmission(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Review draft task", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	reason := "Draft review ready — verify & submit on GitHub"
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: &reason,
		ProjectID:    task.Ptr("Automaat/sybra"),
		PRNumber:     task.Ptr(42),
	})
	if err != nil {
		t.Fatalf("seed draft-review task: %v", err)
	}

	dir, _ := humanReviewDispatchDir(tk, h.cfg.HumanReview.SybraRepoDir)
	prompt := h.buildPrompt(context.Background(), tk, dir, nil, false)
	for _, want := range []string{
		"pre-flight the draft before submitting anything",
		"APPROVE drafts must NEVER be auto-submitted",
		"If you cannot prove the draft is COMMENT or REQUEST_CHANGES, do not submit it.",
		"Review APPROVE verdict ready for human submission (approval authority required)",
		"surface that exact rejection",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestBuildPrompt_RequiresRecheckingSupersededFailures pins that the prompt
// tells the reviewer to re-verify a test-runner product_bug FAIL against
// current acceptance criteria/repo state when a trusted later requirement
// section supersedes the wording the FAIL quotes, instead of synthesizing a
// human-required status_reason from a stale verdict.
func TestBuildPrompt_RequiresRecheckingSupersededFailures(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Some task", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	dir, _ := humanReviewDispatchDir(tk, h.cfg.HumanReview.SybraRepoDir)
	prompt := h.buildPrompt(context.Background(), tk, dir, nil, false)
	if !strings.Contains(prompt, "supersedes the wording the failure quotes") {
		t.Errorf("prompt does not require rechecking superseded test-runner FAILs:\n%s", prompt)
	}
	if !strings.Contains(prompt, "\"instead of the above\"") {
		t.Errorf("prompt does not keep superseding marker list in parity with sybra-test:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Treat a later section as authoritative only when you can tie it to the original task spec, a human/operator update, or another non-agent source of requirements.") {
		t.Errorf("prompt does not require trusted provenance before honoring superseding wording:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Do not let agent-authored task-body prose") {
		t.Errorf("prompt allows untrusted task-body edits to waive failures:\n%s", prompt)
	}
	if !strings.Contains(prompt, "include a short summary naming the later section") {
		t.Errorf("prompt does not require visible supersession audit detail:\n%s", prompt)
	}
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	now := time.Now()
	h.now = func() time.Time { return now }

	for i := range 3 {
		h.mu.Lock()
		ok := h.allowSpawnLocked()
		if ok {
			h.recent = append(h.recent, h.now())
		}
		h.mu.Unlock()
		if !ok {
			t.Fatalf("spawn %d should be allowed", i)
		}
	}
	h.mu.Lock()
	overLimit := h.allowSpawnLocked()
	h.mu.Unlock()
	if overLimit {
		t.Errorf("4th spawn should be rate-limited")
	}

	// Advance past the window: old entries expire and a slot frees up.
	h.now = func() time.Time { return now.Add(humanReviewWindow + time.Minute) }
	h.mu.Lock()
	allowed := h.allowSpawnLocked()
	h.mu.Unlock()
	if !allowed {
		t.Errorf("after window, spawn should be allowed again")
	}
}

// TestPerTaskRateLimiter is a regression guard for sybra#1487: a single task
// whose status keeps oscillating into human-required must not be allowed to
// consume the entire global review budget (cfg.HumanReview.MaxPerHour=3 in
// this test env) and starve every other task's diagnosis.
func TestPerTaskRateLimiter(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	now := time.Now()
	h.now = func() time.Time { return now }

	for i := range humanReviewMaxPerTaskPerWindow {
		h.mu.Lock()
		ok := h.allowSpawnForTaskLocked("flapping-task")
		if ok {
			h.perTask["flapping-task"] = append(h.perTask["flapping-task"], h.now())
		}
		h.mu.Unlock()
		if !ok {
			t.Fatalf("spawn %d for the same task should be allowed", i)
		}
	}
	h.mu.Lock()
	overLimit := h.allowSpawnForTaskLocked("flapping-task")
	h.mu.Unlock()
	if overLimit {
		t.Errorf("task should be rate-limited after %d spawns within the window", humanReviewMaxPerTaskPerWindow)
	}

	// A different task must be unaffected by the flapping task's budget.
	h.mu.Lock()
	otherAllowed := h.allowSpawnForTaskLocked("other-task")
	h.mu.Unlock()
	if !otherAllowed {
		t.Errorf("an unrelated task should not be rate-limited by another task's spawns")
	}

	// Advance past the window: the flapping task's own budget frees up.
	h.now = func() time.Time { return now.Add(humanReviewWindow + time.Minute) }
	h.mu.Lock()
	allowedAfterWindow := h.allowSpawnForTaskLocked("flapping-task")
	h.mu.Unlock()
	if !allowedAfterWindow {
		t.Errorf("after window, the flapping task should be allowed again")
	}
}

func TestOnComplete_HumanVerdict_AppendsNote(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Refactor billing", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Verdict below.\n\n```sybra-verdict\n" +
			`{"decision":"human","summary":"needs product input on scope"}` +
			"\n```\n",
	})
	h.inflight[tk.ID] = "agent-1"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: want empty, got %q", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: needs human") {
		t.Errorf("expected verdict header in body; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "needs product input on scope") {
		t.Errorf("expected summary in body; got:\n%s", got.Body)
	}
	if _, busy := h.inflight[tk.ID]; busy {
		t.Errorf("inflight should be cleared after onComplete")
	}
}

func TestOnComplete_StaleHumanVerdictMarksRendered(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Prompt Lab approval", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusInProgress)}); err != nil {
		t.Fatalf("advance task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "hr-stale",
		Role:    string(agent.RoleHumanReview),
		State:   string(agent.StateStopped),
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: "hr-stale", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"human","summary":"needs approval"}`,
	})
	h.inflight[tk.ID] = ag.ID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Errorf("status: got %q want in-progress", got.Status)
	}
	if strings.Contains(got.Body, "Auto-review verdict: needs human") {
		t.Errorf("stale human verdict should not append a needs-human note to an advanced task; body:\n%s", got.Body)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("human-review run was not marked rendered: %+v", got.AgentRuns)
	}
	if !verdictAlreadyRendered(got) {
		t.Fatal("verdictAlreadyRendered should accept the stale run's rendered marker")
	}
}

func TestOnComplete_UnblockedVerdict_NotesAndDoesNotBlock(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Fix flaky test", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusReadyPR)}); err != nil {
		t.Fatalf("advance to ready-pr: %v", err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "```sybra-verdict\n" +
			`{"decision":"unblocked","reason":"fixed lint, pushed, advanced to ready-pr","recoverable_action":"none","confidence":"high"}` +
			"\n```\n",
	})
	h.inflight[tk.ID] = "agent-1"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusReadyPR {
		t.Errorf("status: got %q want ready-pr (unblocked must not re-block or revert the agent's advance)", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: want empty, got %q", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked") {
		t.Errorf("expected unblocked note in body; got:\n%s", got.Body)
	}
	if _, busy := h.inflight[tk.ID]; busy {
		t.Errorf("inflight should be cleared after onComplete")
	}
}

// TestOnComplete_UnblockedVerdict_TerminalResultWinsOverFuzzyAssistantProse
// reproduces the reported bug: an earlier assistant turn second-guesses
// itself in prose that happens to contain the substring "decision" (but does
// not parse as a verdict), followed by a tool_use turn (rendered as
// "[ToolName]" with no JSON body — see formatHeadlessAssistant) that actually
// invoked the structured-output tool. The real, schema-conformant payload
// only shows up in the terminal result event. The fuzzy substring tier must
// not shadow it.
func TestOnComplete_UnblockedVerdict_TerminalResultWinsOverFuzzyAssistantProse(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Fix flaky test", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusReadyPR)}); err != nil {
		t.Fatalf("advance to ready-pr: %v", err)
	}

	ag := &agent.Agent{ID: "agent-1", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: "Wait, let me double check whether I already reported the decision before calling the tool.",
	})
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: "[StructuredOutput]",
	})
	ag.AppendOutput(agent.StreamEvent{
		Type:    "result",
		Content: `{"decision":"unblocked","reason":"fixed lint, pushed, advanced to ready-pr","recoverable_action":"none","confidence":"high"}`,
	})
	h.inflight[tk.ID] = "agent-1"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusReadyPR {
		t.Errorf("status: got %q want ready-pr (unblocked must not re-block or revert the agent's advance)", got.Status)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked") {
		t.Errorf("expected unblocked note in body; got:\n%s", got.Body)
	}
}

func TestOnComplete_UnblockedVerdict_AppliesRecoverableAction(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-action"
	tk, err := tasks.Create("Recover task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}
	var dispatchedTarget, dispatchedReason string
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		dispatchedTarget = target
		dispatchedReason = reason
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusReadyReview),
			StatusReason: task.Ptr(reason),
		})
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"fixed the issue and the host should resume review","recoverable_action":"ready-review","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if dispatchedTarget != string(task.StatusReadyReview) {
		t.Fatalf("dispatch target = %q, want %q", dispatchedTarget, task.StatusReadyReview)
	}
	if got.Status != task.StatusReadyReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusReadyReview)
	}
	if !strings.Contains(dispatchedReason, "auto-review recovery") || !strings.Contains(got.StatusReason, "auto-review recovery") {
		t.Fatalf("status_reason = %q, want auto-review recovery note", got.StatusReason)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked") {
		t.Fatalf("expected unblocked note in body; got:\n%s", got.Body)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestPrepareRecoveryDispatch_InReviewWithoutPRFallsBackToReadyReview(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Recover pre-PR review", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	got, err := h.prepareRecoveryDispatch(tk, task.StatusInReview)
	if err != nil {
		t.Fatalf("prepareRecoveryDispatch: %v", err)
	}
	if got != task.StatusReadyReview {
		t.Fatalf("target = %q, want %q", got, task.StatusReadyReview)
	}
}

func completedHumanReviewWorkflow() **workflow.Execution {
	wf := &workflow.Execution{WorkflowID: "simple-task-implement", State: workflow.ExecCompleted}
	return &wf
}

func TestRecoverStrandedUnblockedTasks_ReplaysLegacyReasonWithoutConfiguredWorktree(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Recover old missing worktree park", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	reason := `circuit breaker: agent start failed: start agent: agent.Run: Dir "/tmp/sybra/worktrees/task-1" not accessible: stat /tmp/sybra/worktrees/task-1: no such file or directory (tripped after 3 dispatch failures for step "fix_review" within 15m0s)`
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Workflow:     completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         "hr-rendered",
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		State:           "stopped",
		Outcome:         "success",
		VerdictRendered: true,
		Result:          `{"decision":"unblocked","reason":"worktree has been recreated; resume review","recoverable_action":"in-review","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	var dispatchedTarget string
	h.dispatchFromHumanRequired = func(id, target, dispatchReason, _ string) (task.Task, error) {
		dispatchedTarget = target
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusReadyReview),
			StatusReason: task.Ptr(dispatchReason),
		})
	}

	h.recoverStrandedUnblockedTasks()

	if dispatchedTarget != string(task.StatusReadyReview) {
		t.Fatalf("dispatch target = %q, want %q", dispatchedTarget, task.StatusReadyReview)
	}
	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusReadyReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusReadyReview)
	}
}

func TestRecoverStrandedUnblockedTasks_DoneActionLandsMergedPR(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "hr-rendered-done"
	tk, err := tasks.Create("Recover rendered merged PR", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	reason := `circuit breaker: agent start failed: start agent: agent.Run: Dir "/tmp/sybra/worktrees/task-1" not accessible: stat /tmp/sybra/worktrees/task-1: no such file or directory`
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Workflow:     completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         agentID,
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		State:           "stopped",
		Outcome:         "success",
		VerdictRendered: true,
		Result:          `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("unexpected terminal dispatch for merged rendered recovery")
		return task.Task{}, nil
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "MERGED"}, nil
	}
	h.landClosedPR = func(_ context.Context, id string, prNumber int, state, completingAgentID string) error {
		if id != tk.ID {
			t.Fatalf("taskID = %q, want %q", id, tk.ID)
		}
		if prNumber != 2417 {
			t.Fatalf("prNumber = %d, want 2417", prNumber)
		}
		if state != "MERGED" {
			t.Fatalf("state = %q, want MERGED", state)
		}
		if completingAgentID != agentID {
			t.Fatalf("completingAgentID = %q, want %q", completingAgentID, agentID)
		}
		_, err := tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusDone),
			Outcome:      task.Ptr("merged"),
			StatusReason: task.Ptr(""),
		})
		return err
	}

	h.recoverStrandedUnblockedTasks()

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if got.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", got.Outcome)
	}
	if got.PRNumber != 2417 {
		t.Fatalf("prNumber = %d, want 2417", got.PRNumber)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared merged landing", got.StatusReason)
	}
}

func TestRecoverStrandedUnblockedTasks_DoneActionRejectsUnmergedPR(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "hr-rendered-done-closed"
	tk, err := tasks.Create("Recover rendered closed PR", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	reason := `circuit breaker: agent start failed: start agent: agent.Run: Dir "/tmp/sybra/worktrees/task-1" not accessible: stat /tmp/sybra/worktrees/task-1: no such file or directory`
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Workflow:     completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         agentID,
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		State:           "stopped",
		Outcome:         "success",
		VerdictRendered: true,
		Result:          `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("unexpected terminal dispatch for unmerged rendered recovery")
		return task.Task{}, nil
	}
	h.landClosedPR = func(context.Context, string, int, string, string) error {
		t.Fatal("unexpected merged landing for unmerged rendered recovery")
		return nil
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "CLOSED"}, nil
	}

	h.recoverStrandedUnblockedTasks()

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.Outcome != "" {
		t.Fatalf("outcome = %q, want empty", got.Outcome)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked claim not verified") {
		t.Fatalf("expected verification failure note in body; got:\n%s", got.Body)
	}
}

func TestRecoverStrandedUnblockedTasks_ReplaysUnrenderedVerdict(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Recover old missing worktree park", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	reason := `circuit breaker: agent start failed: start agent: agent.Run: Dir "/tmp/sybra/worktrees/task-1" not accessible: stat /tmp/sybra/worktrees/task-1: no such file or directory`
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Workflow:     completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         "hr-unrendered",
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		State:           "stopped",
		Outcome:         "success",
		VerdictRendered: false,
		Result:          `{"decision":"unblocked","reason":"worktree has been recreated; resume review","recoverable_action":"in-review","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(id, target, dispatchReason, agentID string) (task.Task, error) {
		if target != string(task.StatusReadyReview) {
			t.Fatalf("target = %q, want %q", target, task.StatusReadyReview)
		}
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusReadyReview),
			StatusReason: task.Ptr(dispatchReason),
		})
	}

	h.recoverStrandedUnblockedTasks()

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusReadyReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusReadyReview)
	}
	if !got.AgentRuns[0].VerdictRendered {
		t.Fatal("successful replay must latch verdict_rendered")
	}
}

func TestRecoverStrandedUnblockedTasks_DoesNotRequireLegacyReason(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Recover verification park", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("verification could not persist while task storage was unavailable"),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Workflow:     completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "hr-storage", Role: string(agent.RoleHumanReview), State: string(agent.StateStopped),
		Outcome: "success",
		Result:  `{"decision":"unblocked","reason":"storage recovered; resume implementation","recoverable_action":"in-progress","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(id, target, dispatchReason, _ string) (task.Task, error) {
		if target != string(task.StatusInProgress) {
			t.Fatalf("target = %q, want %q", target, task.StatusInProgress)
		}
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusInProgress),
			StatusReason: task.Ptr(dispatchReason),
		})
	}

	h.recoverStrandedUnblockedTasks()

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
}

func TestRecoverStrandedUnblockedTasks_LatestVerdictWins(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Do not replay stale verdict", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
		Workflow:  completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	for _, run := range []task.AgentRun{
		{
			AgentID: "hr-old", Role: string(agent.RoleHumanReview), State: string(agent.StateStopped),
			VerdictRendered: true,
			Result:          `{"decision":"unblocked","reason":"old evidence","recoverable_action":"in-progress","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
		},
		{
			AgentID: "hr-new", Role: string(agent.RoleHumanReview), State: string(agent.StateStopped),
			VerdictRendered: true,
			Result:          `{"decision":"human","reason":"new evidence requires an operator","recoverable_action":"none","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
		},
	} {
		if err := tasks.AddRun(tk.ID, run); err != nil {
			t.Fatalf("add run %s: %v", run.AgentID, err)
		}
	}
	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("an older unblocked verdict must not supersede the latest human verdict")
		return task.Task{}, nil
	}

	h.recoverStrandedUnblockedTasks()

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
}

func TestLatestHumanReviewUnblockedVerdict_RequiresSuccessfulOutcome(t *testing.T) {
	t.Parallel()
	result := `{"decision":"unblocked","reason":"partial output","recoverable_action":"in-progress","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`
	for _, tc := range []struct {
		name string
		run  task.AgentRun
		want bool
	}{
		{name: "success", run: task.AgentRun{Outcome: "success"}, want: true},
		{name: "failed", run: task.AgentRun{Outcome: "failed"}},
		{name: "unfinished legacy", run: task.AgentRun{}},
		{name: "rendered legacy success", run: task.AgentRun{VerdictRendered: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.run
			run.AgentID = "hr"
			run.Role = string(agent.RoleHumanReview)
			run.State = string(agent.StateStopped)
			run.Result = result
			_, _, got := latestHumanReviewUnblockedVerdict(task.Task{AgentRuns: []task.AgentRun{run}})
			if got != tc.want {
				t.Fatalf("latest verdict accepted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStrandedHumanReviewRecoveryCandidate_Guards(t *testing.T) {
	t.Parallel()
	completed := &workflow.Execution{State: workflow.ExecCompleted}
	running := &workflow.Execution{State: workflow.ExecRunning}
	tests := []struct {
		name string
		task task.Task
		want bool
	}{
		{name: "completed", task: task.Task{Status: task.StatusHumanRequired, Workflow: completed}, want: true},
		{name: "failed", task: task.Task{Status: task.StatusHumanRequired, Workflow: &workflow.Execution{State: workflow.ExecFailed}}, want: true},
		{name: "umbrella", task: task.Task{Status: task.StatusHumanRequired, TaskType: task.TaskTypeUmbrella, Workflow: completed}},
		{name: "active workflow", task: task.Task{Status: task.StatusHumanRequired, Workflow: running}},
		{name: "missing workflow", task: task.Task{Status: task.StatusHumanRequired}},
		{name: "not parked", task: task.Task{Status: task.StatusInProgress, Workflow: completed}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := strandedHumanReviewRecoveryCandidate(tc.task); got != tc.want {
				t.Fatalf("candidate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecoverStrandedUnblockedTasks_DirtyWorktreeStaysParked(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	dir := setupUnblockedRecoveryWorktree(t, "fix/stranded-dirty")
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("not committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk, err := tasks.Create("Keep dirty recovery parked", "body", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	tk, err = tasks.Update(tk.ID, task.Update{
		Status:      task.Ptr(task.StatusHumanRequired),
		ProjectID:   task.Ptr("Automaat/sybra"),
		WorktreeDir: task.Ptr(dir),
		Workflow:    completedHumanReviewWorkflow(),
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "hr-dirty", Role: string(agent.RoleHumanReview), State: string(agent.StateStopped),
		Outcome: "success",
		Result:  `{"decision":"unblocked","reason":"claimed pushed fix","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}
	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("dirty worktree must fail verification before dispatch")
		return task.Task{}, nil
	}

	h.recoverStrandedUnblockedTasks()
	first, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get first rejection: %v", err)
	}
	reopenedStore, err := task.NewStore(filepath.Dir(first.FilePath))
	if err != nil {
		t.Fatalf("reopen task store: %v", err)
	}
	tasks = task.NewManager(reopenedStore, nil)
	h.tasks = tasks
	h.recoverStrandedUnblockedTasks()

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if count := strings.Count(got.Body, "## Auto-review: unblocked claim not verified"); count != 1 {
		t.Fatalf("verification note count = %d, want 1 after repeated replay", count)
	}
	if !got.AgentRuns[0].RecoveryReplayRejected {
		t.Fatal("rejected replay must be latched on the exact agent run")
	}

	if err := os.Remove(filepath.Join(dir, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "hr-dirty-repaired", Role: string(agent.RoleHumanReview), State: string(agent.StateStopped),
		Outcome: "success",
		Result:  `{"decision":"unblocked","reason":"claimed pushed fix","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	}); err != nil {
		t.Fatalf("add repaired run: %v", err)
	}
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		return tasks.Update(id, task.Update{Status: task.Ptr(task.StatusReadyPR), StatusReason: task.Ptr(reason)})
	}
	h.recoverStrandedUnblockedTasks()

	got, err = tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get repaired task: %v", err)
	}
	if got.Status != task.StatusReadyPR {
		t.Fatalf("new same-summary verdict status = %q, want %q", got.Status, task.StatusReadyPR)
	}
}

func TestVerifyUnblocked_ConfiguredInvalidWorktreeStaysParked(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "missing", dir: filepath.Join(t.TempDir(), "missing")},
		{name: "not-directory", dir: file},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if h.verifyUnblocked(task.Task{ID: "task-invalid-worktree", WorktreeDir: tc.dir}) {
				t.Fatalf("verifyUnblocked(%q) = true, want false", tc.dir)
			}
		})
	}
}

func TestOnComplete_UnblockedVerdict_ReadyReviewWithPRResumesInReview(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-pr-review"
	tk, err := tasks.Create("Recover PR task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
		PRNumber:  task.Ptr(42),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	var dispatchedTarget string
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		dispatchedTarget = target
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.Status(target)),
			StatusReason: task.Ptr(reason),
		})
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"fixed the issue and the host should resume review","recoverable_action":"ready-review","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if dispatchedTarget != string(task.StatusInReview) {
		t.Fatalf("dispatch target = %q, want %q for linked PR recovery", dispatchedTarget, task.StatusInReview)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInReview)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_DispatchNoteFailureKeepsVerdictUnrendered(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-note-failure"
	tk, err := tasks.Create("Recover task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	taskDir := filepath.Dir(tk.FilePath)
	info, err := os.Stat(taskDir)
	if err != nil {
		t.Fatalf("stat task dir: %v", err)
	}
	restoreMode := info.Mode().Perm()
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		updated, uErr := tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusReadyReview),
			StatusReason: task.Ptr(reason),
		})
		if uErr != nil {
			return task.Task{}, uErr
		}
		if err := os.Chmod(taskDir, 0o555); err != nil {
			t.Fatalf("chmod task dir read-only: %v", err)
		}
		return updated, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"fixed the issue and the host should resume review","recoverable_action":"ready-review","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	if err := os.Chmod(taskDir, restoreMode); err != nil {
		t.Fatalf("restore task dir mode: %v", err)
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusReadyReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusReadyReview)
	}
	if strings.Contains(got.Body, "Auto-review: unblocked") {
		t.Fatalf("unexpected unblocked note after append failure; got:\n%s", got.Body)
	}
	if len(got.AgentRuns) != 1 {
		t.Fatalf("agent runs = %+v, want single run", got.AgentRuns)
	}
	if got.AgentRuns[0].VerdictRendered {
		t.Fatalf("expected verdict to remain unrendered after note write failure; agent runs = %+v", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_ReadyPRWithoutPRStaysReadyPR(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-ready-pr"
	tk, err := tasks.Create("Recover PR fallback task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	var dispatchedTarget string
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		dispatchedTarget = target
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.Status(target)),
			StatusReason: task.Ptr(reason),
		})
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"the final gate cannot run locally; use the PR fallback","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if dispatchedTarget != string(task.StatusReadyPR) {
		t.Fatalf("dispatch target = %q, want %q for no-PR fallback recovery", dispatchedTarget, task.StatusReadyPR)
	}
	if got.Status != task.StatusReadyPR {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusReadyPR)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_DoneActionLandsMergedTask(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-done"
	dir := setupUnblockedRecoveryWorktree(t, "feat/merged")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("dirty local change"), 0o644); err != nil {
		t.Fatalf("dirty file: %v", err)
	}
	tk, err := tasks.Create("Already merged task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("github push preflight failed: auth"),
		ProjectID:    task.Ptr("Automaat/sybra"),
		WorktreeDir:  task.Ptr(dir),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("unexpected terminal dispatch for merged landing")
		return task.Task{}, nil
	}
	h.landClosedPR = func(_ context.Context, id string, prNumber int, state, completingAgentID string) error {
		if id != tk.ID {
			t.Fatalf("taskID = %q, want %q", id, tk.ID)
		}
		if prNumber != 2417 {
			t.Fatalf("prNumber = %d, want 2417", prNumber)
		}
		if state != "MERGED" {
			t.Fatalf("state = %q, want MERGED", state)
		}
		if completingAgentID != agentID {
			t.Fatalf("completingAgentID = %q, want %q", completingAgentID, agentID)
		}
		_, err := tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusDone),
			Outcome:      task.Ptr("merged"),
			StatusReason: task.Ptr(""),
		})
		return err
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "MERGED"}, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if got.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", got.Outcome)
	}
	if got.PRNumber != 2417 {
		t.Fatalf("prNumber = %d, want 2417", got.PRNumber)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared merged landing", got.StatusReason)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked") {
		t.Fatalf("expected unblocked note in body; got:\n%s", got.Body)
	}
	if strings.Contains(got.Body, "claim not verified") {
		t.Fatalf("unexpected verification failure note in body:\n%s", got.Body)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_DoneActionPrefersVerdictPR(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-done-replacement-pr"
	tk, err := tasks.Create("Merged replacement PR task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("waiting on review"),
		ProjectID:    task.Ptr("Automaat/sybra"),
		PRNumber:     task.Ptr(100),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("unexpected terminal dispatch for merged landing")
		return task.Task{}, nil
	}
	h.landClosedPR = func(_ context.Context, id string, prNumber int, state, completingAgentID string) error {
		if id != tk.ID {
			t.Fatalf("taskID = %q, want %q", id, tk.ID)
		}
		if prNumber != 101 {
			t.Fatalf("prNumber = %d, want replacement PR 101", prNumber)
		}
		if state != "MERGED" {
			t.Fatalf("state = %q, want MERGED", state)
		}
		if completingAgentID != agentID {
			t.Fatalf("completingAgentID = %q, want %q", completingAgentID, agentID)
		}
		_, err := tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusDone),
			Outcome:      task.Ptr("merged"),
			StatusReason: task.Ptr(""),
		})
		return err
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 101 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#101", repo, number)
		}
		return github.PRState{State: "MERGED"}, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"merged through PR #101","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if got.PRNumber != 101 {
		t.Fatalf("prNumber = %d, want replacement PR 101", got.PRNumber)
	}
	if got.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", got.Outcome)
	}
	if strings.Contains(got.Body, "claim not verified") {
		t.Fatalf("unexpected verification failure note in body:\n%s", got.Body)
	}
}

func TestOnComplete_UnblockedVerdict_DoneActionPreservesLandingOutcome(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-done-edited"
	tk, err := tasks.Create("Merged task with human edits", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("waiting on review"),
		ProjectID:    task.Ptr("Automaat/sybra"),
		PRNumber:     task.Ptr(2417),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("unexpected terminal dispatch for merged landing")
		return task.Task{}, nil
	}
	h.landClosedPR = func(_ context.Context, id string, prNumber int, state, completingAgentID string) error {
		if id != tk.ID {
			t.Fatalf("taskID = %q, want %q", id, tk.ID)
		}
		if prNumber != 2417 {
			t.Fatalf("prNumber = %d, want 2417", prNumber)
		}
		if state != "MERGED" {
			t.Fatalf("state = %q, want MERGED", state)
		}
		if completingAgentID != agentID {
			t.Fatalf("completingAgentID = %q, want %q", completingAgentID, agentID)
		}
		_, err := tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusDone),
			Outcome:      task.Ptr("merged_with_edits"),
			StatusReason: task.Ptr(""),
		})
		return err
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "MERGED"}, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if got.Outcome != "merged_with_edits" {
		t.Fatalf("outcome = %q, want merged_with_edits", got.Outcome)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared merged landing", got.StatusReason)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_DoneActionRejectsUnmergedPR(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-done-closed-pr"
	tk, err := tasks.Create("Closed PR task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("waiting on review"),
		ProjectID:    task.Ptr("Automaat/sybra"),
		PRNumber:     task.Ptr(2417),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	h.landClosedPR = func(context.Context, string, int, string, string) error {
		t.Fatal("unexpected merged landing for closed PR")
		return nil
	}
	h.dispatchFromHumanRequired = func(string, string, string, string) (task.Task, error) {
		t.Fatal("unexpected terminal dispatch for closed PR")
		return task.Task{}, nil
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "CLOSED"}, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
	if got.Outcome != "" {
		t.Fatalf("outcome = %q, want empty", got.Outcome)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked claim not verified") {
		t.Fatalf("expected verification failure note in body; got:\n%s", got.Body)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_DoneActionFallbackBackfillsPR(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-done-fallback"
	tk, err := tasks.Create("Merged task missing PR link", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("github push preflight failed: auth"),
		ProjectID:    task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	var dispatchedTarget string
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		dispatchedTarget = target
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.Status(target)),
			StatusReason: task.Ptr(reason),
		})
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "MERGED"}, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if dispatchedTarget != string(task.StatusDone) {
		t.Fatalf("dispatch target = %q, want %q", dispatchedTarget, task.StatusDone)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if got.PRNumber != 2417 {
		t.Fatalf("prNumber = %d, want 2417", got.PRNumber)
	}
	if got.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", got.Outcome)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared after terminal recovery", got.StatusReason)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked") {
		t.Fatalf("expected unblocked note in body; got:\n%s", got.Body)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_DoneActionFallsBackWhenLandingFails(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-done-land-error"
	tk, err := tasks.Create("Merged task with landing error", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("waiting on review"),
		ProjectID:    task.Ptr("Automaat/sybra"),
		PRNumber:     task.Ptr(2417),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	var dispatchedTarget string
	h.dispatchFromHumanRequired = func(id, target, reason, completingAgentID string) (task.Task, error) {
		if id != tk.ID {
			t.Fatalf("taskID = %q, want %q", id, tk.ID)
		}
		if completingAgentID != agentID {
			t.Fatalf("completingAgentID = %q, want %q", completingAgentID, agentID)
		}
		dispatchedTarget = target
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.Status(target)),
			StatusReason: task.Ptr(reason),
		})
	}
	h.landClosedPR = func(_ context.Context, id string, prNumber int, state, completingAgentID string) error {
		if id != tk.ID {
			t.Fatalf("taskID = %q, want %q", id, tk.ID)
		}
		if prNumber != 2417 {
			t.Fatalf("prNumber = %d, want 2417", prNumber)
		}
		if state != "MERGED" {
			t.Fatalf("state = %q, want MERGED", state)
		}
		if completingAgentID != agentID {
			t.Fatalf("completingAgentID = %q, want %q", completingAgentID, agentID)
		}
		return errors.New("boom")
	}
	h.fetchPRState = func(repo string, number int) (github.PRState, error) {
		if repo != "Automaat/sybra" || number != 2417 {
			t.Fatalf("fetchPRState(%q, %d), want Automaat/sybra#2417", repo, number)
		}
		return github.PRState{State: "MERGED"}, nil
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"merged through PR #2417","recoverable_action":"done","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if dispatchedTarget != string(task.StatusDone) {
		t.Fatalf("dispatch target = %q, want %q", dispatchedTarget, task.StatusDone)
	}
	if got.Status != task.StatusDone {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusDone)
	}
	if got.PRNumber != 2417 {
		t.Fatalf("prNumber = %d, want 2417", got.PRNumber)
	}
	if got.Outcome != "merged" {
		t.Fatalf("outcome = %q, want merged", got.Outcome)
	}
	if got.StatusReason != "" {
		t.Fatalf("status_reason = %q, want cleared after fallback", got.StatusReason)
	}
	if len(got.AgentRuns) != 1 || !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("agent runs = %+v, want rendered verdict", got.AgentRuns)
	}
}

func TestOnComplete_UnblockedVerdict_TamperRerouteAddsBlessTag(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-tamper"
	tk, err := tasks.Create("Recover tamper task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(workflow.TamperFlaggedReasonPrefix + " internal/foo_test.go: added-skip"),
		Blocker:      task.Ptr(blocker.State{Kind: blocker.KindTamperDetected, Actor: blocker.ActorWorkflow}),
		ProjectID:    task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		cur, gErr := tasks.Get(id)
		if gErr != nil {
			return task.Task{}, gErr
		}
		if !slices.Contains(cur.Tags, workflow.TamperBlessedTag) {
			t.Fatalf("expected %q tag before dispatch, tags=%v", workflow.TamperBlessedTag, cur.Tags)
		}
		if target != string(task.StatusInProgress) {
			t.Fatalf("dispatch target = %q, want %q", target, task.StatusInProgress)
		}
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusInProgress),
			StatusReason: task.Ptr(reason),
		})
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"resume verification after the false-positive tamper gate","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if !slices.Contains(got.Tags, workflow.TamperBlessedTag) {
		t.Fatalf("tags = %v, want %q", got.Tags, workflow.TamperBlessedTag)
	}
	if got.Status != task.StatusInProgress {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInProgress)
	}
}

func TestOnComplete_UnblockedVerdict_TamperReadyReviewAddsBlessTag(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-tamper-ready-review"
	tk, err := tasks.Create("Recover tamper review task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(workflow.TamperFlaggedReasonPrefix + " internal/foo_test.go: added-skip"),
		Blocker:      task.Ptr(blocker.State{Kind: blocker.KindTamperDetected, Actor: blocker.ActorWorkflow}),
		ProjectID:    task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}
	h.dispatchFromHumanRequired = func(id, target, reason, _ string) (task.Task, error) {
		cur, gErr := tasks.Get(id)
		if gErr != nil {
			return task.Task{}, gErr
		}
		if !slices.Contains(cur.Tags, workflow.TamperBlessedTag) {
			t.Fatalf("expected %q tag before dispatch, tags=%v", workflow.TamperBlessedTag, cur.Tags)
		}
		if target != string(task.StatusReadyReview) {
			t.Fatalf("dispatch target = %q, want %q", target, task.StatusReadyReview)
		}
		return tasks.Update(id, task.Update{
			Status:       task.Ptr(task.StatusReadyReview),
			StatusReason: task.Ptr(reason),
		})
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"resume review after accepting the tamper finding","recoverable_action":"ready-review","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if !slices.Contains(got.Tags, workflow.TamperBlessedTag) {
		t.Fatalf("tags = %v, want %q", got.Tags, workflow.TamperBlessedTag)
	}
	if got.Status != task.StatusReadyReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusReadyReview)
	}
}

// TestOnComplete_UnblockedVerdict_CleanPushedBranchTransitions is the happy
// path for verifyUnblocked against a real worktree: a clean, fully pushed
// branch is exactly the state a genuinely self-unblocked task should be in,
// and the status transition must still go through.
func TestOnComplete_UnblockedVerdict_CleanPushedBranchTransitions(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-clean"
	dir := setupUnblockedRecoveryWorktree(t, "fix/clean")
	tk, err := tasks.Create("Recover task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:      task.Ptr(task.StatusHumanRequired),
		ProjectID:   task.Ptr("Automaat/sybra"),
		WorktreeDir: task.Ptr(dir),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID, Role: string(agent.RoleHumanReview), Mode: "headless", State: "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"pushed the fix","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusReadyPR {
		t.Fatalf("status = %q, want %q (clean, pushed branch must be trusted)", got.Status, task.StatusReadyPR)
	}
}

// TestOnComplete_UnblockedVerdict_DirtyWorktreeDoesNotTransition is the
// regression for #2347: an "unblocked" claim against a worktree that still
// has uncommitted changes must not be trusted — the task stays parked in
// human-required with only a note, instead of silently advancing on an
// unverified claim.
func TestOnComplete_UnblockedVerdict_DirtyWorktreeDoesNotTransition(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-dirty"
	dir := setupUnblockedRecoveryWorktree(t, "fix/dirty")
	if err := os.WriteFile(filepath.Join(dir, "uncommitted.txt"), []byte("still editing"), 0o644); err != nil {
		t.Fatal(err)
	}

	tk, err := tasks.Create("Recover task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:      task.Ptr(task.StatusHumanRequired),
		ProjectID:   task.Ptr("Automaat/sybra"),
		WorktreeDir: task.Ptr(dir),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID, Role: string(agent.RoleHumanReview), Mode: "headless", State: "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"pushed the fix","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want unchanged %q (dirty worktree must not be trusted)", got.Status, task.StatusHumanRequired)
	}
	if strings.Contains(got.Body, "Auto-review: unblocked\n") {
		t.Fatalf("body must not claim success when verification failed; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked claim not verified") {
		t.Fatalf("expected verification-failure note in body; got:\n%s", got.Body)
	}
}

// TestOnComplete_UnblockedVerdict_UnpushedBranchDoesNotTransition is the
// other half of #2347's regression: a clean worktree whose commits never
// actually reached the remote (the exact "claimed unblocked but PR still
// dirty" symptom from the incident) must also not be trusted.
func TestOnComplete_UnblockedVerdict_UnpushedBranchDoesNotTransition(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-unblocked-unpushed"
	dir := setupUnblockedRecoveryWorktree(t, "fix/unpushed")
	runGit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "unpushed.txt"), []byte("local only"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "unpushed local commit")

	tk, err := tasks.Create("Recover task", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:      task.Ptr(task.StatusHumanRequired),
		ProjectID:   task.Ptr("Automaat/sybra"),
		WorktreeDir: task.Ptr(dir),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID, Role: string(agent.RoleHumanReview), Mode: "headless", State: "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"unblocked","reason":"pushed the fix","recoverable_action":"ready-pr","confidence":"high","issue_title":null,"issue_body":null,"issue_labels":null}`,
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want unchanged %q (unpushed commit must not be trusted)", got.Status, task.StatusHumanRequired)
	}
	if strings.Contains(got.Body, "Auto-review: unblocked\n") {
		t.Fatalf("body must not claim success when verification failed; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "Auto-review: unblocked claim not verified") {
		t.Fatalf("expected verification-failure note in body; got:\n%s", got.Body)
	}
}

func TestBuildPrompt_IncludesUnblockAndAutonomyMandate(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk := task.Task{
		ID: "t1", Title: "x", Status: task.StatusHumanRequired,
		Branch: "feat/queue", WorktreeDir: "/data/worktrees/queue",
	}
	dir, _ := humanReviewDispatchDir(tk, h.cfg.HumanReview.SybraRepoDir)
	p := h.buildPrompt(context.Background(), tk, dir, nil, false)
	for _, want := range []string{
		"UNBLOCK", "AUTONOMY", "ROOT CAUSE", "unblocked",
		"never fabricate", "/data/worktrees/queue", "feat/queue",
		"do NOT jump straight to `ready-pr`",
		"Prefer the first safe downstream stage (`in-progress`/`testing`/`in-review`) over `ready-pr`",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestOnComplete_BareJSONVerdict_HumanDecision drives onComplete with a bare
// structured-output JSON assistant turn (verdict.SourceJSON) instead of the
// legacy fenced ```sybra-verdict``` block — the production path introduced
// by --json-schema enforcement, previously untested end-to-end.
func TestOnComplete_BareJSONVerdict_HumanDecision(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Refactor billing", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"human","summary":"needs product input on scope"}`,
	})
	h.inflight[tk.ID] = "agent-3"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: needs human") {
		t.Errorf("expected verdict header in body; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "needs product input on scope") {
		t.Errorf("expected summary in body; got:\n%s", got.Body)
	}
}

// TestOnComplete_BareJSONVerdict_SybraBug is the sybra_bug counterpart of
// TestOnComplete_BareJSONVerdict_HumanDecision — bare JSON assistant output,
// no fence, driving the default non-filing note side effect.
func TestOnComplete_BareJSONVerdict_SybraBug(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Workflow misfire", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: `{
  "decision": "sybra_bug",
  "summary": "verify_commits never re-checked the branch",
  "issue_title": "fix(workflow): verify_commits race",
  "issue_body": "## What\nrace condition",
  "issue_labels": ["workflow", "regression"]
}`,
	})
	h.inflight[tk.ID] = "agent-4"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: got %q want empty", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: Sybra bug (note only)") ||
		!strings.Contains(got.Body, "fix(workflow): verify_commits race") {
		t.Errorf("expected non-filing note in body; got:\n%s", got.Body)
	}
}

func TestOnComplete_SybraBugVerdict_DefaultNotesWithoutFiling(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Workflow misfire", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "verify_commits never re-checked the branch",
  "issue_title": "fix(workflow): verify_commits race",
  "issue_body": "## What\nrace condition",
  "issue_labels": ["workflow", "regression"]
}` + "\n```\n",
	})
	h.inflight[tk.ID] = "agent-2"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: got %q want empty", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: Sybra bug (note only)") ||
		!strings.Contains(got.Body, "race condition") {
		t.Errorf("expected non-filing diagnosis note; got:\n%s", got.Body)
	}
}

func TestOnComplete_SybraBugVerdict_NoteOnly(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()
	h.cfg.HumanReview.SybraBugAction = config.HumanReviewSybraBugActionNoteOnly

	tk, err := tasks.Create("Workflow misfire", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "verify_commits never re-checked the branch",
  "issue_title": "fix(workflow): verify_commits race",
  "issue_body": "## What\nrace condition",
  "issue_labels": ["workflow", "regression"]
}` + "\n```\n",
	})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: got %q want empty", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "Auto-review verdict: Sybra bug (note only)") ||
		!strings.Contains(got.Body, "fix(workflow): verify_commits race") {
		t.Errorf("expected note-only diagnosis in body; got:\n%s", got.Body)
	}
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("note_only should not create tasks; got %d tasks", len(all))
	}
}

func TestOnComplete_SybraBugVerdict_BlockOnly(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()
	h.cfg.HumanReview.SybraBugAction = config.HumanReviewSybraBugActionBlockOnly

	tk, err := tasks.Create("Workflow misfire", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "verify_commits never re-checked the branch",
  "issue_title": "fix(workflow): verify_commits race",
  "issue_body": "## What\nrace condition"
}` + "\n```\n",
	})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusBlocked {
		t.Errorf("status: got %q want blocked", got.Status)
	}
	if got.BlockedByIssue != "" {
		t.Errorf("BlockedByIssue: got %q want empty", got.BlockedByIssue)
	}
	if !strings.Contains(got.Body, "issue filing disabled") {
		t.Errorf("expected block-only note in body; got:\n%s", got.Body)
	}
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("block_only should not create tasks; got %d tasks", len(all))
	}
}

func TestOnComplete_SybraBugVerdict_NoteOnlyScrubsWorkProject(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()
	h.cfg.HumanReview.SybraBugAction = config.HumanReviewSybraBugActionNoteOnly

	const workProject = "work-owner/work-repo"
	h.workCtx = func(projectID string) *WorkScrubContext {
		if projectID != workProject {
			return nil
		}
		return &WorkScrubContext{
			ProjectID: workProject,
			Blocklist: []string{workProject, "work-owner", "work-repo"},
		}
	}
	tk, err := tasks.Create("Workflow misfire", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		ProjectID: task.Ptr(workProject),
		Status:    task.Ptr(task.StatusHumanRequired),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "verify_commits failed for work-owner/work-repo",
  "issue_title": "fix(workflow): verify_commits race in work-owner/work-repo",
  "issue_body": "## What\nwork-owner/work-repo leaked"
}` + "\n```\n",
	})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	for _, leak := range []string{workProject, "work-owner", "work-repo"} {
		if strings.Contains(got.Body, leak) {
			t.Fatalf("note_only body leaks %q:\n%s", leak, got.Body)
		}
	}
}

func TestOnComplete_StaleVerdictSkipsWhenTaskNoLongerHumanRequired(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Transient failure", "Original body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusTodo)}); err != nil {
		t.Fatalf("requeue task: %v", err)
	}

	ag := &agent.Agent{ID: "agent-stale", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "retryable provider failure",
  "issue_title": "fix(workflow): retry provider failures",
  "issue_body": "details"
}` + "\n```\n",
	})
	h.inflight[tk.ID] = "agent-stale"
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusTodo {
		t.Errorf("status: got %q want todo", got.Status)
	}
	if got.Body != "Original body." {
		t.Errorf("stale verdict should not mutate body; got:\n%s", got.Body)
	}
	if _, busy := h.inflight[tk.ID]; busy {
		t.Errorf("inflight should be cleared after stale onComplete")
	}
}

func TestOnComplete_WorkProject_ConfiguredLocalTaskScrubbed(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()
	h.cfg.HumanReview.SybraBugAction = config.HumanReviewSybraBugActionLocalTask

	const workProject = "work-owner/work-repo"
	h.workCtx = func(projectID string) *WorkScrubContext {
		if projectID != workProject {
			return nil
		}
		return &WorkScrubContext{
			ProjectID: workProject,
			Blocklist: []string{workProject, "work-owner", "work-repo"},
		}
	}

	tk, err := tasks.Create("Workflow misfire", "Body with KAG-1234 reference.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		ProjectID: task.Ptr(workProject),
		Status:    task.Ptr(task.StatusHumanRequired),
	}); err != nil {
		t.Fatalf("assign work project: %v", err)
	}

	// Verdict body contains all three leak vectors: blocklist literal, GH
	// URL, Jira key. With local_task explicitly configured, none may survive
	// in the created task.
	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Diagnosis.\n\n```sybra-verdict\n" + `{
  "decision": "sybra_bug",
  "summary": "verify_commits never re-checked the branch in work-owner/work-repo",
  "issue_title": "fix(workflow): verify_commits race for work-owner/work-repo",
  "issue_body": "## What\nwork-owner referenced https://github.com/work-owner/work-repo/pull/9 (ticket KAG-1234)",
  "issue_labels": ["workflow"]
}` + "\n```\n",
	})
	h.inflight[tk.ID] = "agent-work"
	h.onComplete(ag)

	// Original task: flipped to blocked with a pointer to the local task.
	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load origin: %v", err)
	}
	if got.Status != task.StatusBlocked {
		t.Errorf("origin status: got %q want blocked", got.Status)
	}
	if !strings.Contains(got.Body, "blocked by Sybra bug (scrubbed)") {
		t.Errorf("origin body missing scrubbed-blocked header; got:\n%s", got.Body)
	}

	// A new local task must exist with scrubbed content + sybra-bug tag.
	all, err := tasks.List()
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var local *task.Task
	for i := range all {
		if all[i].ID == tk.ID {
			continue
		}
		t := all[i]
		local = &t
	}
	if local == nil {
		t.Fatalf("expected a second (scrubbed) task to be created; got only origin")
		return
	}
	body := local.Title + "\n" + local.Body
	for _, leak := range []string{workProject, "work-owner", "work-repo", "github.com/work-owner", "KAG-1234"} {
		if strings.Contains(body, leak) {
			t.Errorf("local task leaks %q in title/body: %s", leak, body)
		}
	}
	wantTag := func(needle string) {
		t.Helper()
		if !slices.Contains(local.Tags, needle) {
			t.Errorf("local task missing tag %q; got tags=%v", needle, local.Tags)
		}
	}
	wantTag("sybra-bug")
	wantTag("scrubbed")
	wantTag("workflow")
	if local.ProjectID != h.cfg.HumanReviewRepo() {
		t.Errorf("local task project_id = %q, want %q", local.ProjectID, h.cfg.HumanReviewRepo())
	}
}

func TestOnComplete_MalformedVerdict_AppendsRaw(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Mystery", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "no fenced verdict here"})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required", got.Status)
	}
	if !strings.Contains(got.Body, "unparseable verdict") {
		t.Errorf("expected unparseable header; got:\n%s", got.Body)
	}
}

func TestOnComplete_StructuredVerdictFailure_RetriesAlternateProvider(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const firstAgentID = "hr-structured-1"
	tk, err := tasks.Create("Retry structured review", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("owner/repo"),
	}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:   firstAgentID,
		Role:      string(agent.RoleHumanReview),
		Mode:      "headless",
		Provider:  "claude",
		Model:     "claude-haiku-4-5-20251001",
		State:     "running",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	runner := &fakeHumanReviewAgentRunner{
		apply: func(cfg agent.RunConfig) agent.RunConfig { return cfg },
		run: func(cfg agent.RunConfig) (*agent.Agent, error) {
			if cfg.Provider != "codex" {
				t.Fatalf("fallback provider = %q, want codex", cfg.Provider)
			}
			if cfg.Model != "haiku" {
				t.Fatalf("fallback model = %q, want haiku alias", cfg.Model)
			}
			return &agent.Agent{
				ID:        "hr-structured-2",
				TaskID:    cfg.TaskID,
				Provider:  "codex",
				Model:     "gpt-5.6-luna",
				StartedAt: time.Now().UTC(),
			}, nil
		},
	}
	h.agents = runner

	ag := &agent.Agent{
		ID:       firstAgentID,
		TaskID:   tk.ID,
		Name:     agent.RoleHumanReview.AgentName(tk.Title),
		Provider: "claude",
		Model:    "claude-haiku-4-5-20251001",
	}
	ag.SetHasOutputSchema(true)
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: `{"decision":"human","reason":"","recoverable_action":"none","confidence":"high"}`})
	h.inflight[tk.ID] = firstAgentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if len(got.AgentRuns) != 2 {
		t.Fatalf("agent runs = %d, want 2 (failed structured run + fallback run)", len(got.AgentRuns))
	}
	if !got.AgentRuns[0].VerdictRendered {
		t.Fatalf("first run not rendered: %+v", got.AgentRuns[0])
	}
	if got.AgentRuns[1].Provider != "codex" || got.AgentRuns[1].Model != "gpt-5.6-luna" {
		t.Fatalf("fallback run = %+v, want codex/gpt-5.6-luna", got.AgentRuns[1])
	}
	if _, busy := h.inflight[tk.ID]; !busy {
		t.Fatal("fallback retry should keep the replacement run inflight")
	}
	if strings.Contains(got.Body, "unparseable verdict") {
		t.Fatalf("fallback spawn should not append a terminal note yet; body:\n%s", got.Body)
	}
}

func TestOnComplete_StructuredVerdictFailure_SecondFailureRendersDurableNote(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const currentAgentID = "hr-structured-2"
	tk, err := tasks.Create("Retry exhausted structured review", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("owner/repo"),
	}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	start := time.Now().UTC()
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         "hr-structured-1",
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		Provider:        "claude",
		Model:           "claude-haiku-4-5-20251001",
		State:           "stopped",
		StartedAt:       start.Add(-time.Minute),
		VerdictRendered: true,
	}); err != nil {
		t.Fatalf("add first run: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:   currentAgentID,
		Role:      string(agent.RoleHumanReview),
		Mode:      "headless",
		Provider:  "codex",
		Model:     "gpt-5.6-luna",
		State:     "running",
		StartedAt: start,
	}); err != nil {
		t.Fatalf("add second run: %v", err)
	}

	ag := &agent.Agent{
		ID:       currentAgentID,
		TaskID:   tk.ID,
		Name:     agent.RoleHumanReview.AgentName(tk.Title),
		Provider: "codex",
		Model:    "gpt-5.6-luna",
	}
	ag.SetHasOutputSchema(true)
	h.inflight[tk.ID] = currentAgentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if len(got.AgentRuns) != 2 {
		t.Fatalf("agent runs = %d, want no third retry", len(got.AgentRuns))
	}
	if !got.AgentRuns[1].VerdictRendered {
		t.Fatalf("second run not rendered: %+v", got.AgentRuns[1])
	}
	if !strings.Contains(got.Body, "did not return a usable structured verdict") {
		t.Fatalf("expected durable structured-verdict note; got:\n%s", got.Body)
	}
}

// TestOnComplete_PlaceholderVerdict_RejectedNotFiled pins task 2379fece's
// repro: a run that returns a placeholder/echoed-schema payload
// (summary="test", issue_title="test title", issue_body="test body") must
// not file a GitHub issue or block the task — verdict.Parse rejects it, so
// onComplete treats it like any other unparseable verdict.
func TestOnComplete_PlaceholderVerdict_RejectedNotFiled(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Some task", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}

	ag := &agent.Agent{TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type:    "assistant",
		Content: `{"decision":"sybra_bug","summary":"test","issue_title":"test title","issue_body":"test body"}`,
	})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status: got %q want human-required (placeholder verdict must not block)", got.Status)
	}
	if !strings.Contains(got.Body, "unparseable verdict") {
		t.Errorf("expected unparseable-verdict note; got:\n%s", got.Body)
	}
}

func TestOnComplete_RateLimitedVerdictDoesNotRenderNoise(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Quota limited", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{AgentID: "hr1", Role: string(agent.RoleHumanReview)}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: "hr1", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.SetError("rate_limit", "weekly limit")
	ag.AppendOutput(agent.StreamEvent{Type: "result", Content: "You've hit your weekly limit"})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if strings.Contains(got.Body, "unparseable verdict") {
		t.Errorf("rate-limited human-review should not append unparseable verdict noise; got:\n%s", got.Body)
	}
	for _, run := range got.AgentRuns {
		if run.AgentID == "hr1" && run.VerdictRendered {
			t.Fatal("rate-limited human-review must not mark verdict_rendered")
		}
	}
}

// TestOnComplete_SilentHangVerdictDoesNotRenderNoise mirrors the rate-limited
// case for a review agent the watchdog killed for producing nothing. It has no
// output at all, so without an explicit defer it reads as an unparseable
// verdict, gets a "crashed before producing a verdict" note appended, and
// latches verdict_rendered — retiring the review permanently over a hang that
// is about to be re-dispatched.
func TestOnComplete_SilentHangVerdictDoesNotRenderNoise(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Silent review", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{AgentID: "hr1", Role: string(agent.RoleHumanReview)}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: "hr1", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.SetError(agent.ErrorKindSilentHang, watchdogreason.ZeroOutputBeforeStartup)
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if strings.Contains(got.Body, "unparseable verdict") || strings.Contains(got.Body, "crashed before producing a verdict") {
		t.Errorf("silently-hung human-review should not append verdict noise; got:\n%s", got.Body)
	}
	for _, run := range got.AgentRuns {
		if run.AgentID == "hr1" && run.VerdictRendered {
			t.Fatal("silently-hung human-review must not mark verdict_rendered")
		}
	}
}

// TestOnComplete_ExecutionCrashRendersDiagnosis pins the no-dead-end contract
// for a crash AFTER real tool calls: HadTerminalError+ToolCalls==0 is already
// routed to handleCrashedVerdict's retry-budget path (see
// TestOnComplete_CrashedVerdict_ExhaustedRetriesMarksDistinguishableNote), but
// that path only fires on an "instant" crash. A run that did real diagnostic
// work and then hit an exit error (error_during_execution) has no retry
// trigger either (maybeSpawn only fires on a fresh transition, and a
// directly-spawned review agent is not a rescheduled workflow step), so it
// must render an honest, non-empty crash note and latch VerdictRendered —
// leaving the task in human-required for a human WITH a diagnosis, rather
// than deferring into a dead-end that strands it silently.
func TestOnComplete_ExecutionCrashRendersDiagnosis(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Crashed review", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{AgentID: "hr-crash", Role: string(agent.RoleHumanReview)}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: "hr-crash", TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	// Crash after real work: some tool calls were made (so the zero-tool-call
	// retry-budget path does not intercept this), then an error result event
	// + recorded exit error, no parseable verdict text (and, critically, no
	// assistant text — the fall-through unparseable path would append an
	// empty note and latch nothing).
	ag.AddToolCalls(2)
	ag.SetExitErr(errors.New("provider result error error_during_execution"))
	ag.AppendOutput(agent.StreamEvent{Type: "result", Subtype: "error_during_execution"})
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if strings.Contains(got.Body, "unparseable verdict") {
		t.Errorf("crashed human-review must not mislabel the crash as unparseable; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "Auto-review did not complete (execution error)") {
		t.Errorf("crashed human-review must render an honest crash note; got:\n%s", got.Body)
	}
	rendered := false
	for _, run := range got.AgentRuns {
		if run.AgentID == "hr-crash" && run.VerdictRendered {
			rendered = true
		}
	}
	if !rendered {
		t.Fatal("crashed human-review must latch verdict_rendered — there is no retry trigger, so the diagnosis must be durable")
	}
	if !verdictAlreadyRendered(got) {
		t.Fatal("verdictAlreadyRendered must be true so a restart does not silently drop the crashed task")
	}
	// The task stays in human-required — a human still owns it, the auto-review
	// just could not diagnose it.
	if got.Status != task.StatusHumanRequired {
		t.Errorf("crashed review must leave task in human-required; got %s", got.Status)
	}
}

func TestMaybeSpawn_IdempotencyGate_SkipsWhenVerdictRendered(t *testing.T) {
	t.Parallel()
	// h.agents is nil in newReviewTestEnv — if maybeSpawn tries to spawn an
	// agent past the gate it will panic, which is the test's failure signal.
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Stale task", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	// Simulate a fully completed review: VerdictRendered proves onComplete ran.
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         "agent-prior",
		Role:            "human-review",
		Mode:            "headless",
		State:           "stopped",
		Verdict:         "human",
		VerdictRendered: true,
	}); err != nil {
		t.Fatalf("add prior run: %v", err)
	}

	// Must not panic (agents is nil) and must skip.
	h.maybeSpawn(context.Background(), tk.ID, "")

	h.mu.Lock()
	_, busy := h.inflight[tk.ID]
	h.mu.Unlock()
	if busy {
		t.Error("expected no inflight entry — gate should have skipped the spawn")
	}
}

func TestMaybeSpawn_SkipsWhenTaskNoLongerHumanRequired(t *testing.T) {
	t.Parallel()
	// h.agents is nil in newReviewTestEnv — a stale human-required hook must
	// exit before attempting a real spawn.
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Stale hook task", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusBlocked),
		ProjectID: task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("update task: %v", err)
	}

	h.maybeSpawn(context.Background(), tk.ID, string(task.StatusTodo))

	h.mu.Lock()
	_, busy := h.inflight[tk.ID]
	h.mu.Unlock()
	if busy {
		t.Error("expected no inflight entry — stale hook should skip the spawn")
	}
}

func TestMaybeSpawn_RechecksStatusBeforeRun(t *testing.T) {
	t.Parallel()

	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Racey hook task", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	runner := &fakeHumanReviewAgentRunner{}
	runCalls := 0
	runner.apply = func(cfg agent.RunConfig) agent.RunConfig {
		if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusBlocked)}); err != nil {
			t.Fatalf("advance task before run: %v", err)
		}
		return cfg
	}
	runner.run = func(cfg agent.RunConfig) (*agent.Agent, error) {
		runCalls++
		return &agent.Agent{ID: "should-not-run", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}
	h.agents = runner

	if spawned := h.maybeSpawn(context.Background(), tk.ID, string(task.StatusTodo)); spawned {
		t.Fatal("maybeSpawn = true, want false after task left human-required before Run")
	}
	if runCalls != 0 {
		t.Fatalf("Run called %d times, want 0", runCalls)
	}

	h.mu.Lock()
	_, busy := h.inflight[tk.ID]
	recent := len(h.recent)
	perTask := len(h.perTask[tk.ID])
	h.mu.Unlock()
	if busy {
		t.Fatal("expected no inflight entry after stale pre-run recheck")
	}
	if recent != 0 {
		t.Fatalf("recent reservations = %d, want 0 after stale pre-run recheck", recent)
	}
	if perTask != 0 {
		t.Fatalf("per-task reservations = %d, want 0 after stale pre-run recheck", perTask)
	}

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got.Status != task.StatusBlocked {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusBlocked)
	}
	if len(got.AgentRuns) != 0 {
		t.Fatalf("agent runs = %d, want 0", len(got.AgentRuns))
	}
}

func TestMaybeSpawn_IdempotencyGate_IgnoresRenderedVerdictBeforeTestingCycle(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Re-tested task", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	cycleStart := time.Now().UTC()
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:                task.Ptr(task.StatusHumanRequired),
		ProjectID:             task.Ptr("Automaat/sybra"),
		TestingCycleStartedAt: &cycleStart,
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         "agent-old-review",
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		State:           "stopped",
		StartedAt:       cycleStart.Add(-time.Minute),
		Verdict:         "human",
		VerdictRendered: true,
	}); err != nil {
		t.Fatalf("add prior run: %v", err)
	}

	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		h.maybeSpawn(context.Background(), tk.ID, "")
		return false
	}()
	if !panicked {
		t.Fatal("expected spawn attempt; old rendered verdict must not block a new testing cycle")
	}
}

func TestMaybeSpawn_IdempotencyGate_SpawnsWhenVerdictSetButNotRendered(t *testing.T) {
	t.Parallel()
	// Regression test for the crash-window: Verdict persisted by onAgentComplete
	// but the process crashed before onComplete appended the "## Auto-review"
	// section. The gate must allow a re-spawn so the task is not permanently
	// stranded at human-required with no diagnosis.
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Crash-window task", "Body without auto-review section.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired), ProjectID: task.Ptr("Automaat/sybra")}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	// Verdict is set (persisted by onAgentComplete) but onComplete never ran —
	// the task body has no "## Auto-review" section.
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-crashed",
		Role:    "human-review",
		Mode:    "headless",
		State:   "stopped",
		Verdict: "human",
	}); err != nil {
		t.Fatalf("add prior run: %v", err)
	}

	// Gate must NOT block — panic on nil agents.Run confirms spawn was attempted.
	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		h.maybeSpawn(context.Background(), tk.ID, "")
		return false
	}()
	if !panicked {
		t.Fatal("expected spawn attempt; gate must not block when Verdict is set but VerdictRendered is false (crash window)")
	}
}

func TestMaybeSpawn_IdempotencyGate_PreexistingAutoReviewTextDoesNotBlock(t *testing.T) {
	t.Parallel()
	// Regression: a task body that already contains a heading beginning with
	// "## Auto-review" (for reasons unrelated to human-review rendering) must
	// NOT satisfy the idempotency gate when onComplete has not run.
	// The gate now checks AgentRun.VerdictRendered, not body text, so
	// pre-existing headings never falsely block a re-spawn.
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	body := "Original task body.\n\n## Auto-review requirements\n\nThis task is about auto-review diagnostics, but no diagnostic has been rendered yet.\n"
	tk, err := tasks.Create("Crash window with preexisting auto-review text", body, "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired), ProjectID: task.Ptr("Automaat/sybra")}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	// Verdict persisted by onAgentComplete but onComplete never rendered the note.
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-crashed",
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "stopped",
		Verdict: "human",
	}); err != nil {
		t.Fatalf("add prior run: %v", err)
	}

	// Gate must NOT block — panic on nil agents.Run confirms spawn was attempted.
	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		h.maybeSpawn(context.Background(), tk.ID, "")
		return false
	}()
	if !panicked {
		t.Fatal("expected spawn attempt; gate should not treat pre-existing ## Auto-review heading as a rendered verdict")
	}
}

func TestMaybeSpawn_IdempotencyGate_SpawnsWhenNoVerdict(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Fresh task", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired), ProjectID: task.Ptr("Automaat/sybra")}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	// Add a run with NO verdict (e.g. agent was killed mid-run).
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "agent-killed",
		Role:    "human-review",
		Mode:    "headless",
		State:   "stopped",
		Verdict: "",
	}); err != nil {
		t.Fatalf("add prior run: %v", err)
	}

	// Gate should NOT block — recover from the expected nil-agents panic to
	// confirm that spawn was attempted (past the idempotency gate).
	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		h.maybeSpawn(context.Background(), tk.ID, "")
		return false
	}()
	if !panicked {
		t.Fatal("expected spawn attempt; gate must not block a run with no verdict (agent killed mid-run)")
	}
}

// TestMaybeSpawn_SkipsUmbrellaTracker reproduces the production incident
// where an umbrella tracker's rollup flips its own status to human-required
// (e.g. reason "umbrella child needs attention") and the status hook spawns
// a real human-review agent onto it — the tracker has no product code to
// analyze, so the agent falls into a repetitive info-gathering loop until
// the watchdog kills it and re-parks the task in human-required (#2610).
func TestMaybeSpawn_SkipsUmbrellaTracker(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Umbrella tracker", "queued", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		TaskType:  task.Ptr(task.TaskTypeUmbrella),
		ProjectID: task.Ptr("Automaat/sybra"),
		Status:    task.Ptr(task.StatusHumanRequired),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	if spawned := h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInProgress)); spawned {
		t.Error("expected maybeSpawn to report no spawn for an umbrella tracker")
	}

	h.mu.Lock()
	_, busy := h.inflight[tk.ID]
	h.mu.Unlock()
	if busy {
		t.Error("expected no inflight entry — an umbrella tracker must not spawn a review agent")
	}
}

func TestMaybeSpawn_SkipsProjectlessTask(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Orphan smoke-test task", "queued", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}

	h.maybeSpawn(context.Background(), tk.ID, "")

	h.mu.Lock()
	_, busy := h.inflight[tk.ID]
	h.mu.Unlock()
	if busy {
		t.Error("expected no inflight entry — a project-less task must not spawn a review agent in the Sybra source tree")
	}
}

func TestMaybeSpawn_SkipsStaleNonHumanRequiredTask(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("Already handled task", "queued", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusBlocked),
		ProjectID: task.Ptr("Automaat/sybra"),
	}); err != nil {
		t.Fatalf("flip to blocked: %v", err)
	}

	h.maybeSpawn(context.Background(), tk.ID, string(task.StatusHumanRequired))

	h.mu.Lock()
	_, busy := h.inflight[tk.ID]
	h.mu.Unlock()
	if busy {
		t.Error("expected no inflight entry - stale non-human-required task must not spawn a review agent")
	}
}

func TestOnComplete_SetsVerdictRendered(t *testing.T) {
	t.Parallel()
	// Verifies that onComplete sets VerdictRendered on the matching AgentRun so
	// verdictAlreadyRendered can use it as the durable rendered-marker on restart.
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-hr-1"
	tk, err := tasks.Create("Billing refactor", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{
		Type: "assistant",
		Content: "Analysis done.\n\n```sybra-verdict\n" +
			`{"decision":"human","summary":"scope clarification needed"}` +
			"\n```\n",
	})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	var rendered bool
	for i := range got.AgentRuns {
		if got.AgentRuns[i].AgentID == agentID {
			rendered = got.AgentRuns[i].VerdictRendered
			break
		}
	}
	if !rendered {
		t.Error("expected AgentRun.VerdictRendered=true after onComplete; idempotency gate will not block re-spawn")
	}
}

// TestOnComplete_CrashedVerdict_RetriesOnce pins that a review run that
// crashed before producing any output (e.g. error_during_execution) triggers
// a retry spawn rather than being treated like a normal unparseable verdict.
// h.agents is nil in newReviewTestEnv, so a genuine retry attempt panics
// inside maybeSpawn — the same "panic proves a spawn was attempted" signal
// TestMaybeSpawn_IdempotencyGate_SkipsWhenVerdictRendered relies on.
func TestOnComplete_CrashedVerdict_RetriesOnce(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-hr-crash-1"
	tk, err := tasks.Create("Crashed review", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// ProjectID must be non-empty: maybeSpawn's no_project gate would
	// otherwise skip before the retry logic under test ever runs.
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("owner/repo"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Subtype: "error_during_execution"})
	h.inflight[tk.ID] = agentID

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a retry spawn attempt (nil h.agents panics inside maybeSpawn)")
		}
	}()
	h.onComplete(ag)
	t.Fatal("onComplete should have panicked via the retry spawn before reaching here")
}

// TestOnComplete_CrashedVerdict_ExhaustedRetriesMarksDistinguishableNote pins
// that once the per-task spawn budget is exhausted (this crash would be the
// second spawn within the window), onComplete stops retrying and leaves a
// note that says the automated recovery crashed rather than reviewed the
// task — not the generic "unparseable verdict" text, and not silence.
func TestOnComplete_CrashedVerdict_ExhaustedRetriesMarksDistinguishableNote(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-hr-crash-2"
	tk, err := tasks.Create("Crashed review, exhausted", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("owner/repo"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}
	// Simulate the per-task window budget already spent (humanReviewMaxPerTaskPerWindow=2):
	// this completion represents the second spawn, so no further retry fits.
	h.perTask[tk.ID] = []time.Time{h.now(), h.now()}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Subtype: "error_during_execution"})
	h.inflight[tk.ID] = agentID
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if !strings.Contains(got.Body, "crashed") {
		t.Errorf("expected a distinguishable crash note in body; got:\n%s", got.Body)
	}
	if strings.Contains(got.Body, "unparseable verdict") {
		t.Errorf("crash exhaustion should not read like a normal unparseable verdict; got:\n%s", got.Body)
	}
	var rendered bool
	for i := range got.AgentRuns {
		if got.AgentRuns[i].AgentID == agentID {
			rendered = got.AgentRuns[i].VerdictRendered
			break
		}
	}
	if !rendered {
		t.Error("expected VerdictRendered=true once retries are exhausted, so the task doesn't loop forever")
	}
}

// TestOnComplete_CrashedVerdict_GlobalCapDeclinesRetrySilently pins that a
// retry maybeSpawn declines for a reason retryAfterCrash never checks itself
// (here: the fleet-wide global cap, allowSpawnLocked) still lands on the
// distinguishable crashed-exhausted note rather than acting as if a retry
// actually happened — retryAfterCrash trusts maybeSpawn's own bool return,
// it has no budget logic of its own to get out of sync with.
func TestOnComplete_CrashedVerdict_GlobalCapDeclinesRetrySilently(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	h.cfg.HumanReview.MaxPerHour = 1
	h.recent = []time.Time{h.now()}

	const agentID = "agent-hr-crash-global"
	tk, err := tasks.Create("Crashed review, global cap", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{
		Status:    task.Ptr(task.StatusHumanRequired),
		ProjectID: task.Ptr("owner/repo"),
	}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{Type: "result", Subtype: "error_during_execution"})
	h.inflight[tk.ID] = agentID
	// Must not panic: the global cap declines inside maybeSpawn before it
	// ever reaches the nil h.agents call that TestOnComplete_CrashedVerdict_
	// RetriesOnce relies on panicking.
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if !strings.Contains(got.Body, "crashed") {
		t.Errorf("expected a distinguishable crash note in body; got:\n%s", got.Body)
	}
	var rendered bool
	for i := range got.AgentRuns {
		if got.AgentRuns[i].AgentID == agentID {
			rendered = got.AgentRuns[i].VerdictRendered
			break
		}
	}
	if !rendered {
		t.Error("expected VerdictRendered=true once the global cap silently declines the retry")
	}
}

// TestOnComplete_TerminalErrorWithToolCalls_SkipsCrashPath pins that a run
// which made real tool calls before hitting a terminal error is NOT treated
// as "crashed before doing anything" — it must fall through to the ordinary
// unparseable-verdict path so its (possibly substantive) output is preserved
// instead of being discarded for a pointless retry.
func TestOnComplete_TerminalErrorWithToolCalls_SkipsCrashPath(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	const agentID = "agent-hr-worked-then-errored"
	tk, err := tasks.Create("Errored after real work", "Body.", "headless")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := tasks.Update(tk.ID, task.Update{Status: task.Ptr(task.StatusHumanRequired)}); err != nil {
		t.Fatalf("flip to human-required: %v", err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: agentID,
		Role:    string(agent.RoleHumanReview),
		Mode:    "headless",
		State:   "running",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}

	ag := &agent.Agent{ID: agentID, TaskID: tk.ID, Name: agent.RoleHumanReview.AgentName(tk.Title)}
	ag.AppendOutput(agent.StreamEvent{Type: "assistant", Content: "Investigated the failure at length."})
	ag.AppendOutput(agent.StreamEvent{
		Type:    "result",
		Subtype: "error_during_execution",
		Content: "Ran out of turns before finishing the investigation.",
	})
	ag.AddToolCalls(3)
	h.inflight[tk.ID] = agentID
	// h.agents is nil: if this went down the crash-retry path it would
	// attempt a spawn and panic, which is this test's failure signal too.
	h.onComplete(ag)

	got, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if strings.Contains(got.Body, "Auto-review (crashed)") {
		t.Errorf("a run with tool calls should not use the crashed-run note; got:\n%s", got.Body)
	}
	if !strings.Contains(got.Body, "Auto-review (unparseable verdict)") {
		t.Errorf("expected the ordinary unparseable-verdict path; got:\n%s", got.Body)
	}
}

// The provider-fallback path marks the verdict rendered and then deliberately
// re-spawns with IgnoreRenderedVerdict. If that respawn loses a claim race the
// retry must still run: honouring the idempotency gate there drops the fallback
// permanently and audits it as a duplicate diagnosis, leaving the task parked
// with no verdict at all.
func TestHumanReviewSpawn_ContendedFallbackRetryIgnoresRenderedVerdict(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("contended fallback", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}
	// The fallback caller has already persisted the rendered verdict.
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID:         "malformed-run",
		Role:            string(agent.RoleHumanReview),
		Mode:            "headless",
		State:           "stopped",
		Outcome:         "failure",
		VerdictRendered: true,
		Result:          "not json",
	}); err != nil {
		t.Fatalf("add run: %v", err)
	}
	seeded, err := tasks.Get(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !verdictAlreadyRendered(seeded) {
		t.Fatal("precondition: verdict must already be rendered or this asserts nothing")
	}

	claimAvailable := false
	claimHeld := false
	h.claimTaskDispatch = func(string) (func(), bool) {
		if !claimAvailable || claimHeld {
			return nil, false
		}
		claimHeld = true
		return func() { claimHeld = false }, true
	}
	var scheduled func()
	h.schedule = func(_ time.Duration, fn func()) { scheduled = fn }
	h.prepareTaskWorktree = func(task.Task) (string, error) { return t.TempDir(), nil }
	runCalls := 0
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		runCalls++
		return &agent.Agent{ID: "fallback-run", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	opts := humanReviewSpawnOptions{IgnoreRenderedVerdict: true}
	if h.maybeSpawnWithOptions(context.Background(), tk.ID, string(task.StatusHumanRequired), opts) {
		t.Fatal("spawned despite a conflicting dispatch claim")
	}
	if scheduled == nil {
		t.Fatal("contended fallback did not schedule a retry")
	}

	claimAvailable = true
	scheduled()

	if runCalls != 1 {
		t.Errorf("fallback retry runs = %d, want 1 — the rendered-verdict gate swallowed it", runCalls)
	}
}

// TestWriteAutonomyMandate_UsesHostCommitFlags passes its own strings in and
// asserts they come back out, so it proves Fprintf works and never touches the
// production argument. This covers the argument: the recovery prompt must honor
// agent.commit_signing, on a host that DOES resolve a key so "never" and the
// host default are distinguishable.
func TestHumanReviewPrompt_HonorsCommitSigningPolicy(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "gitconfig")
	contents := "[user]\n\tname = Test\n\temail = t@example.invalid\n\tsigningkey = DEADBEEFDEADBEEF\n"
	if err := os.WriteFile(cfgPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write git config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	if got := project.SigningAuto.CommitFlags(context.Background()); got != "-s -S" {
		t.Fatalf("precondition: host must resolve a signing key, got %q", got)
	}

	cfg := &config.Config{}
	cfg.Agent.CommitSigning = "never"
	h := &humanReviewHandler{cfg: cfg}

	if got := h.signingPolicy(); got != project.SigningNever {
		t.Fatalf("signingPolicy() = %q, want never from the snapshot", got)
	}

	// Go through the real prompt builder, not writeAutonomyMandate directly:
	// asserting on a value this test supplies is what made the original test
	// unable to catch the defect it appeared to cover.
	prompt := h.buildPrompt(context.Background(), task.Task{ID: "t1", ProjectID: "Automaat/sybra"}, t.TempDir(), nil, false)
	for line := range strings.Lines(prompt) {
		if strings.Contains(line, "commit") && strings.Contains(line, "-S") {
			t.Errorf("mandate instructs GPG signing under the never policy:\n%s", line)
		}
	}

	// And a hot reload must reach it, since cfg is never rewritten.
	h.SetSigningPolicy(project.SigningRequire)
	if got := h.signingPolicy().CommitFlags(context.Background()); got != "-s -S" {
		t.Errorf("after reload to require, flags = %q, want -s -S", got)
	}
}

// The ladder is bounded on both ends: long enough to outlast a claim held
// across PrepareForTask (its setup batch alone runs five minutes), short
// enough that no retry lands after agent.StaleDispatchClaimAge, where the
// age-based release could hand it a claim a live holder is still using.
//
// Delays are pinned to literals, not to the constants under test, or a unit
// typo in humanReviewClaimRetryStep passes by moving both sides at once.
func TestScheduleClaimRetry_LadderIsBoundedOnBothEnds(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	var delays []time.Duration
	h.schedule = func(d time.Duration, _ func()) { delays = append(delays, d) }

	opts := humanReviewSpawnOptions{}
	for range humanReviewClaimRetryMax {
		h.scheduleClaimRetry(context.Background(), "t1", string(task.StatusInReview), opts)
		opts.ClaimRetryAttempt++
	}
	h.scheduleClaimRetry(context.Background(), "t1", string(task.StatusInReview), opts)

	want := []time.Duration{
		9 * time.Second, 36 * time.Second, 81 * time.Second,
		144 * time.Second, 225 * time.Second, 324 * time.Second,
	}
	if !slices.Equal(delays, want) {
		t.Fatalf("ladder = %v, want %v", delays, want)
	}
	var span time.Duration
	for _, d := range delays {
		span += d
	}
	// A setup batch alone was measured holding the claim for ten minutes.
	if span < 11*time.Minute {
		t.Errorf("ladder spans %s, gives up while a normal preparation is still running", span)
	}
	if span >= agent.StaleDispatchClaimAge {
		t.Errorf("ladder spans %s, retries past the age-based claim release (%s)",
			span, agent.StaleDispatchClaimAge)
	}
}

// Exhaustion is terminal and nothing re-triggers it: recoverStrandedUnblocked
// Tasks replays verdicts whose side effects failed, never a spawn that was
// wanted and never happened. Without an event the only trace is an Info line.
func TestScheduleClaimRetry_ExhaustionIsAudited(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	auditDir := t.TempDir()
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	h.audit = auditLog
	h.schedule = func(time.Duration, func()) {}

	h.scheduleClaimRetry(context.Background(), "t1", string(task.StatusInReview),
		humanReviewSpawnOptions{ClaimRetryAttempt: humanReviewClaimRetryMax})

	events, err := audit.Read(auditDir, audit.Query{
		Since: time.Now().Add(-time.Minute),
		Until: time.Now().Add(time.Minute),
		Type:  audit.EventHumanReviewRetriesExhausted,
	})
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("exhausted ladder wrote %d audit events, want 1", len(events))
	}
	if got := events[0].TaskID; got != "t1" {
		t.Errorf("task_id = %q, want t1", got)
	}
	if got := events[0].Data["attempts"]; got != float64(humanReviewClaimRetryMax) {
		t.Errorf("attempts = %v, want %d", got, humanReviewClaimRetryMax)
	}
}

// A retry that is still within the ladder must not be audited as exhausted, or
// the event stops meaning "this task was dropped".
func TestScheduleClaimRetry_InProgressIsNotAudited(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	auditDir := t.TempDir()
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatalf("audit.NewLogger: %v", err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	h.audit = auditLog
	h.schedule = func(time.Duration, func()) {}

	h.scheduleClaimRetry(context.Background(), "t1", string(task.StatusInReview), humanReviewSpawnOptions{})

	events, err := audit.Read(auditDir, audit.Query{
		Since: time.Now().Add(-time.Minute),
		Until: time.Now().Add(time.Minute),
		Type:  audit.EventHumanReviewRetriesExhausted,
	})
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("first retry audited as exhausted: %+v", events)
	}

	// A handler with no scheduler ran no ladder at all, so calling that
	// exhausted would give one event two incompatible meanings.
	h.schedule = nil
	h.scheduleClaimRetry(context.Background(), "t2", string(task.StatusInReview), humanReviewSpawnOptions{})
	events, err = audit.Read(auditDir, audit.Query{
		Since: time.Now().Add(-time.Minute),
		Until: time.Now().Add(time.Minute),
		Type:  audit.EventHumanReviewRetriesExhausted,
	})
	if err != nil {
		t.Fatalf("audit.Read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("absent scheduler audited as exhausted: %+v", events)
	}
}

// A pending claim retry is armed on the scheduler context, which every
// graceful shutdown cancels, and lifecycleSchedule drops the callback without
// a trace. Nothing else replays it — recoverStrandedUnblockedTasks needs a
// completed verdict, and a task whose review never spawned has no run to hold
// one — so the startup sweep is the only thing standing between a restart and
// a park that is never examined.
func TestRespawnDroppedReviews(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		parkedAgo   time.Duration
		existingRun bool
		declined    bool
		wantSpawn   bool
	}{
		{name: "recent park with no review respawns", parkedAgo: time.Minute, wantSpawn: true},
		{name: "park already reviewed is left alone", parkedAgo: time.Minute, existingRun: true},
		{name: "park older than the sweep window is left alone", parkedAgo: 3 * time.Hour},
		// maybeSpawn's prev_status_blocked guard cannot fire for a sweep, which
		// never sees the transition. Prior agent activity was a poor proxy —
		// any task that ever ran an agent has some — so the live path records
		// its refusal instead.
		{name: "park the live path declined is left alone", parkedAgo: time.Minute, declined: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, tasks, cleanup := newReviewTestEnv(t)
			defer cleanup()

			tk, err := tasks.Create("dropped review", "", task.AgentModeHeadless)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.UpdateMap(tk.ID, map[string]any{
				"project_id": "Automaat/sybra",
				"status":     string(task.StatusHumanRequired),
			}); err != nil {
				t.Fatal(err)
			}
			if tc.existingRun {
				if err := tasks.AddRun(tk.ID, task.AgentRun{
					AgentID: "prior", Role: string(agent.RoleHumanReview),
					State: string(agent.StateStopped),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := tasks.AddRun(tk.ID, task.AgentRun{
				AgentID: "impl", Role: string(agent.RoleImplementation),
				State: string(agent.StateStopped),
			}); err != nil {
				t.Fatal(err)
			}
			if tc.declined {
				// Exactly what the live path does on a blocked transition.
				if h.maybeSpawn(context.Background(), tk.ID, string(task.StatusBlocked)) {
					t.Fatal("live path spawned on a blocked transition")
				}
			}
			// Age the park by moving the clock, not the file: UpdatedAt is
			// rewritten by every store mutation above.
			h.now = func() time.Time { return time.Now().Add(tc.parkedAgo) }

			// Pre-created: the spawn runs on its own goroutine, so calling
			// t.TempDir() from inside it races the harness's own cleanup.
			wtDir := t.TempDir()
			h.prepareTaskWorktree = func(task.Task) (string, error) { return wtDir, nil }
			spawns := make(chan string, 4)
			h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
				spawns <- cfg.TaskID
				return &agent.Agent{ID: "swept", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
			}}

			h.RespawnDroppedReviews(context.Background())

			if !tc.wantSpawn {
				select {
				case id := <-spawns:
					t.Fatalf("sweep spawned a review for %s that it should have skipped", id)
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			select {
			case <-spawns:
			case <-time.After(5 * time.Second):
				t.Fatal("sweep never spawned a review for a recent never-reviewed park")
			}
			// A spawn's last store write, so its goroutine is done before the
			// harness removes the store's temp dir underneath it.
			waitForHumanReviewRun(t, tasks, tk.ID)
		})
	}
}

func waitForHumanReviewRun(t *testing.T, tasks *task.Manager, id string) {
	t.Helper()
	for range 200 {
		got, err := tasks.Get(id)
		if err == nil && hasHumanReviewRun(got) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("spawned review agent never recorded a run")
}

// Measured live: running the sweep inline serialized one full PrepareForTask
// per swept task, and a single setup batch burned its whole ten-minute timeout
// — startup did not arm dispatch for 9m32s, and no live park in that window
// got a review at all. A sweep for stale work must not delay the current work.
func TestRespawnDroppedReviews_DoesNotBlockOnPreparation(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	for range 3 {
		tk, err := tasks.Create("dropped review", "", task.AgentModeHeadless)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tasks.UpdateMap(tk.ID, map[string]any{
			"project_id": "Automaat/sybra",
			"status":     string(task.StatusHumanRequired),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Both fail after unblocking: the spawn goroutines outlive the assertion,
	// and a successful one would still be writing to the task store while the
	// harness removes it.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		<-release
		return "", errors.New("torn down")
	}
	h.agents = &fakeHumanReviewAgentRunner{run: func(agent.RunConfig) (*agent.Agent, error) {
		return nil, errors.New("torn down")
	}}

	done := make(chan struct{})
	go func() {
		h.RespawnDroppedReviews(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep blocked on worktree preparation; startup cannot arm dispatch behind it")
	}
}

// The human-required status hook spawns while the agent that just failed is
// still winding down in the registry, so PrepareForTask returns
// ErrAgentRunning on the most common entry path into recovery. Falling back to
// the read-only Sybra source there is the exact condition writable recovery
// worktrees exist to eliminate — three agents on three tasks each verified a
// fix and then reported they could not apply it.
func TestHumanReviewDispatchDir_RetryableErrorsDoNotFallBackReadOnly(t *testing.T) {
	tests := []struct {
		name          string
		prepareErr    error
		wantRetryable bool
	}{
		{"live agent still winding down", worktree.ErrAgentRunning, true},
		{"transient fetch blip", worktree.ErrTransientFetch, true},
		{
			// A task that genuinely has no worktree is what the read-only
			// fallback is for: diagnosing Sybra itself.
			name:          "permanent failure still falls back",
			prepareErr:    errors.New("no worktree and no branch"),
			wantRetryable: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _, cleanup := newReviewTestEnv(t)
			defer cleanup()
			h.cfg.HumanReview.SybraRepoDir = "/opt/sybra/src"
			h.prepareTaskWorktree = func(task.Task) (string, error) { return "", tc.prepareErr }

			dir, readOnly, retryable, _ := h.dispatchDir(task.Task{ID: "t1"})
			if retryable != tc.wantRetryable {
				t.Fatalf("retryable = %v, want %v", retryable, tc.wantRetryable)
			}
			if tc.wantRetryable {
				if dir != "" || readOnly {
					t.Errorf("retryable prepare returned dir=%q readOnly=%v, want no dispatch", dir, readOnly)
				}
				return
			}
			if dir != "/opt/sybra/src" || !readOnly {
				t.Errorf("permanent failure gave dir=%q readOnly=%v, want the read-only fallback", dir, readOnly)
			}
		})
	}
}

// Classifying the error is not enough: the spawn path must actually wait
// instead of dispatching. Without this the agent still runs, just against the
// read-only fallback.
func TestHumanReviewSpawn_RetryablePrepareSchedulesRetry(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("retryable prepare", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}

	prepareCalls := 0
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		prepareCalls++
		if prepareCalls == 1 {
			return "", worktree.ErrAgentRunning
		}
		return t.TempDir(), nil
	}
	var scheduled func()
	h.schedule = func(_ time.Duration, fn func()) { scheduled = fn }
	runCalls := 0
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		runCalls++
		if cfg.ReadOnlyDir {
			t.Error("dispatched read-only after a retryable prepare failure")
		}
		return &agent.Agent{ID: "recovered", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	if h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("spawned despite a retryable prepare failure")
	}
	if runCalls != 0 {
		t.Fatalf("agent ran %d times before the retry", runCalls)
	}
	if scheduled == nil {
		t.Fatal("retryable prepare did not schedule a retry")
	}

	scheduled()

	if runCalls != 1 {
		t.Errorf("agent runs after retry = %d, want 1", runCalls)
	}
}

// A spawn armed before BeginDrain must not start a provider process after it.
// Measured live: a six-way sweep interrupted by SIGTERM started ten agents and
// wrote their runs and verdicts after app.draining.
func TestHumanReviewSpawn_RefusesAfterDrain(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("drain", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}

	h.prepareTaskWorktree = func(task.Task) (string, error) { return t.TempDir(), nil }
	started := 0
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		started++
		return &agent.Agent{ID: "late", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if h.maybeSpawn(ctx, tk.ID, string(task.StatusInReview)) {
		t.Fatal("spawned after the scheduler context was cancelled")
	}
	if started != 0 {
		t.Errorf("started %d provider agents during shutdown, want 0", started)
	}
}

// A single PrepareForTask can hold the claim for its fetch budget plus its
// setup budget — measured at 14m44s — while the ladder must stay under
// StaleDispatchClaimAge (15m). Exhaustion therefore cannot be terminal: the
// sweep has to pick the task back up rather than leave it parked forever.
func TestRespawnDroppedReviews_RecoversAnExhaustedLadder(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("exhausted ladder", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "impl", Role: string(agent.RoleImplementation), State: string(agent.StateStopped),
	}); err != nil {
		t.Fatal(err)
	}

	// The ladder runs out while the claim is still held.
	h.schedule = func(time.Duration, func()) {}
	h.claimTaskDispatch = func(string) (func(), bool) { return nil, false }
	h.scheduleClaimRetry(context.Background(), tk.ID, string(task.StatusInReview),
		humanReviewSpawnOptions{ClaimRetryAttempt: humanReviewClaimRetryMax})

	// The preparation finishes and the claim frees.
	wtDir := t.TempDir()
	h.claimTaskDispatch = func(string) (func(), bool) { return func() {}, true }
	h.prepareTaskWorktree = func(task.Task) (string, error) { return wtDir, nil }
	spawns := make(chan string, 2)
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		spawns <- cfg.TaskID
		return &agent.Agent{ID: "swept", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	h.RespawnDroppedReviews(context.Background())

	select {
	case <-spawns:
	case <-time.After(5 * time.Second):
		t.Fatal("an exhausted ladder is never retried; the task stays parked forever")
	}
	waitForHumanReviewRun(t, tasks, tk.ID)
}

// The recovery is only real if the maintenance tick reaches it. Startup alone
// leaves an exhausted ladder parked until the next restart.
func TestMaintenancePass_RunsTheDroppedReviewSweep(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("swept on tick", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tasks.AddRun(tk.ID, task.AgentRun{
		AgentID: "impl", Role: string(agent.RoleImplementation), State: string(agent.StateStopped),
	}); err != nil {
		t.Fatal(err)
	}

	wtDir := t.TempDir()
	h.prepareTaskWorktree = func(task.Task) (string, error) { return wtDir, nil }
	spawns := make(chan string, 2)
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		spawns <- cfg.TaskID
		return &agent.Agent{ID: "swept", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	a := &App{cfg: h.cfg, logger: slog.New(slog.DiscardHandler), humanReview: h}
	a.maintenancePass(context.Background())

	select {
	case <-spawns:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance tick does not sweep dropped reviews")
	}
	waitForHumanReviewRun(t, tasks, tk.ID)
}

// A context check alone loses the race: a goroutine can pass it microseconds
// before cancellation and still launch a provider process — measured at up to
// 945ms past app.draining, in 9 of 14 randomized SIGTERM trials. BeginDrain
// takes the lock the spawn holds across agents.Run, so the outcome is decided
// rather than raced.
func TestHumanReviewSpawn_BeginDrainRefusesEvenWithLiveContext(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("drain race", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}

	h.prepareTaskWorktree = func(task.Task) (string, error) { return t.TempDir(), nil }
	started := 0
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		started++
		return &agent.Agent{ID: "late", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	h.BeginDrain()

	// Context deliberately still live, as it is for a goroutine that got past
	// the ctx check just before schedulerCancel ran.
	if h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("spawned after BeginDrain")
	}
	if started != 0 {
		t.Errorf("started %d provider agents after drain, want 0", started)
	}
}

// A permanently-ineligible park was re-swept on every tick, writing a skip
// audit event each time — 21 no_project events in 200 seconds at a ten-second
// interval. The sweep refuses those locally instead.
func TestRespawnDroppedReviews_SkipsIneligibleWithoutAuditing(t *testing.T) {
	t.Parallel()
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	auditDir := t.TempDir()
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = auditLog.Close() })
	h.audit = auditLog

	for _, seed := range []struct {
		title  string
		fields map[string]any
	}{
		{"no project", map[string]any{"status": string(task.StatusHumanRequired)}},
		{"umbrella", map[string]any{
			"status": string(task.StatusHumanRequired), "project_id": "Automaat/sybra",
			"task_type": string(task.TaskTypeUmbrella),
		}},
	} {
		tk, err := tasks.Create(seed.title, "", task.AgentModeHeadless)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tasks.UpdateMap(tk.ID, seed.fields); err != nil {
			t.Fatal(err)
		}
	}

	h.agents = &fakeHumanReviewAgentRunner{run: func(agent.RunConfig) (*agent.Agent, error) {
		t.Error("sweep spawned a review for an ineligible task")
		return nil, errors.New("unreachable")
	}}

	for range 5 {
		h.RespawnDroppedReviews(context.Background())
	}

	events, err := audit.Read(auditDir, audit.Query{
		Since: time.Now().Add(-time.Minute),
		Until: time.Now().Add(time.Minute),
		Type:  audit.EventHumanReviewSkipped,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Errorf("sweep wrote %d skip events for permanently-ineligible tasks, want 0", len(events))
	}
}

// The handler gate is only reached if App.BeginDrain calls it, and it must be
// called before schedulerCancel — after it, the spawn goroutine may already
// have won the race the gate exists to decide.
func TestAppBeginDrain_RefusesHumanReviewSpawns(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	a := &App{cfg: h.cfg, logger: slog.New(slog.DiscardHandler), humanReview: h}
	a.lifecycle.Store(uint32(lifecycleStateRunning))

	if !a.BeginDrain() {
		t.Fatal("BeginDrain did not transition out of running")
	}

	h.drainMu.Lock()
	draining := h.draining
	h.drainMu.Unlock()
	if !draining {
		t.Error("App.BeginDrain did not close the human-review spawn gate")
	}
}

// A preparation can fail before it mutates anything — adoptWorktree refuses a
// detached or default-branch checkout up front — and the fallback then lands
// back on the task's own untouched worktree. Telling the agent it is "not
// looking at the state that failed" there makes it distrust a correct
// reproduction, which is the opposite of what the warning is for.
func TestHumanReviewDispatchDir_AdoptedFallbackIsNotFlaggedRebuilt(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	adopted := t.TempDir()
	h.resolveExistingWorktree = func(task.Task) (string, error) { return "", os.ErrInvalid }
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		return "", errors.New("adopt worktree: checked out on default branch")
	}

	dir, readOnly, _, rebuilt := h.dispatchDir(task.Task{ID: "t1", WorktreeDir: adopted})
	if dir != adopted || readOnly {
		t.Fatalf("got dir=%q readOnly=%v, want the task's own worktree", dir, readOnly)
	}
	if rebuilt {
		t.Error("rebuilt=true for an untouched worktree; the agent would distrust a correct reproduction")
	}
}

// Reuse skips PrepareForTask, and with it the ErrAgentRunning guard that #3108
// turned into a retry. A busy worktree must stay retryable, or the recovery
// agent edits a checkout the implementer is still committing into.
func TestHumanReviewDispatchDir_BusyWorktreeIsRetryable(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	h.resolveExistingWorktree = func(task.Task) (string, error) {
		return "", fmt.Errorf("resolve: %w", worktree.ErrWorktreeBusy)
	}
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		t.Fatal("preparation ran against a worktree with a live agent")
		return "", nil
	}

	dir, readOnly, retryable, _ := h.dispatchDir(task.Task{ID: "busy-task"})
	if !retryable {
		t.Errorf("retryable=false for a busy worktree; recovery falls back instead of waiting")
	}
	if dir != "" || readOnly {
		t.Errorf("got dir=%q readOnly=%v, want an empty retryable result", dir, readOnly)
	}
}

// The empty-dir guard is load-bearing: without it a resolver returning
// ("", nil) would give the agent an empty RunConfig.Dir and run it in the
// server process's own working directory.
func TestHumanReviewDispatchDir_EmptyReuseFallsThrough(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	prepared := t.TempDir()
	h.resolveExistingWorktree = func(task.Task) (string, error) { return "  ", nil }
	h.prepareTaskWorktree = func(task.Task) (string, error) { return prepared, nil }

	dir, _, _, _ := h.dispatchDir(task.Task{ID: "t1"})
	if dir != prepared {
		t.Errorf("dir = %q, want the prepared worktree %q", dir, prepared)
	}
}

// A preparation that sanitizes the tree and then fails non-retryably has
// destroyed the evidence more thoroughly than one that succeeded, so the
// read-only fallback still has to carry the warning.
func TestHumanReviewDispatchDir_FailedPrepareStillFlagsRebuilt(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	h.resolveExistingWorktree = func(task.Task) (string, error) { return "", os.ErrNotExist }
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		return "", errors.New("reconcile failed after sanitize")
	}

	dir, readOnly, _, rebuilt := h.dispatchDir(task.Task{ID: "t1"})
	if dir != h.cfg.HumanReview.SybraRepoDir || !readOnly {
		t.Fatalf("got dir=%q readOnly=%v, want the read-only fallback", dir, readOnly)
	}
	if !rebuilt {
		t.Error("rebuilt=false after a preparation that already mutated the tree")
	}
}

// With no usable checkout, preparation is unavoidable — but then the agent has
// to be told, because the mandate orders it to re-run the failing command and
// forbids inferring "flaky" from reasoning alone.
func TestHumanReviewDispatchDir_PreparedWorktreeIsFlaggedRebuilt(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	prepared := t.TempDir()
	h.resolveExistingWorktree = func(task.Task) (string, error) { return "", os.ErrNotExist }
	h.prepareTaskWorktree = func(task.Task) (string, error) { return prepared, nil }

	dir, _, _, rebuilt := h.dispatchDir(task.Task{ID: "parked-task"})
	if dir != prepared {
		t.Errorf("dir = %q, want %q", dir, prepared)
	}
	if !rebuilt {
		t.Fatal("rebuilt=false after preparation: the agent is never told its tree is not the tree that failed")
	}

	prompt := h.buildPrompt(context.Background(), task.Task{ID: "t1", ProjectID: "Automaat/sybra"}, dir, nil, rebuilt)
	if !strings.Contains(prompt, "not looking at the state that failed") {
		t.Error("prompt does not warn that the worktree was rebuilt")
	}
	clean := h.buildPrompt(context.Background(), task.Task{ID: "t1", ProjectID: "Automaat/sybra"}, dir, nil, false)
	if strings.Contains(clean, "not looking at the state that failed") {
		t.Error("prompt warns about a rebuild that did not happen")
	}
}

// The recovery agent is sent to explain a failure, and every Prepare* path
// rewrites the tree that failed before it reads a line of it — auto-committing
// the dirty state, resetting, and rebasing onto a fresher base. A healthy
// existing checkout must therefore be used untouched, or a base-induced
// failure stops reproducing and the agent reports "flaky" on manufactured
// evidence.
func TestHumanReviewDispatchDir_ReusesExistingWorktreeUnmutated(t *testing.T) {
	h, _, cleanup := newReviewTestEnv(t)
	defer cleanup()

	existing := t.TempDir()
	h.resolveExistingWorktree = func(task.Task) (string, error) { return existing, nil }
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		t.Fatal("preparation ran against a healthy existing worktree, rewriting the evidence")
		return "", nil
	}

	dir, readOnly, retryable, rebuilt := h.dispatchDir(task.Task{ID: "parked-task"})
	if dir != existing {
		t.Errorf("dir = %q, want the existing worktree %q", dir, existing)
	}
	if readOnly || retryable {
		t.Errorf("readOnly=%v retryable=%v, want both false", readOnly, retryable)
	}
	if rebuilt {
		t.Error("rebuilt=true for a reused worktree: the agent would be told its evidence is stale when it is not")
	}
}

// The provider-fallback retry and retryAfterCrash both build fresh spawn
// options, so carrying the flag on options alone lost it: the second agent was
// handed the tree the first one's preparation had already rewritten, with no
// warning. The flag is sticky for the whole human-required episode instead.
func TestHumanReviewSpawn_PreparedFlagIsStickyPerEpisode(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("sticky prepared", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}

	wtDir := t.TempDir()
	prepared := false
	h.resolveExistingWorktree = func(task.Task) (string, error) {
		if prepared {
			return wtDir, nil
		}
		return "", os.ErrNotExist
	}
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		prepared = true
		return wtDir, nil
	}

	var prompts []string
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		prompts = append(prompts, cfg.Prompt)
		return &agent.Agent{ID: fmt.Sprintf("rev%d", len(prompts)), TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}

	if !h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("first review did not spawn")
	}
	// The first agent finished, as it has when the provider fallback re-spawns.
	h.clearInflight(tk.ID)
	// A fresh options value, as the provider fallback and crash retry both build.
	if !h.maybeSpawnWithOptions(context.Background(), tk.ID, string(task.StatusInReview),
		humanReviewSpawnOptions{IgnoreRenderedVerdict: true, RetryReason: "malformed_verdict"}) {
		t.Fatal("retry did not spawn")
	}

	if len(prompts) != 2 {
		t.Fatalf("got %d prompts, want 2", len(prompts))
	}
	for i, p := range prompts {
		if !strings.Contains(p, "not looking at the state that failed") {
			t.Errorf("prompt %d reused the rewritten tree without warning the agent", i)
		}
	}

	// A later episode must not inherit the warning.
	h.clearInflight(tk.ID)
	h.forgetPrepared(tk.ID)
	h.mu.Lock()
	h.perTask = map[string][]time.Time{}
	h.recent = nil
	h.mu.Unlock()
	prompts = nil
	if !h.maybeSpawnWithOptions(context.Background(), tk.ID, string(task.StatusInReview),
		humanReviewSpawnOptions{IgnoreRenderedVerdict: true}) {
		t.Fatal("third spawn refused")
	}
	if len(prompts) != 1 {
		t.Fatalf("third spawn produced %d prompts, want 1", len(prompts))
	}
	if strings.Contains(prompts[0], "not looking at the state that failed") {
		t.Error("a new episode inherited a warning about a preparation that predates it")
	}
}

// A transient fetch failure lands after SanitizeWorktree has auto-committed
// and reset the tree, so the retry reuses a checkout that no longer holds the
// evidence. Without carrying the flag across the retry it is reported as
// untouched — the exact false reassurance the warning exists to prevent.
func TestHumanReviewSpawn_PreparedFlagSurvivesRetry(t *testing.T) {
	h, tasks, cleanup := newReviewTestEnv(t)
	defer cleanup()

	tk, err := tasks.Create("transient prepare", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.UpdateMap(tk.ID, map[string]any{
		"project_id": "Automaat/sybra",
		"status":     string(task.StatusHumanRequired),
	}); err != nil {
		t.Fatal(err)
	}

	wtDir := t.TempDir()
	prepared := false
	// First pass: nothing to reuse, and preparation fails transiently after
	// having already sanitized. Second pass: the sanitized tree is reusable.
	h.resolveExistingWorktree = func(task.Task) (string, error) {
		if prepared {
			return wtDir, nil
		}
		return "", os.ErrNotExist
	}
	h.prepareTaskWorktree = func(task.Task) (string, error) {
		prepared = true
		return "", fmt.Errorf("fetch: %w", worktree.ErrTransientFetch)
	}

	var prompt string
	h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
		prompt = cfg.Prompt
		return &agent.Agent{ID: "rev", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
	}}
	// Run the real scheduled retry rather than reconstructing its options, so
	// the flag has to survive the actual handoff.
	var retry func()
	h.schedule = func(_ time.Duration, fn func()) { retry = fn }

	if h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
		t.Fatal("spawned despite a retryable preparation failure")
	}
	if retry == nil {
		t.Fatal("no retry scheduled after a retryable preparation failure")
	}
	retry()

	if prompt == "" {
		t.Fatal("retry did not spawn")
	}
	if !strings.Contains(prompt, "not looking at the state that failed") {
		t.Error("retry reused a sanitized worktree without warning the agent")
	}
}

// dispatchDir computing the flag correctly is worth nothing if spawnReview
// drops it. Assert against the prompt the agent is actually handed.
func TestHumanReviewSpawn_RebuiltWarningReachesTheAgent(t *testing.T) {
	cases := []struct {
		name        string
		reuse       bool
		wantWarning bool
	}{
		{name: "reused worktree carries no warning", reuse: true},
		{name: "prepared worktree carries the warning", wantWarning: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, tasks, cleanup := newReviewTestEnv(t)
			defer cleanup()

			tk, err := tasks.Create("rebuilt warning", "", task.AgentModeHeadless)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tasks.UpdateMap(tk.ID, map[string]any{
				"project_id": "Automaat/sybra",
				"status":     string(task.StatusHumanRequired),
			}); err != nil {
				t.Fatal(err)
			}

			wtDir := t.TempDir()
			h.resolveExistingWorktree = func(task.Task) (string, error) {
				if tc.reuse {
					return wtDir, nil
				}
				return "", os.ErrNotExist
			}
			h.prepareTaskWorktree = func(task.Task) (string, error) { return wtDir, nil }

			var prompt string
			h.agents = &fakeHumanReviewAgentRunner{run: func(cfg agent.RunConfig) (*agent.Agent, error) {
				prompt = cfg.Prompt
				return &agent.Agent{ID: "rev", TaskID: cfg.TaskID, StartedAt: time.Now().UTC()}, nil
			}}

			if !h.maybeSpawn(context.Background(), tk.ID, string(task.StatusInReview)) {
				t.Fatal("review did not spawn")
			}
			got := strings.Contains(prompt, "not looking at the state that failed")
			if got != tc.wantWarning {
				t.Errorf("prompt warns = %v, want %v", got, tc.wantWarning)
			}
		})
	}
}
