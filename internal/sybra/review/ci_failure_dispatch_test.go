package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
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
		logger:          logger,
		emit:            func(string, any) {},
		tasks:           tasks,
		projects:        projects,
		agents:          agentMgr,
		prTracker:       github.NewIssueTracker(time.Minute),
		WorkflowEngine:  engine,
		fetchReviewsFn:  fetchReviewsFn,
		pushPreflightFn: stubPushPreflight(nil),
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

func TestPollAndMonitorPRs_ApprovedCIFailureDispatchesFixAgent(t *testing.T) {
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
		Number:         4242,
		Repository:     "o/r",
		HeadRefName:    "feat/x",
		HeadSHA:        "sha-fail",
		URL:            "https://github.com/o/r/pull/4242",
		Mergeable:      "MERGEABLE",
		CIStatus:       "FAILURE",
		ReviewDecision: "APPROVED",
		Author:         "me",
	}

	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{failingPR}}, nil
	})

	r.pollAndMonitorPRs(context.Background())

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow == nil {
		t.Fatal("no pr-fix workflow dispatched for an approved failing-CI PR")
	}
	if got.Status == task.StatusHumanRequired {
		t.Fatalf("status = %q, want agent dispatch instead of parking", got.Status)
	}
	prompt := got.Workflow.Variables["prompt"]
	if !strings.Contains(prompt, "Approval preservation") {
		t.Fatalf("prompt missing approval-preservation guard:\n%s", prompt)
	}
	if retries := r.prTracker.Retries(created.ID, github.PRIssueCIFailure); retries != 1 {
		t.Fatalf("ci_failure retries = %d, want 1; fix was dispatched", retries)
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

// A rerun re-runs jobs the repo already ran and sends no content anywhere, so
// the work/pet split that guards content-authoring paths must not apply to it.
// Gating it to pet made every transient CI failure on a work project spend a
// full fix agent on work `gh run rerun --failed` does for free.
func TestPollAndMonitorPRs_CIFailureRerunsWorkPRBeforeFixAgent(t *testing.T) {
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
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypeWork); err != nil {
		t.Fatal(err)
	}
	r.rerunFailedChecks = func(repo string, number int) error {
		rerunRepo = repo
		rerunNumber = number
		return nil
	}

	r.pollAndMonitorPRs(context.Background())

	if rerunRepo != "o/r" || rerunNumber != 4242 {
		t.Fatalf("rerun = %s#%d, want o/r#4242 — a work project must get the free rerun too", rerunRepo, rerunNumber)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatal("unexpected pr-fix workflow; a work project's transient rerun should wait for GitHub checks")
	}
}

// TestPollAndMonitorPRs_FlakyCIFailureRerunsAndLogsFlakeEvent verifies that
// with github.flaky_detection enabled and the classifier reporting every
// gating check flaky, a lone ci_failure issue reruns (same deterministic
// infra-rerun path as the unconditional rerun) but additionally records a
// distinct ci_flake_detected audit event and marks the PRIssueCIFlake budget,
// while still never dispatching a pr-fix agent.
func TestPollAndMonitorPRs_FlakyCIFailureRerunsAndLogsFlakeEvent(t *testing.T) {
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

	auditDir := filepath.Join(t.TempDir(), "audit")
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	defer auditLog.Close()

	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{failingPR}}, nil
	})
	r.audit = auditLog
	r.cfg = &config.Config{GitHub: config.GitHubConfig{FlakyDetection: true}}
	var classifiedRepo, classifiedSHA string
	var classifiedThreshold float64
	r.classifyFlakiness = func(repo, sha string, threshold float64) (bool, []string, error) {
		classifiedRepo, classifiedSHA, classifiedThreshold = repo, sha, threshold
		return true, []string{"unit-tests"}, nil
	}
	var rerunCalled bool
	r.rerunFailedChecks = func(repo string, number int) error {
		rerunCalled = true
		return nil
	}
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}

	r.pollAndMonitorPRs(context.Background())

	if !rerunCalled {
		t.Fatal("flaky classification should still trigger the deterministic infra rerun")
	}
	if classifiedRepo != "o/r" || classifiedSHA != "sha-fail" {
		t.Fatalf("classifier called with repo=%q sha=%q, want o/r sha-fail", classifiedRepo, classifiedSHA)
	}
	if classifiedThreshold != config.DefaultFlakySuccessThreshold {
		t.Fatalf("classifier threshold = %v, want default %v", classifiedThreshold, config.DefaultFlakySuccessThreshold)
	}
	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFlake); got != 1 {
		t.Errorf("ci_flake retries = %d, want 1", got)
	}
	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFailure); got != 0 {
		t.Errorf("ci_failure retries = %d, want 0 (no fix agent dispatched)", got)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatal("unexpected pr-fix workflow; a flaky classification should skip the fix agent")
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review", got.Status)
	}

	events := readExperienceAuditEvents(t, auditDir)
	idx := slices.IndexFunc(events, func(e audit.Event) bool {
		return e.Type == audit.EventPRCIFlakeDetected
	})
	if idx < 0 {
		t.Fatal("missing pr_monitor.ci_flake_detected audit event")
	}
	data := events[idx].Data
	if got := data["repo"]; got != "o/r" {
		t.Errorf("flake event repo = %v, want o/r", got)
	}
	checksRaw, _ := json.Marshal(data["checks"])
	if !strings.Contains(string(checksRaw), "unit-tests") {
		t.Errorf("flake event checks = %s, want to contain unit-tests", checksRaw)
	}
}

// TestPollAndMonitorPRs_DeterministicCIFailureSkipsFlakeEventEvenWhenEnabled
// verifies that enabling github.flaky_detection does not change behavior for
// a deterministic (non-flaky) ci_failure: the blind infra rerun still fires
// exactly as before, but no ci_flake_detected event is logged and no
// PRIssueCIFlake budget is spent.
func TestPollAndMonitorPRs_DeterministicCIFailureSkipsFlakeEventEvenWhenEnabled(t *testing.T) {
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

	auditDir := filepath.Join(t.TempDir(), "audit")
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	defer auditLog.Close()

	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{failingPR}}, nil
	})
	r.audit = auditLog
	r.cfg = &config.Config{GitHub: config.GitHubConfig{FlakyDetection: true}}
	var classifyCalled bool
	r.classifyFlakiness = func(repo, sha string, threshold float64) (bool, []string, error) {
		classifyCalled = true
		return false, nil, nil
	}
	var rerunCalled bool
	r.rerunFailedChecks = func(repo string, number int) error {
		rerunCalled = true
		return nil
	}
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}

	r.pollAndMonitorPRs(context.Background())

	if !classifyCalled {
		t.Fatal("classifier should have been consulted")
	}
	if !rerunCalled {
		t.Fatal("deterministic failure should still fall through to the ordinary infra rerun")
	}
	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFlake); got != 0 {
		t.Errorf("ci_flake retries = %d, want 0 for a deterministic failure", got)
	}

	events := readExperienceAuditEvents(t, auditDir)
	if idx := slices.IndexFunc(events, func(e audit.Event) bool {
		return e.Type == audit.EventPRCIFlakeDetected
	}); idx >= 0 {
		t.Fatalf("unexpected ci_flake_detected event for a deterministic failure: %+v", events[idx])
	}
}

// TestEscalateExhaustedFix_FlakyCIFailureStaysInReview verifies that an
// exhausted ci_failure budget does not park the task human-required when
// flaky detection is enabled and the classifier attributes the failure to
// flakiness — the task stays exactly where it was, and a
// ci_flake_detected audit event records why escalation was skipped.
func TestEscalateExhaustedFix_FlakyCIFailureStaysInReview(t *testing.T) {
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

	auditDir := filepath.Join(t.TempDir(), "audit")
	auditLog, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	defer auditLog.Close()

	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		audit:     auditLog,
		tasks:     tasks,
		prTracker: github.NewIssueTracker(time.Minute),
		cfg:       &config.Config{GitHub: config.GitHubConfig{FlakyDetection: true}},
		classifyFlakiness: func(repo, sha string, threshold float64) (bool, []string, error) {
			return true, []string{"e2e-tests"}, nil
		},
	}

	issue := github.PRIssue{
		Kind:   github.PRIssueCIFailure,
		TaskID: created.ID,
		PR:     github.PullRequest{Number: 4242, Repository: "o/r", HeadSHA: "sha-fail"},
	}
	r.escalateExhaustedFix(issue)

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review (flaky exhaustion must not escalate)", got.Status)
	}

	events := readExperienceAuditEvents(t, auditDir)
	if idx := slices.IndexFunc(events, func(e audit.Event) bool {
		return e.Type == audit.EventPRCIFlakeDetected
	}); idx < 0 {
		t.Fatal("missing pr_monitor.ci_flake_detected audit event on exhausted flaky escalation")
	} else if idx2 := slices.IndexFunc(events, func(e audit.Event) bool {
		return e.Type == audit.EventPRFixExhausted
	}); idx2 >= 0 {
		t.Fatal("unexpected pr_monitor.fix_exhausted event for a flaky-classified exhaustion")
	}
}

// TestEscalateExhaustedFix_DeterministicCIFailureStillEscalates guards the
// flaky gate from swallowing real escalations: when the classifier reports a
// deterministic failure, exhaustion still parks the task human-required
// exactly as it did before flaky detection existed.
func TestEscalateExhaustedFix_DeterministicCIFailureStillEscalates(t *testing.T) {
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

	r := &Handler{
		logger:    slog.New(slog.DiscardHandler),
		tasks:     tasks,
		prTracker: github.NewIssueTracker(time.Minute),
		cfg:       &config.Config{GitHub: config.GitHubConfig{FlakyDetection: true}},
		classifyFlakiness: func(repo, sha string, threshold float64) (bool, []string, error) {
			return false, nil, nil
		},
	}

	issue := github.PRIssue{
		Kind:   github.PRIssueCIFailure,
		TaskID: created.ID,
		PR:     github.PullRequest{Number: 4242, Repository: "o/r", HeadSHA: "sha-fail"},
	}
	r.escalateExhaustedFix(issue)

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required (deterministic failures still escalate)", got.Status)
	}
}

// TestPollAndMonitorPRs_FlakyCIFailureRerunsInsteadOfEscalating is the
// counterpart to TestPollAndMonitorPRs_CIFailureDispatchesFix: a ci_failure
// classified as flaky (CIFlaky=true) must log the pattern and get a rerun,
// never a fix agent, and must not touch the deterministic ci_failure retry
// budget or escalate to human-required on first detection.
func TestPollAndMonitorPRs_FlakyCIFailureRerunsInsteadOfEscalating(t *testing.T) {
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

	flakyPR := github.PullRequest{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		HeadSHA:     "sha-flaky",
		URL:         "https://github.com/o/r/pull/4242",
		Mergeable:   "MERGEABLE",
		CIStatus:    "FAILURE",
		CIFlaky:     true,
		Author:      "me",
	}

	var rerunCalled bool
	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{flakyPR}}, nil
	})
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	r.rerunFailedChecks = func(repo string, number int) error {
		rerunCalled = true
		return nil
	}

	r.pollAndMonitorPRs(context.Background())

	if !rerunCalled {
		t.Fatal("flaky ci_failure did not trigger a rerun")
	}
	if got := r.prTracker.Retries(created.ID, github.PRIssueCIFailure); got != 0 {
		t.Errorf("ci_failure retries = %d, want 0 (flaky issues never touch this budget)", got)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatal("unexpected pr-fix workflow; a flaky ci_failure must never dispatch a fix agent")
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want in-review (no premature escalation)", got.Status)
	}
}

func TestPollAndMonitorPRs_FlakyCIFailureConsumesRerunBudgetOnSameSHA(t *testing.T) {
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

	flakyPR := github.PullRequest{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		HeadSHA:     "sha-flaky",
		URL:         "https://github.com/o/r/pull/4242",
		Mergeable:   "MERGEABLE",
		CIStatus:    "FAILURE",
		CIFlaky:     true,
		Author:      "me",
	}

	var reruns int
	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{flakyPR}}, nil
	})
	r.prTracker = github.NewIssueTracker(0)
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	r.rerunFailedChecks = func(string, int) error {
		reruns++
		return nil
	}

	for range github.MaxRetries {
		r.pollAndMonitorPRs(context.Background())
	}

	if reruns != github.MaxRetries {
		t.Fatalf("reruns = %d, want %d for repeated flaky CI on the same SHA", reruns, github.MaxRetries)
	}
	if got := r.prTracker.Retries(created.ID, ciInfraRerunKind); got != github.MaxRetries {
		t.Fatalf("ci rerun retries = %d, want %d", got, github.MaxRetries)
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q after final rerun, want in-review until next poll observes cap", got.Status)
	}

	r.pollAndMonitorPRs(context.Background())

	if reruns != github.MaxRetries {
		t.Fatalf("reruns = %d after cap, want still %d", reruns, github.MaxRetries)
	}
	got, err = tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q after capped flaky CI, want human-required", got.Status)
	}
	if got.StatusReason != persistentFlakyCIReason {
		t.Fatalf("statusReason = %q, want %q", got.StatusReason, persistentFlakyCIReason)
	}
}

// TestPollAndMonitorPRs_FlakyCIFailureEscalatesAfterRerunBudgetExhausted
// verifies the only path a flaky ci_failure escalates: the ci-infra rerun
// budget itself (not the deterministic ci_failure budget) is spent.
func TestPollAndMonitorPRs_FlakyCIFailureEscalatesAfterRerunBudgetExhausted(t *testing.T) {
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
		Tags:      task.Ptr([]string{reconciledLatchTag, "keep"}),
	}); err != nil {
		t.Fatal(err)
	}

	flakyPR := github.PullRequest{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		HeadSHA:     "sha-flaky",
		URL:         "https://github.com/o/r/pull/4242",
		Mergeable:   "MERGEABLE",
		CIStatus:    "FAILURE",
		CIFlaky:     true,
		Author:      "me",
	}

	r := buildPRFixHandler(t, tasks, func() (github.ReviewSummary, error) {
		return github.ReviewSummary{CreatedByMe: []github.PullRequest{flakyPR}}, nil
	})
	if _, err := r.projects.CreateMeta("https://github.com/o/r", project.ProjectTypePet); err != nil {
		t.Fatal(err)
	}
	for i := range github.MaxRetries {
		r.prTracker.MarkHandled(created.ID, ciInfraRerunKind, fmt.Sprintf("sha-prior-%d", i))
	}
	var rerunCalled bool
	r.rerunFailedChecks = func(string, int) error { rerunCalled = true; return nil }

	r.pollAndMonitorPRs(context.Background())

	if rerunCalled {
		t.Fatal("rerun attempted after the ci-infra rerun budget was already exhausted")
	}
	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatal("unexpected pr-fix workflow; a flaky ci_failure must never dispatch a fix agent")
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want human-required once the rerun budget is spent", got.Status)
	}
	if got.StatusReason != persistentFlakyCIReason {
		t.Fatalf("statusReason = %q, want %q", got.StatusReason, persistentFlakyCIReason)
	}
	for _, tag := range got.Tags {
		if tag == reconciledLatchTag {
			t.Fatalf("reconciliation latch still present after flaky escalation: tags=%v", got.Tags)
		}
	}

	all, err := tasks.List()
	if err != nil {
		t.Fatal(err)
	}
	r.reconcileHumanRequiredBlockers(all, []github.PullRequest{{
		Number:      4242,
		Repository:  "o/r",
		HeadRefName: "feat/x",
		Mergeable:   "MERGEABLE",
		CIStatus:    "SUCCESS",
	}})
	got, err = tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q after green PR reconciliation, want in-review", got.Status)
	}
	if got.StatusReason != "" {
		t.Fatalf("statusReason = %q after green PR reconciliation, want cleared", got.StatusReason)
	}
}
