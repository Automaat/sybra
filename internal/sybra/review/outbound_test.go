package review

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/poll"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

// newOutboundTestHandler builds a Handler backed by a temp task store and
// a real (idle) agent manager — enough to exercise the PR-phase reconciler.
func newOutboundTestHandler(t *testing.T) (*Handler, *task.Manager) {
	t.Helper()
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	r := &Handler{
		logger: logger,
		tasks:  tasks,
		agents: agents,
	}
	return r, tasks
}

func newOutboundWorkflowTestHandler(t *testing.T) (*Handler, *task.Manager) {
	t.Helper()
	tmp := t.TempDir()
	store, err := task.NewStore(filepath.Join(tmp, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)
	logger := slog.New(slog.DiscardHandler)
	agents := newTestAgentManager(t, t.Context(), func(string, any) {}, logger, t.TempDir())
	wfStore, err := workflow.NewStore(filepath.Join(tmp, "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.SyncBuiltins(wfStore); err != nil {
		t.Fatal(err)
	}
	projects, err := project.NewStore(filepath.Join(tmp, "projects"), filepath.Join(tmp, "clones"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projects.CreateMeta("https://github.com/Automaat/sybra", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(wfStore,
		&taskAdapter{tasks: tasks},
		&agentAdapter{agents: agents, tasks: tasks},
		logger,
	)
	r := &Handler{
		logger:         logger,
		tasks:          tasks,
		projects:       projects,
		agents:         agents,
		WorkflowEngine: engine,
	}
	return r, tasks
}

// mkOwnPRTask creates a task and drives it to in-review with a linked PR.
func mkOwnPRTask(t *testing.T, tasks *task.Manager, prNumber int, tags []string) task.Task {
	t.Helper()
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		PRNumber:  task.Ptr(prNumber),
		ProjectID: task.Ptr("Automaat/sybra"),
		Tags:      &tags,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return updated
}

func TestCancelSettledImplementationWorkflows(t *testing.T) {
	t.Run("linked green pr cancels stale implement workflow", func(t *testing.T) {
		r, tasks := newOutboundWorkflowTestHandler(t)

		created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatal(err)
		}
		wf := &workflow.Execution{
			WorkflowID:  "simple-task-implement",
			CurrentStep: "implement",
			State:       workflow.ExecWaiting,
		}
		reason := "watchdog hang: no stream activity"
		if _, err := tasks.Update(created.ID, task.Update{
			Status:       task.Ptr(task.StatusInProgress),
			StatusReason: &reason,
			ProjectID:    task.Ptr("Automaat/sybra"),
			PRNumber:     task.Ptr(42),
			Branch:       task.Ptr("feat/watchdog-pr"),
			Workflow:     &wf,
		}); err != nil {
			t.Fatal(err)
		}

		all, err := tasks.List()
		if err != nil {
			t.Fatal(err)
		}
		r.cancelSettledImplementationWorkflows(all, []github.PullRequest{{
			Number:      42,
			Repository:  "Automaat/sybra",
			HeadRefName: "feat/watchdog-pr",
			Mergeable:   "MERGEABLE",
			CIStatus:    "SUCCESS",
		}})

		got, err := tasks.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != task.StatusInReview {
			t.Fatalf("status = %q, want in-review", got.Status)
		}
		if got.StatusReason != "" {
			t.Fatalf("statusReason = %q, want cleared", got.StatusReason)
		}
		if got.Workflow == nil || got.Workflow.State != workflow.ExecCompleted || got.Workflow.CurrentStep != "" {
			t.Fatalf("workflow = %+v, want completed with empty current step", got.Workflow)
		}
	})

	t.Run("branch matched green pr adopts number before cancel", func(t *testing.T) {
		r, tasks := newOutboundWorkflowTestHandler(t)

		created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatal(err)
		}
		wf := &workflow.Execution{
			WorkflowID:  "simple-task-implement",
			CurrentStep: "implement",
			State:       workflow.ExecWaiting,
		}
		if _, err := tasks.Update(created.ID, task.Update{
			Status:    task.Ptr(task.StatusInProgress),
			ProjectID: task.Ptr("Automaat/sybra"),
			Branch:    task.Ptr("feat/branch-only"),
			Workflow:  &wf,
		}); err != nil {
			t.Fatal(err)
		}

		all, err := tasks.List()
		if err != nil {
			t.Fatal(err)
		}
		r.cancelSettledImplementationWorkflows(all, []github.PullRequest{{
			Number:      77,
			Repository:  "Automaat/sybra",
			HeadRefName: "feat/branch-only",
			Mergeable:   "MERGEABLE",
			CIStatus:    "SUCCESS",
		}})

		got, err := tasks.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PRNumber != 77 {
			t.Fatalf("PRNumber = %d, want 77", got.PRNumber)
		}
		if got.Status != task.StatusInReview {
			t.Fatalf("status = %q, want in-review", got.Status)
		}
		if got.Workflow == nil || got.Workflow.State != workflow.ExecCompleted {
			t.Fatalf("workflow = %+v, want completed", got.Workflow)
		}
	})

	t.Run("pending checks keep implement workflow live", func(t *testing.T) {
		r, tasks := newOutboundWorkflowTestHandler(t)

		created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatal(err)
		}
		wf := &workflow.Execution{
			WorkflowID:  "simple-task-implement",
			CurrentStep: "implement",
			State:       workflow.ExecWaiting,
		}
		reason := "watchdog hang: no stream activity"
		if _, err := tasks.Update(created.ID, task.Update{
			Status:       task.Ptr(task.StatusInProgress),
			StatusReason: &reason,
			ProjectID:    task.Ptr("Automaat/sybra"),
			PRNumber:     task.Ptr(55),
			Branch:       task.Ptr("feat/not-green"),
			Workflow:     &wf,
		}); err != nil {
			t.Fatal(err)
		}

		all, err := tasks.List()
		if err != nil {
			t.Fatal(err)
		}
		r.cancelSettledImplementationWorkflows(all, []github.PullRequest{{
			Number:           55,
			Repository:       "Automaat/sybra",
			HeadRefName:      "feat/not-green",
			Mergeable:        "MERGEABLE",
			CIStatus:         "PENDING",
			HasPendingChecks: true,
		}})

		got, err := tasks.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != task.StatusInProgress {
			t.Fatalf("status = %q, want in-progress", got.Status)
		}
		if got.StatusReason != reason {
			t.Fatalf("statusReason = %q, want %q", got.StatusReason, reason)
		}
		if got.Workflow == nil || got.Workflow.State != workflow.ExecWaiting || got.Workflow.CurrentStep != "implement" {
			t.Fatalf("workflow = %+v, want waiting on implement", got.Workflow)
		}
	})

	t.Run("REST unfetched CI keeps implement workflow live", func(t *testing.T) {
		r, tasks := newOutboundWorkflowTestHandler(t)

		created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
		if err != nil {
			t.Fatal(err)
		}
		wf := &workflow.Execution{
			WorkflowID:  "simple-task-implement",
			CurrentStep: "implement",
			State:       workflow.ExecWaiting,
		}
		reason := "watchdog hang: no stream activity"
		if _, err := tasks.Update(created.ID, task.Update{
			Status:       task.Ptr(task.StatusInProgress),
			StatusReason: &reason,
			ProjectID:    task.Ptr("Automaat/sybra"),
			PRNumber:     task.Ptr(66),
			Branch:       task.Ptr("feat/rest-ci-unknown"),
			Workflow:     &wf,
		}); err != nil {
			t.Fatal(err)
		}

		all, err := tasks.List()
		if err != nil {
			t.Fatal(err)
		}
		r.cancelSettledImplementationWorkflows(all, []github.PullRequest{{
			Number:         66,
			Repository:     "Automaat/sybra",
			HeadRefName:    "feat/rest-ci-unknown",
			Mergeable:      "MERGEABLE",
			SourcedViaREST: true,
			RESTCIFetched:  false,
			CIStatus:       "",
		}})

		got, err := tasks.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != task.StatusInProgress {
			t.Fatalf("status = %q, want in-progress", got.Status)
		}
		if got.StatusReason != reason {
			t.Fatalf("statusReason = %q, want %q", got.StatusReason, reason)
		}
		if got.Workflow == nil || got.Workflow.State != workflow.ExecWaiting || got.Workflow.CurrentStep != "implement" {
			t.Fatalf("workflow = %+v, want waiting on implement", got.Workflow)
		}
	})
}

func TestPollKnownTaskPRs_CancelsBranchOnlySettledImplementationWorkflow(t *testing.T) {
	r, tasks := newOutboundWorkflowTestHandler(t)
	r.authCircuit = poll.NewAuthCircuit("reviews", r.logger)
	r.cfg = &config.Config{GitHub: config.GitHubConfig{PollerRole: "secondary"}}

	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       workflow.ExecWaiting,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInProgress),
		ProjectID: task.Ptr("Automaat/sybra"),
		Branch:    task.Ptr("feat/branch-only-secondary"),
		Workflow:  &wf,
	}); err != nil {
		t.Fatal(err)
	}

	r.findOpenPRForBranchFn = func(_ context.Context, repo, branch string) (int, bool, error) {
		if repo != "Automaat/sybra" || branch != "feat/branch-only-secondary" {
			t.Fatalf("find branch = %s %s, want Automaat/sybra feat/branch-only-secondary", repo, branch)
		}
		return 77, true, nil
	}
	var fetched []github.PRRef
	r.fetchKnownPRsFn = func(refs []github.PRRef) []github.MonitorPRResult {
		fetched = refs
		results := make([]github.MonitorPRResult, len(refs))
		for i, ref := range refs {
			results[i] = github.MonitorPRResult{
				Repo: ref.Repo, Number: ref.Number, Open: true,
				PR: github.PullRequest{
					Number:      ref.Number,
					Repository:  ref.Repo,
					HeadRefName: "feat/branch-only-secondary",
					Mergeable:   "MERGEABLE",
					CIStatus:    "SUCCESS",
				},
			}
		}
		return results
	}

	r.Poll(context.Background())

	if len(fetched) != 1 || fetched[0].Repo != "Automaat/sybra" || fetched[0].Number != 77 {
		t.Fatalf("fetched refs = %+v, want Automaat/sybra#77", fetched)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRNumber != 77 {
		t.Fatalf("PRNumber = %d, want 77", got.PRNumber)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", got.Status)
	}
	if got.Workflow == nil || got.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", got.Workflow)
	}
}

func TestHandleKnownPRConflictsViaREST_CancelsBranchOnlySettledImplementationWorkflow(t *testing.T) {
	r, tasks := newOutboundWorkflowTestHandler(t)
	r.authCircuit = poll.NewAuthCircuit("reviews", r.logger)

	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-implement",
		CurrentStep: "implement",
		State:       workflow.ExecWaiting,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInProgress),
		ProjectID: task.Ptr("Automaat/sybra"),
		Branch:    task.Ptr("feat/branch-only-rest"),
		Workflow:  &wf,
	}); err != nil {
		t.Fatal(err)
	}

	r.findOpenPRForBranchFn = func(_ context.Context, repo, branch string) (int, bool, error) {
		if repo != "Automaat/sybra" || branch != "feat/branch-only-rest" {
			t.Fatalf("find branch = %s %s, want Automaat/sybra feat/branch-only-rest", repo, branch)
		}
		return 88, true, nil
	}
	var fetched []github.PRRef
	r.fetchKnownPRsFn = func(refs []github.PRRef) []github.MonitorPRResult {
		fetched = refs
		results := make([]github.MonitorPRResult, len(refs))
		for i, ref := range refs {
			results[i] = github.MonitorPRResult{
				Repo: ref.Repo, Number: ref.Number, Open: true,
				PR: github.PullRequest{
					Number:      ref.Number,
					Repository:  ref.Repo,
					HeadRefName: "feat/branch-only-rest",
					Mergeable:   "MERGEABLE",
					CIStatus:    "SUCCESS",
				},
			}
		}
		return results
	}
	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}

	r.handleKnownPRConflictsViaREST(context.Background(), all)

	if len(fetched) != 1 || fetched[0].Repo != "Automaat/sybra" || fetched[0].Number != 88 {
		t.Fatalf("fetched refs = %+v, want Automaat/sybra#88", fetched)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRNumber != 88 {
		t.Fatalf("PRNumber = %d, want 88", got.PRNumber)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", got.Status)
	}
	if got.Workflow == nil || got.Workflow.State != workflow.ExecCompleted {
		t.Fatalf("workflow = %+v, want completed", got.Workflow)
	}
}

func TestReconcilePRPhases(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	own := mkOwnPRTask(t, tasks, 42, nil)
	review := mkOwnPRTask(t, tasks, 7, []string{"review"})

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{
		{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"},
		{Number: 7, ReviewDecision: "CHANGES_REQUESTED"},
	}
	r.reconcilePRPhases(all, prs)

	got, err := tasks.Get(own.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRPhase != PRPhaseApproved {
		t.Errorf("own PR phase = %q, want %q", got.PRPhase, PRPhaseApproved)
	}

	// A review-tagged task is inbound — the outbound reconciler must skip it so
	// it stays driven by ReviewPhase, not PRPhase.
	gotReview, err := tasks.Get(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotReview.PRPhase != "" {
		t.Errorf("review task PRPhase = %q, want empty", gotReview.PRPhase)
	}
}

func TestReconcilePRPhasesClearsStaleWhenIneligible(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	if _, err := tasks.Update(created.ID, task.Update{
		PRPhase: task.Ptr(PRPhaseAwaitingApproval),
		Status:  task.Ptr(task.StatusInProgress),
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRPhase != "" {
		t.Errorf("stale PRPhase = %q, want cleared", got.PRPhase)
	}
}

func TestLinkedOwnPRHumanRequiredDrift(t *testing.T) {
	completedAt := time.Now().UTC()
	ts := completedAt.Add(-time.Minute)
	drifted := &task.Task{
		Status:       task.StatusHumanRequired,
		PRNumber:     42,
		UpdatedAt:    ts,
		StatusReason: "",
		Workflow: &workflow.Execution{
			WorkflowID:  "simple-task-pr",
			State:       workflow.ExecCompleted,
			CompletedAt: &completedAt,
		},
	}
	if !linkedOwnPRHumanRequiredDrift(drifted, true) {
		t.Fatal("expected linked PR drift")
	}
}

func TestReconcilePRPhasesDoesNotReactivateFreshManualHumanRequired(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	completedAt := time.Now().UTC().Add(-500 * time.Millisecond)
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &completedAt,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(""),
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func TestReconcilePRPhasesDoesNotReactivateReasonedHumanRequired(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	reason := "testing infrastructure failed after retry"
	now := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &now,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: &reason,
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
	if got.PRPhase != "" {
		t.Errorf("phase = %q, want unchanged empty", got.PRPhase)
	}
}

func TestReconcilePRPhasesDoesNotReactivateLaterManualHumanRequired(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	completedAt := time.Now().UTC().Add(-5 * time.Minute)
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &completedAt,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(""),
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func TestReconcilePRPhasesDoesNotReactivateWithoutLivePR(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)

	created := mkOwnPRTask(t, tasks, 42, nil)
	now := time.Now().UTC()
	wf := &workflow.Execution{
		WorkflowID:  "simple-task-pr",
		State:       workflow.ExecCompleted,
		CompletedAt: &now,
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(""),
		Workflow:     &wf,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, nil)

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want human-required", got.Status)
	}
}

func TestExhaustedFixReasonKind(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   github.PRIssueKind
		wantOK bool
	}{
		{"ci_failure", "pr-monitor: auto-fix exhausted after 3 attempts (ci_failure) — needs a human", github.PRIssueCIFailure, true},
		{"conflict", "pr-monitor: auto-fix exhausted after 3 attempts (conflict) — needs a human", github.PRIssueConflict, true},
		{"empty", "", "", false},
		{"unrelated reason", "DCO check failing — needs a human to amend history", "", false},
		{"missing parens", "pr-monitor: auto-fix exhausted after 3 attempts — needs a human", "", false},
		{"empty parens", "pr-monitor: auto-fix exhausted after 3 attempts () — needs a human", "", false},
		{"extra parens after prefix", "pr-monitor: auto-fix exhausted after 3 attempts blah (ci_failure) — needs a human", "", false},
		{"prefix with unrelated suffix", "pr-monitor: auto-fix exhausted after 3 attempts (ci_failure) and then (comments)", "", false},
		{"non-numeric attempts", "pr-monitor: auto-fix exhausted after three attempts (ci_failure) — needs a human", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := exhaustedFixReasonKind(tt.reason)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("exhaustedFixReasonKind(%q) = (%q, %v), want (%q, %v)", tt.reason, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestExhaustedFixReasonRoundTrip(t *testing.T) {
	kinds := []github.PRIssueKind{
		github.PRIssueCIFailure,
		github.PRIssueConflict,
		github.PRIssueComments,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			got, ok := exhaustedFixReasonKind(exhaustedFixReason(3, kind))
			if !ok || got != kind {
				t.Errorf("round-trip of %q = (%q, %v), want (%q, true)", kind, got, ok, kind)
			}
		})
	}
}

func TestHumanRequiredBlockerReconcilable(t *testing.T) {
	ciReason := exhaustedFixReason(3, github.PRIssueCIFailure)
	conflictReason := exhaustedFixReason(3, github.PRIssueConflict)
	tests := []struct {
		name     string
		task     *task.Task
		wantKind github.PRIssueKind
		wantOK   bool
	}{
		{"eligible ci_failure exhaustion", &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason}, github.PRIssueCIFailure, true},
		{"eligible conflict exhaustion", &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: conflictReason}, github.PRIssueConflict, true},
		{"not human-required", &task.Task{Status: task.StatusInReview, PRNumber: 42, StatusReason: ciReason}, "", false},
		{"no PR linked", &task.Task{Status: task.StatusHumanRequired, StatusReason: ciReason}, "", false},
		{"draft review reason requires a human", &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: "Draft review ready — verify & submit on GitHub"}, "", false},
		{"comments exhaustion needs a human", &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: exhaustedFixReason(3, github.PRIssueComments)}, "", false},
		{"review-tagged task is inbound", &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason, Tags: []string{"review"}}, "", false},
		{"latched task does not re-reconcile", &task.Task{Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason, Tags: []string{reconciledLatchTag}}, "", false},
		{"chat task never own-PR", &task.Task{TaskType: task.TaskTypeChat, Status: task.StatusHumanRequired, PRNumber: 42, StatusReason: ciReason}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKind, gotOK := humanRequiredBlockerReconcilable(tt.task)
			if gotOK != tt.wantOK || gotKind != tt.wantKind {
				t.Errorf("humanRequiredBlockerReconcilable() = (%q, %v), want (%q, %v)", gotKind, gotOK, tt.wantKind, tt.wantOK)
			}
		})
	}
}

func TestHumanRequiredBlockerReconcileEligible(t *testing.T) {
	if !humanRequiredBlockerReconcileEligible(&task.Task{
		Status:       task.StatusHumanRequired,
		PRNumber:     42,
		StatusReason: exhaustedFixReason(3, github.PRIssueCIFailure),
	}) {
		t.Fatal("expected ci_failure exhaustion to be eligible")
	}
	if !humanRequiredBlockerReconcileEligible(&task.Task{
		Status:       task.StatusHumanRequired,
		PRNumber:     42,
		StatusReason: exhaustedFixReason(3, github.PRIssueConflict),
	}) {
		t.Fatal("expected conflict exhaustion to be eligible")
	}
}

func mkHumanRequiredBlockerTask(t *testing.T, tasks *task.Manager, prNumber int, reason string, tags []string) task.Task {
	t.Helper()
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		PRNumber:     task.Ptr(prNumber),
		ProjectID:    task.Ptr("Automaat/sybra"),
		Tags:         &tags,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return updated
}

func prStateFromJSON(raw string) github.PRState {
	var s github.PRState
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		panic(err)
	}
	return s
}

func openMergeableGreenPR() github.PRState {
	return prStateFromJSON(`{
		"state": "OPEN",
		"mergeable": "MERGEABLE",
		"statusCheckRollup": [{"__typename": "StatusContext", "name": "ci", "state": "SUCCESS"}]
	}`)
}

func openMergeablePendingPR() github.PRState {
	return prStateFromJSON(`{
		"state": "OPEN",
		"mergeable": "MERGEABLE",
		"statusCheckRollup": [{"__typename": "StatusContext", "name": "ci", "state": "PENDING"}]
	}`)
}

func openMergeableFailedPR() github.PRState {
	return prStateFromJSON(`{
		"state": "OPEN",
		"mergeable": "MERGEABLE",
		"statusCheckRollup": [{"__typename": "StatusContext", "name": "ci", "state": "FAILURE"}]
	}`)
}

func openMergeableNoChecksPR() github.PRState {
	return prStateFromJSON(`{"state": "OPEN", "mergeable": "MERGEABLE", "statusCheckRollup": []}`)
}

func closedPR() github.PRState {
	return prStateFromJSON(`{"state": "CLOSED", "mergeable": "MERGEABLE"}`)
}

func TestReconcileHumanRequiredBlockersFallbackProbe(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		tags       []string
		fetchState func(repo string, number int) (github.PRState, error)
		wantStatus task.Status
		wantLatch  bool
	}{
		{"ci_failure cleared -> flips to in-review", exhaustedFixReason(3, github.PRIssueCIFailure), nil, func(string, int) (github.PRState, error) { return openMergeableGreenPR(), nil }, task.StatusInReview, true},
		{"CI infra rerun permission cleared -> flips to in-review", ciInfraRerunPermissionReason, nil, func(string, int) (github.PRState, error) { return openMergeableGreenPR(), nil }, task.StatusInReview, true},
		{"conflict cleared -> flips to in-review", exhaustedFixReason(3, github.PRIssueConflict), nil, func(string, int) (github.PRState, error) { return openMergeableGreenPR(), nil }, task.StatusInReview, true},
		{"CI pending -> stays parked", exhaustedFixReason(3, github.PRIssueCIFailure), nil, func(string, int) (github.PRState, error) { return openMergeablePendingPR(), nil }, task.StatusHumanRequired, false},
		{"CI unknown/empty -> stays parked", exhaustedFixReason(3, github.PRIssueCIFailure), nil, func(string, int) (github.PRState, error) { return openMergeableNoChecksPR(), nil }, task.StatusHumanRequired, false},
		{"CI failed -> stays parked", exhaustedFixReason(3, github.PRIssueCIFailure), nil, func(string, int) (github.PRState, error) { return openMergeableFailedPR(), nil }, task.StatusHumanRequired, false},
		{"PR closed -> stays parked", exhaustedFixReason(3, github.PRIssueCIFailure), nil, func(string, int) (github.PRState, error) { return closedPR(), nil }, task.StatusHumanRequired, false},
		{"fetch error -> stays parked", exhaustedFixReason(3, github.PRIssueCIFailure), nil, func(string, int) (github.PRState, error) { return github.PRState{}, errors.New("boom") }, task.StatusHumanRequired, false},
		{"human-authored reason -> skipped", "please double check the migration by hand", nil, func(string, int) (github.PRState, error) {
			t.Fatal("must not probe a human-authored reason")
			return github.PRState{}, nil
		}, task.StatusHumanRequired, false},
		{"watchdog reason -> skipped", "watchdog: rate limit", nil, func(string, int) (github.PRState, error) {
			t.Fatal("must not probe a watchdog reason")
			return github.PRState{}, nil
		}, task.StatusHumanRequired, false},
		{"tamper-flagged reason -> skipped", workflow.TamperFlaggedReasonPrefix + " tests/foo_test.go", nil, func(string, int) (github.PRState, error) {
			t.Fatal("must not probe a tamper-flagged reason")
			return github.PRState{}, nil
		}, task.StatusHumanRequired, false},
		{"comments exhaustion -> skipped", exhaustedFixReason(3, github.PRIssueComments), nil, func(string, int) (github.PRState, error) {
			t.Fatal("must not probe comments exhaustion")
			return github.PRState{}, nil
		}, task.StatusHumanRequired, false},
		{"already latched -> skipped", exhaustedFixReason(3, github.PRIssueCIFailure), []string{reconciledLatchTag}, func(string, int) (github.PRState, error) {
			t.Fatal("must not re-probe a latched task")
			return github.PRState{}, nil
		}, task.StatusHumanRequired, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, tasks := newOutboundTestHandler(t)
			r.fetchPRStateFn = tt.fetchState
			created := mkHumanRequiredBlockerTask(t, tasks, 42, tt.reason, tt.tags)

			all, err := tasks.List()
			if err != nil {
				t.Fatal(err)
			}
			r.reconcileHumanRequiredBlockers(all, nil)

			got, err := tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tt.wantStatus)
			}
			hasLatch := false
			for _, tag := range got.Tags {
				if tag == reconciledLatchTag {
					hasLatch = true
				}
			}
			if hasLatch != tt.wantLatch {
				t.Errorf("latch tag present = %v, want %v", hasLatch, tt.wantLatch)
			}
			if tt.wantStatus == task.StatusInReview && got.StatusReason != "" {
				t.Errorf("statusReason = %q, want cleared", got.StatusReason)
			}
		})
	}
}

func TestReconcileHumanRequiredBlockersNoDoubleMoveWithReactivateLinkedOwnPR(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.fetchPRStateFn = func(string, int) (github.PRState, error) {
		return openMergeableGreenPR(), nil
	}

	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcilePRPhases(all, []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}})
	afterPhases, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPhases.Status != task.StatusHumanRequired {
		t.Fatalf("reconcilePRPhases moved the task prematurely: status = %q", afterPhases.Status)
	}

	all, err = tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcileHumanRequiredBlockers(all, nil)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Errorf("status = %q, want in-review via blocker reconciliation", got.Status)
	}
	if got.StatusReason != "" {
		t.Errorf("statusReason = %q, want cleared", got.StatusReason)
	}
}

func TestReconcileHumanRequiredBlockersClearsOnCleanPR(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Errorf("status = %q, want in-review", got.Status)
	}
	if got.StatusReason != "" {
		t.Errorf("status reason = %q, want cleared", got.StatusReason)
	}
	hasLatch := false
	for _, tag := range got.Tags {
		if tag == reconciledLatchTag {
			hasLatch = true
		}
	}
	if !hasLatch {
		t.Error("expected reconciledLatchTag after blocker reconciliation")
	}
}

func TestReconcileHumanRequiredBlockersStaysParkedWhileCIStillFailing(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "FAILURE"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersStaysParkedWhileChecksStillPending(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{
		Number:           42,
		Mergeable:        "MERGEABLE",
		CIStatus:         "FAILURE",
		HasPendingChecks: true,
	}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required while checks are pending", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersStaysParkedOnFreshConflict(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "CONFLICTING", CIStatus: "SUCCESS"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersIgnoresUnrelatedHumanRequiredReasons(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	parked, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("Draft review ready — verify & submit on GitHub"),
		PRNumber:     task.Ptr(42),
		ProjectID:    task.Ptr("Automaat/sybra"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{Number: 42, Mergeable: "MERGEABLE", CIStatus: "SUCCESS"}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersSkipsWhenPRNotFound(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	r.fetchPRStateFn = func(string, int) (github.PRState, error) {
		return github.PRState{}, errors.New("not found")
	}
	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcileHumanRequiredBlockers(all, nil)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestPollKnownTaskPRs_ReconcilesHumanRequiredBlocker(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)
	r.authCircuit = poll.NewAuthCircuit("reviews", r.logger)
	r.cfg = &config.Config{GitHub: config.GitHubConfig{PollerRole: "secondary"}}

	parked := mkHumanRequiredBlockerTask(t, tasks, 42, exhaustedFixReason(3, github.PRIssueCIFailure), nil)

	var fetched []github.PRRef
	r.fetchKnownPRsFn = func(refs []github.PRRef) []github.MonitorPRResult {
		fetched = refs
		results := make([]github.MonitorPRResult, len(refs))
		for i, ref := range refs {
			results[i] = github.MonitorPRResult{
				Repo: ref.Repo, Number: ref.Number, Open: true,
				PR: github.PullRequest{
					Number: ref.Number, Repository: ref.Repo,
					Mergeable: "MERGEABLE", CIStatus: "SUCCESS",
				},
			}
		}
		return results
	}

	r.Poll(t.Context())

	if len(fetched) != 1 || fetched[0].Number != 42 {
		t.Fatalf("fetched refs = %+v, want PR #42", fetched)
	}
	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Errorf("status = %q, want in-review", got.Status)
	}
}

func TestReconcileHumanRequiredBlockersSkipsCrossRepoBranchCollision(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	r.prTracker = github.NewIssueTracker(0)

	created, err := tasks.Create("Implement thing", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	parked, err := tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(exhaustedFixReason(3, github.PRIssueCIFailure)),
		PRNumber:     task.Ptr(42),
		Branch:       task.Ptr("renovate/lock-file-maintenance"),
		ProjectID:    task.Ptr("Automaat/sybra"),
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	prs := []github.PullRequest{{
		Number: 42, Repository: "other/repo",
		HeadRefName: "renovate/lock-file-maintenance",
		Mergeable:   "MERGEABLE", CIStatus: "SUCCESS",
	}}
	r.reconcileHumanRequiredBlockers(all, prs)

	got, err := tasks.Get(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Errorf("status = %q, want still human-required", got.Status)
	}
}

func TestApplyPRPhaseSkipsNoOp(t *testing.T) {
	r, tasks := newOutboundTestHandler(t)
	created := mkOwnPRTask(t, tasks, 42, nil)
	if _, err := tasks.Update(created.ID, task.Update{PRPhase: task.Ptr(PRPhaseDraft)}); err != nil {
		t.Fatal(err)
	}

	cur, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := cur.UpdatedAt

	r.applyPRPhase(&cur, PRPhaseDraft)
	after, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before) {
		t.Errorf("no-op applyPRPhase wrote the task (updatedAt changed)")
	}
	if after.Status != task.StatusInReview {
		t.Errorf("applyPRPhase changed status to %q", after.Status)
	}
}
