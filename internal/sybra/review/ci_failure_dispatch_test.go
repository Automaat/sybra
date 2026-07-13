package review

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/project"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

func buildPRFixHandler(t *testing.T, tasks *task.Manager, fetchReviewsFn func() (github.ReviewSummary, error)) *Handler {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	projects, err := project.NewStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wfStore, wfErr := workflow.NewStore(filepath.Join(t.TempDir(), "workflows"))
	if wfErr != nil {
		t.Fatal(wfErr)
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
	return &Handler{
		logger:         logger,
		emit:           func(string, any) {},
		tasks:          tasks,
		projects:       projects,
		agents:         agentMgr,
		prTracker:      github.NewIssueTracker(time.Minute),
		WorkflowEngine: engine,
		fetchReviewsFn: fetchReviewsFn,
	}
}

func TestPollAndMonitorPRs_CIFailureDispatchesFix(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:   task.Ptr(task.StatusInReview),
		PRNumber: task.Ptr(4242),
		Branch:   task.Ptr("feat/x"),
	}); err != nil {
		t.Fatal(err)
	}

	failingPR := github.PullRequest{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		HeadSHA:     "sha-fail",
		URL:         "https://github.com/o/r/pull/4242",
		Mergeable:   "MERGEABLE",
		CIStatus:    "FAILURE",
		Author:      "me",
	}

	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{failingPR}}, nil
	})

	r.pollAndMonitorPRs(context.Background())

	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFailure); got != 1 {
		t.Errorf("ci_failure retries = %d, want 1 (a pr-fix dispatch marks it handled)", got)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no pr-fix workflow dispatched for a failing-CI in-review PR")
	}
	if k := got.Workflow.Variables["pr_issue_kind"]; k != string(github.PRIssueCIFailure) {
		t.Errorf("pr_issue_kind = %q, want %q", k, github.PRIssueCIFailure)
	}
}

func TestPollAndMonitorPRs_CIFailureRerunsPetPRBeforeFixAgent(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		ProjectID: task.Ptr("o/r"),
		PRNumber:  task.Ptr(4242),
		Branch:    task.Ptr("feat/x"),
	}); err != nil {
		t.Fatal(err)
	}

	failingPR := github.PullRequest{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		HeadSHA:     "sha-fail",
		URL:         "https://github.com/o/r/pull/4242",
		Mergeable:   "MERGEABLE",
		CIStatus:    "FAILURE",
		Author:      "me",
	}

	var rerunRepo string
	var rerunNumber int
	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{failingPR}}, nil
	})
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	r.rerunFailedChecks = func(repo string, number int) error {
		rerunRepo = repo
		rerunNumber = number
		return nil
	}

	r.pollAndMonitorPRs(context.Background())

	if rerunRepo != "o/r" || rerunNumber != 4242 {
		t.Fatalf("rerun = %s#%d, want o/r#4242", rerunRepo, rerunNumber)
	}
	if got := r.prTracker.Retries(created.ID, ciInfraRerunKind); got != 1 {
		t.Errorf("ci rerun retries = %d, want 1", got)
	}
	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFailure); got != 0 {
		t.Errorf("ci_failure retries = %d, want 0 (no fix agent dispatched)", got)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatal("unexpected pr-fix workflow; transient rerun should wait for GitHub checks")
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", got.Status)
	}
}

func TestPollAndMonitorPRs_CIFailureRerunPermissionDenialParks(t *testing.T) {
	store, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(store, nil)

	created, err := tasks.Create("ship it", "", string(task.AgentModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.Update(created.ID, task.Update{
		Status:    task.Ptr(task.StatusInReview),
		ProjectID: task.Ptr("o/r"),
		PRNumber:  task.Ptr(4242),
		Branch:    task.Ptr("feat/x"),
	}); err != nil {
		t.Fatal(err)
	}

	failingPR := github.PullRequest{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		HeadSHA:     "sha-fail",
		URL:         "https://github.com/o/r/pull/4242",
		Mergeable:   "MERGEABLE",
		CIStatus:    "FAILURE",
		Author:      "me",
	}

	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{failingPR}}, nil
	})
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	r.rerunFailedChecks = func(string, int) error {
		return fmt.Errorf("gh run rerun --failed: Resource not accessible by integration: exit status 1")
	}

	r.pollAndMonitorPRs(context.Background())

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatal("unexpected pr-fix workflow after rerun permission denial")
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required", got.Status)
	}
	if got.StatusReason != ciInfraRerunPermissionReason {
		t.Fatalf("statusReason = %q, want %q", got.StatusReason, ciInfraRerunPermissionReason)
	}
}
