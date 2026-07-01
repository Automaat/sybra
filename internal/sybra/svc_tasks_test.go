package sybra

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/audit"
	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
	"github.com/Automaat/sybra/internal/workflow"
)

func TestTaskService_WithEstimatedAgentRunCosts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	codexLog := filepath.Join(dir, "codex.ndjson")
	if err := os.WriteFile(codexLog, []byte(`{"type":"turn.completed","usage":{"input_tokens":1000000,"cached_input_tokens":800000,"output_tokens":100000}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copilotLog := filepath.Join(dir, "copilot.ndjson")
	if err := os.WriteFile(copilotLog, []byte(`{"type":"result","sessionId":"s1","usage":{"premiumRequests":7.5}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := &TaskService{}
	got := svc.withEstimatedAgentRunCosts(task.Task{
		AgentRuns: []task.AgentRun{
			{AgentID: "codex", Model: "gpt-5", LogFile: codexLog},
			{AgentID: "copilot", Provider: "copilot", LogFile: copilotLog},
			{AgentID: "copilot-old", Model: "gpt-5", LogFile: copilotLog},
			{AgentID: "reported", Provider: "codex", Model: "gpt-5", CostUSD: 0.42, LogFile: codexLog},
		},
	})

	if diff := got.AgentRuns[0].CostUSD - 1.35; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("codex cost = %g, want 1.35", got.AgentRuns[0].CostUSD)
	}
	if diff := got.AgentRuns[1].CostUSD - 0.075; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("copilot cost = %g, want 0.075", got.AgentRuns[1].CostUSD)
	}
	if got.AgentRuns[1].PremiumRequests != 7.5 {
		t.Fatalf("copilot premium requests = %g, want 7.5", got.AgentRuns[1].PremiumRequests)
	}
	if diff := got.AgentRuns[2].CostUSD - 0.075; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("legacy copilot cost = %g, want 0.075", got.AgentRuns[2].CostUSD)
	}
	if got.AgentRuns[2].PremiumRequests != 7.5 {
		t.Fatalf("legacy copilot premium requests = %g, want 7.5", got.AgentRuns[2].PremiumRequests)
	}
	if got.AgentRuns[3].CostUSD != 0.42 {
		t.Fatalf("reported cost = %g, want 0.42", got.AgentRuns[3].CostUSD)
	}
}

func TestEstimateUsageFromEvents_UsesRunStartedAtForDatedPricing(t *testing.T) {
	t.Parallel()

	events := []agent.StreamEvent{{
		Type:         "result",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	}}
	intro, ok := estimateUsageFromEvents(
		"claude-sonnet-5",
		"claude",
		events,
		time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC),
	)
	if !ok {
		t.Fatal("intro estimate not produced")
	}
	if diff := intro.CostUSD - 12.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("intro cost = %g, want 12", intro.CostUSD)
	}

	standard, ok := estimateUsageFromEvents(
		"claude-sonnet-5",
		"claude",
		events,
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	)
	if !ok {
		t.Fatal("standard estimate not produced")
	}
	if diff := standard.CostUSD - 18.0; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("standard cost = %g, want 18", standard.CostUSD)
	}
}

func TestTaskService_GetTaskPersistsEstimatedAgentRunCosts(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "copilot.ndjson")
	if err := os.WriteFile(logPath, []byte(`{"type":"result","sessionId":"s1","usage":{"premiumRequests":7.5}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := svc.tasks.Create("Persist cost", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.tasks.AddRun(created.ID, task.AgentRun{
		AgentID:   "agent-cost",
		Model:     "gpt-5",
		Mode:      "headless",
		State:     "stopped",
		StartedAt: time.Now().UTC(),
		LogFile:   logPath,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := got.AgentRuns[0].CostUSD - 0.075; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("returned cost = %g, want 0.075", got.AgentRuns[0].CostUSD)
	}
	persisted, err := svc.tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := persisted.AgentRuns[0].CostUSD - 0.075; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("persisted cost = %g, want 0.075", persisted.AgentRuns[0].CostUSD)
	}
	if persisted.AgentRuns[0].Provider != "copilot" {
		t.Fatalf("persisted provider = %q, want copilot", persisted.AgentRuns[0].Provider)
	}
}

func TestTaskService_BlessTamperingMergesTagStatusAndAudit(t *testing.T) {
	svc, _ := setupTaskService(t)
	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = al.Close() }()
	svc.audit = al

	created, err := svc.tasks.CreateFull("flagged task", "", "headless", task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("possible test tampering — needs human bless before review: test.go: skipped test"),
		Tags:         task.Ptr([]string{"existing"}),
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := svc.BlessTampering(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != task.StatusReadyReview {
		t.Fatalf("status = %q, want %q", updated.Status, task.StatusReadyReview)
	}
	if !slices.Contains(updated.Tags, "existing") {
		t.Fatalf("tags = %v, want existing tag preserved", updated.Tags)
	}
	if !slices.Contains(updated.Tags, workflow.TamperBlessedTag) {
		t.Fatalf("tags = %v, want %q", updated.Tags, workflow.TamperBlessedTag)
	}
	if updated.CanBlessTampering {
		t.Fatalf("CanBlessTampering = true after bless, want false")
	}

	events, err := audit.Read(auditDir, audit.Query{
		Since:  time.Now().Add(-time.Hour),
		Until:  time.Now().Add(time.Hour),
		Type:   audit.EventTamperBlessed,
		TaskID: created.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("tamper blessed audit count = %d, want 1", len(events))
	}
	if events[0].Data["from_status"] != string(task.StatusHumanRequired) || events[0].Data["to_status"] != string(task.StatusReadyReview) {
		t.Fatalf("audit data = %+v, want from/to status", events[0].Data)
	}
	if events[0].Data["status_reason"] != created.StatusReason {
		t.Fatalf("audit data = %+v, want status_reason %q", events[0].Data, created.StatusReason)
	}
	if events[0].Data["finding_severity"] != "high" {
		t.Fatalf("audit data = %+v, want finding_severity high", events[0].Data)
	}
}

func TestTaskService_BlessTamperingRejectsNonTamperTasks(t *testing.T) {
	svc, _ := setupTaskService(t)

	cases := []struct {
		name         string
		status       task.Status
		statusReason string
	}{
		{
			name:         "human required unrelated reason",
			status:       task.StatusHumanRequired,
			statusReason: "approval required",
		},
		{
			name:         "tamper reason wrong status",
			status:       task.StatusInProgress,
			statusReason: workflow.TamperReasonPrefix + " — needs human bless before review: test.go",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := svc.tasks.CreateFull(tc.name, "", "headless", task.Update{
				Status:       task.Ptr(tc.status),
				StatusReason: task.Ptr(tc.statusReason),
			})
			if err != nil {
				t.Fatal(err)
			}

			updated, err := svc.BlessTampering(created.ID)
			if err == nil {
				t.Fatal("BlessTampering succeeded, want error")
			}
			if updated.Status != tc.status {
				t.Fatalf("returned status = %q, want %q", updated.Status, tc.status)
			}
			persisted, err := svc.tasks.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != tc.status {
				t.Fatalf("persisted status = %q, want %q", persisted.Status, tc.status)
			}
			if slices.Contains(persisted.Tags, workflow.TamperBlessedTag) {
				t.Fatalf("persisted tags = %v, must not contain %q", persisted.Tags, workflow.TamperBlessedTag)
			}
		})
	}
}

func TestTaskService_CreateTask_IssueURLWaitsForEnrichment(t *testing.T) {
	svc, _ := setupTaskService(t)

	fetchEntered := make(chan struct{})
	releaseFetch := make(chan struct{})
	svc.fetchIssue = func(repo string, number int) (github.Issue, error) {
		if repo != "owner/repo" || number != 12 {
			t.Fatalf("fetchIssue = %s#%d, want owner/repo#12", repo, number)
		}
		close(fetchEntered)
		<-releaseFetch
		return github.Issue{
			Number:     12,
			Title:      "real issue",
			URL:        "https://github.com/owner/repo/issues/12",
			Repository: "owner/repo",
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		return []github.PullRequest{{
			Number:      34,
			HeadRefName: "fix/issue-12",
			Author:      "me",
		}}, nil
	}
	svc.viewerLogin = func() string { return "me" }

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/12", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-fetchEntered:
	case <-time.After(time.Second):
		t.Fatal("fetchIssue was not called")
	}

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want nil before issue enrichment", got.Workflow)
	}

	close(releaseFetch)
	svc.wg.Wait()

	got, err = svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("Status = %q, want %q", got.Status, task.StatusInReview)
	}
	if got.PRNumber != 34 || got.Branch != "fix/issue-12" {
		t.Fatalf("linked PR = %d/%q, want 34/fix/issue-12", got.PRNumber, got.Branch)
	}
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want nil for linked viewer PR", got.Workflow)
	}
}

func TestTaskService_CreateTask_UmbrellaIssueExpandsAndDropsStub(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.cfg = &config.Config{Umbrella: config.UmbrellaConfig{Enabled: true}}
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     1151,
			Title:      "☂️ Workflow reliability: re-dispatch & PR-linking",
			URL:        "https://github.com/owner/repo/issues/1151",
			Repository: "owner/repo",
		}, nil
	}
	// A linked-PR fetch must NOT happen on the umbrella path. Record the call
	// from the async enrichment goroutine and assert after wg.Wait() — calling
	// t.Fatal from a non-test goroutine is unreliable.
	var linkedPRFetched atomic.Bool
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		linkedPRFetched.Store(true)
		return nil, nil
	}

	var expandedURL atomic.Value
	svc.umbrellaExpand = func(issueURL string) (umbrella.Result, error) {
		expandedURL.Store(issueURL)
		return umbrella.Result{UmbrellaURL: issueURL, Created: 6}, nil
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/1151", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	if linkedPRFetched.Load() {
		t.Fatal("linked-PR fetch must not run for an umbrella issue")
	}
	if got, _ := expandedURL.Load().(string); got != "https://github.com/owner/repo/issues/1151" {
		t.Fatalf("expander called with %q, want the umbrella URL", got)
	}
	// The stub must be deleted — the expander created its own tracker.
	if _, err := svc.GetTask(created.ID); err == nil {
		t.Fatalf("stub task %s still exists; want it deleted after expansion", created.ID)
	}
}

func TestTaskService_CreateTask_UmbrellaExpandFailureKeepsInertStub(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.cfg = &config.Config{Umbrella: config.UmbrellaConfig{Enabled: true}}
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     1151,
			Title:      "☂️ broken umbrella",
			URL:        "https://github.com/owner/repo/issues/1151",
			Repository: "owner/repo",
			Labels:     []string{"umbrella", "backend"},
		}, nil
	}
	svc.umbrellaExpand = func(string) (umbrella.Result, error) {
		return umbrella.Result{}, errors.New("planner boom")
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/1151", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatalf("stub task should survive a failed expansion: %v", err)
	}
	if got.Title != "☂️ broken umbrella" {
		t.Fatalf("Title = %q, want enriched umbrella title", got.Title)
	}
	// Labels must be preserved so tag-driven routing/UI still identify the
	// umbrella when the title doesn't start with ☂️.
	if !slices.Equal(got.Tags, []string{"umbrella", "backend"}) {
		t.Fatalf("Tags = %v, want [umbrella backend] from the issue labels", got.Tags)
	}
	// A StatusReason must explain why no workflow started.
	if got.StatusReason == "" {
		t.Fatal("StatusReason is empty, want an explanation for the inert stub")
	}
	// No flat workflow may be started on a known umbrella, even when expansion failed.
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want nil for a failed umbrella expansion", got.Workflow)
	}
}

func TestTaskService_CreateTask_UmbrellaDisabledFallsBackToFlat(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.cfg = &config.Config{Umbrella: config.UmbrellaConfig{Enabled: false}}
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     1151,
			Title:      "☂️ umbrella but feature off",
			URL:        "https://github.com/owner/repo/issues/1151",
			Repository: "owner/repo",
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) { return nil, nil }
	svc.viewerLogin = func() string { return "me" }
	var expanderCalled atomic.Bool
	svc.umbrellaExpand = func(string) (umbrella.Result, error) {
		expanderCalled.Store(true)
		return umbrella.Result{}, nil
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/1151", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	if expanderCalled.Load() {
		t.Fatal("expander must not run when umbrella.enabled is false")
	}
	// Disabled → flat enrichment proceeds and a workflow is started.
	got := waitForWorkflow(t, svc, created.ID)
	if got.Title != "☂️ umbrella but feature off" {
		t.Fatalf("Title = %q, want enriched title", got.Title)
	}
}

func TestSkipTaskCreatedWorkflow_EnrichPendingStub(t *testing.T) {
	t.Parallel()
	if !skipTaskCreatedWorkflow(task.Task{Tags: []string{enrichPendingTag}}) {
		t.Fatal("enrich-pending stub must be skipped by the emit-path dispatch")
	}
	if skipTaskCreatedWorkflow(task.Task{Tags: []string{"backend"}}) {
		t.Fatal("an ordinary task must not be skipped")
	}
}

func TestTaskService_CreateTask_IssueURLStubMarkedThenCleared(t *testing.T) {
	svc, _ := setupTaskService(t)

	releaseFetch := make(chan struct{})
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		<-releaseFetch
		return github.Issue{
			Number:     13,
			Title:      "plain issue",
			URL:        "https://github.com/owner/repo/issues/13",
			Repository: "owner/repo",
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) { return nil, nil }
	svc.viewerLogin = func() string { return "me" }

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/13", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	// Before enrichment: the stub carries the marker so the emit-path
	// task.created dispatch skips it (no flat workflow on a raw-URL stub).
	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, want enrich-pending marker before enrichment", got.Tags)
	}
	if skipTaskCreatedWorkflow(got) != true {
		t.Fatal("stub must be skipped while enrich-pending")
	}

	// After enrichment: marker is cleared and the workflow dispatches.
	close(releaseFetch)
	svc.wg.Wait()
	got = waitForWorkflow(t, svc, created.ID)
	if slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, enrich-pending marker should be cleared after enrichment", got.Tags)
	}
}

func TestTaskService_CreateTask_IssueURLNoPRStartsWorkflowAfterEnrichment(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     13,
			Title:      "plain issue",
			URL:        "https://github.com/owner/repo/issues/13",
			Repository: "owner/repo",
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) { return nil, nil }
	svc.viewerLogin = func() string { return "me" }

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/13", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got := waitForWorkflow(t, svc, created.ID)
	if got.Title != "plain issue" {
		t.Fatalf("Title = %q, want plain issue", got.Title)
	}
	if got.Status != task.StatusTodo && got.Status != task.StatusPlanning && got.Status != task.StatusInProgress {
		t.Fatalf("Status = %q, want workflow-owned issue status", got.Status)
	}
}
