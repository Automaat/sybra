package sybra

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// mechanicalPRFixYAML is a pr.event workflow with no run_agent step, so
// DispatchEvent completes without spawning an agent — enough to exercise the
// coalescing dispatch bookkeeping.
const mechanicalPRFixYAML = `id: test-pr-fix
name: Test PR Fix Coalesce
trigger:
  on: pr.event
  conditions:
    - field: pr.issue_kind
      operator: in
      value: conflict,ci_failure,comments
steps:
  - id: mark
    name: Mark In Progress
    type: set_status
    config:
      status: in-progress
    next:
      - goto: ""
`

// TestCoalescedFixPrompt locks the composition: a single issue yields its bare
// per-kind body (unchanged behavior), while multiple issues from one push are
// stitched into a single agent prompt covering every kind under one header.
func TestCoalescedFixPrompt(t *testing.T) {
	t.Parallel()
	pr := github.PullRequest{Number: 7, Repository: "o/r", HeadRefName: "feat", URL: "https://github.com/o/r/pull/7"}

	single := coalescedFixPrompt([]github.PRIssue{{Kind: github.PRIssueComments, PR: pr}}, "")
	if strings.Contains(single, "multiple open issues") {
		t.Errorf("single-issue prompt must not carry the coalesced header:\n%s", single)
	}
	if !strings.Contains(single, "/fix-review") {
		t.Errorf("single comments prompt missing its body:\n%s", single)
	}

	multi := coalescedFixPrompt([]github.PRIssue{
		{Kind: github.PRIssueCIFailure, PR: pr},
		{Kind: github.PRIssueComments, PR: pr},
	}, "")
	for _, want := range []string{"multiple open issues", "Failing CI", "Review comments", "/fix-review", "gh run view"} {
		if !strings.Contains(multi, want) {
			t.Errorf("coalesced prompt missing %q:\n%s", want, multi)
		}
	}
}

// TestDispatchFixIssues_ReviewHoldSetsParkVar is the regression guard for the
// blocking review finding: relying on the prompt sentinel is unsafe because
// dispatchPRIssue appends prFixResultContract AFTER the hold suffix, and in push
// mode the agent pushes and emits `continue`, which classifyPRFixResult would
// pick as the last sentinel. So parking must be deterministic — a review-hold +
// comments dispatch sets the ReviewHoldParkVar workflow var that
// route_pr_fix_result honors regardless of agent output.
func TestDispatchFixIssues_ReviewHoldSetsParkVar(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfStore.Dir(), "test-pr-fix.yaml"),
		[]byte(mechanicalPRFixYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	engine := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agentMgr, tasks: tasks},
		logger,
	)

	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:   task.Ptr(task.StatusInReview),
		PRNumber: task.Ptr(1446),
	}); err != nil {
		t.Fatal(err)
	}

	r := &ReviewHandler{
		DomainHandler:  DomainHandler{logger: logger},
		tasks:          tasks,
		agents:         agentMgr,
		prTracker:      github.NewIssueTracker(time.Minute),
		workflowEngine: engine,
		// push mode: the agent pushes and would emit `continue`; the park var
		// must still force human-required.
		cfg: &config.Config{ReviewHold: config.ReviewHoldConfig{Enabled: true, Mode: config.ReviewHoldModePush}},
	}

	pr := github.PullRequest{
		Number: 1446, Repository: "o/r", HeadRefName: "feat", HeadSHA: "sha1",
		URL: "https://github.com/o/r/pull/1446", FeedbackSig: "sig",
	}
	r.handleMatchedPRIssues(context.Background(), []github.PRIssue{
		{Kind: github.PRIssueComments, TaskID: created.ID, PR: pr},
	})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched")
	}
	if v := got.Workflow.Variables[workflow.ReviewHoldParkVar]; v != "true" {
		t.Errorf("%s = %q, want \"true\" (deterministic park backstop must be set)", workflow.ReviewHoldParkVar, v)
	}
}

// TestCoalescedFixPrompt_ReviewHold locks how the review-hold suffix is grafted
// on: appended once (at the end, after the reply-live instructions it overrides)
// when the set includes a comments issue, and suppressed entirely when it does
// not — a lone CI/conflict fix posts no replies, so it must stay unchanged.
func TestCoalescedFixPrompt_ReviewHold(t *testing.T) {
	t.Parallel()
	pr := github.PullRequest{Number: 7, Repository: "o/r", HeadRefName: "feat", URL: "https://github.com/o/r/pull/7"}
	const suffix = "\n\n---\nREVIEW HOLD sentinel"

	withComments := coalescedFixPrompt([]github.PRIssue{{Kind: github.PRIssueComments, PR: pr}}, suffix)
	if !strings.HasSuffix(withComments, suffix) {
		t.Errorf("comments prompt must end with the hold suffix:\n%s", withComments)
	}

	noComments := coalescedFixPrompt([]github.PRIssue{{Kind: github.PRIssueCIFailure, PR: pr}}, suffix)
	if strings.Contains(noComments, "REVIEW HOLD") {
		t.Errorf("a non-comments fix posts no replies, so it must not carry the hold suffix:\n%s", noComments)
	}

	// Disabled hold (empty suffix) leaves a comments prompt byte-for-byte as-is.
	plain := coalescedFixPrompt([]github.PRIssue{{Kind: github.PRIssueComments, PR: pr}}, "")
	if strings.Contains(plain, "REVIEW HOLD") {
		t.Errorf("empty suffix must not alter the prompt:\n%s", plain)
	}
}

// TestHandleMatchedPRIssues_CoalescesFixIssues is the regression guard for the
// double pr-fix (PR #1352): a single poll surfacing both a ci_failure and
// review comments for one PR must dispatch ONE fix agent that addresses both,
// not two sequential agents across cycles. Under the old per-kind loop the
// comments issue was blocked by HasActiveWorkflow after ci_failure dispatched,
// so it re-fired as a separate agent on the next cycle.
func TestHandleMatchedPRIssues_CoalescesFixIssues(t *testing.T) {
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfStore.Dir(), "test-pr-fix.yaml"),
		[]byte(mechanicalPRFixYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	agentMgr := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	engine := workflow.NewEngine(
		wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agentMgr, tasks: tasks},
		logger,
	)

	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	// ProjectID left empty so prepareWorktree short-circuits (no worktree).
	if _, err := tasks.Update(created.ID, task.Update{
		Status:   task.Ptr(task.StatusInReview),
		PRNumber: task.Ptr(1352),
	}); err != nil {
		t.Fatal(err)
	}

	r := &ReviewHandler{
		DomainHandler:  DomainHandler{logger: logger},
		tasks:          tasks,
		agents:         agentMgr,
		prTracker:      github.NewIssueTracker(time.Minute),
		workflowEngine: engine,
	}

	pr := github.PullRequest{
		Number: 1352, Repository: "o/r", HeadRefName: "feat", HeadSHA: "sha1",
		URL: "https://github.com/o/r/pull/1352", FeedbackSig: "sig",
	}
	// Same order MatchTaskPRs emits: ci_failure before comments.
	issues := []github.PRIssue{
		{Kind: github.PRIssueCIFailure, TaskID: created.ID, PR: pr},
		{Kind: github.PRIssueComments, TaskID: created.ID, PR: pr},
	}

	r.handleMatchedPRIssues(context.Background(), issues)

	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFailure); got != 1 {
		t.Errorf("ci_failure retries = %d, want 1", got)
	}
	if got := r.prTracker.Retries(created.ID, github.PRIssueComments); got != 1 {
		t.Errorf("comments retries = %d, want 1 (coalesced dispatch must mark it handled)", got)
	}

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no workflow dispatched")
	}
	// comments outranks ci_failure for the primary kind (branch-preserving
	// worktree prep + cancel/phase reconciliation key on it).
	if k := got.Workflow.Variables["pr_issue_kind"]; k != string(github.PRIssueComments) {
		t.Errorf("pr_issue_kind = %q, want %q (primary)", k, github.PRIssueComments)
	}
	prompt := got.Workflow.Variables["prompt"]
	if !strings.Contains(prompt, "Failing CI") || !strings.Contains(prompt, "Review comments") {
		t.Errorf("coalesced prompt missing a section:\n%s", prompt)
	}
}
