package sybra

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/artifact"
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

func TestTaskService_ListTaskArtifactsIncludesContent(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	created, err := svc.tasks.Create("Artifacts", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.artifacts.Put(created.ID, artifact.Artifact{
		Kind:    artifact.KindPlan,
		Name:    "plan.md",
		Content: []byte("# Plan\n\ndo it"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListTaskArtifacts(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Name != "plan.md" || got[0].Content != "# Plan\n\ndo it" {
		t.Fatalf("artifact = %+v", got[0])
	}
}

func TestTaskService_ListTaskArtifactsStripsSourcePath(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	created, err := svc.tasks.Create("Artifacts", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.artifacts.Put(created.ID, artifact.Artifact{
		Kind:       artifact.KindPlan,
		Name:       "plan.md",
		SourcePath: "/Users/operator/.sybra/worktrees/t1/agent-out.log",
		Content:    []byte("# Plan"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListTaskArtifacts(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].SourcePath != "" {
		t.Fatalf("SourcePath = %q, want stripped", got[0].SourcePath)
	}
}

func TestTaskService_ListTaskArtifactsTruncatesStreamFromTail(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	created, err := svc.tasks.Create("Artifacts", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("a", 1024)
	for range taskDiagnosticReadLimit/1024 + 2 {
		if err := svc.artifacts.Append(created.ID, artifact.KindTrace, map[string]string{"pad": padding}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.artifacts.Append(created.ID, artifact.KindTrace, map[string]string{"marker": "TAIL_MARKER"}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListTaskArtifacts(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if !got[0].Stream {
		t.Fatalf("expected trace.jsonl to be a stream artifact")
	}
	if !strings.Contains(got[0].Content, "TAIL_MARKER") {
		t.Fatalf("expected truncated content to keep the tail, got suffix %q", got[0].Content[len(got[0].Content)-40:])
	}
}

func TestTaskService_GetTaskSetupLog(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	svc := &TaskService{cfg: &config.Config{}}
	svc.cfg.Logging.Dir = logDir
	taskID := "task-setup"
	logPath := filepath.Join(logDir, "worktrees", taskID+"-setup.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("setup failed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetTaskSetupLog(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Exists || got.Content != "setup failed\n" || got.Path != logPath {
		t.Fatalf("setup log = %+v", got)
	}
}

func TestTaskService_ListTaskAuditEventsNewestFirst(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	svc := &TaskService{cfg: &config.Config{}}
	svc.cfg.Logging.Dir = logDir
	al, err := audit.NewLogger(svc.cfg.AuditDir())
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	if err := al.Log(audit.Event{Type: audit.EventTaskCreated, TaskID: "task-a"}); err != nil {
		t.Fatal(err)
	}
	if err := al.Log(audit.Event{Type: audit.EventAgentStarted, TaskID: "task-b"}); err != nil {
		t.Fatal(err)
	}
	if err := al.Log(audit.Event{Type: audit.EventAgentCompleted, TaskID: "task-a", AgentID: "agent-1"}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListTaskAuditEvents("task-a", 14)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].Type != audit.EventAgentCompleted || got[0].AgentID != "agent-1" {
		t.Fatalf("newest event = %+v", got[0])
	}
	if got[1].Type != audit.EventTaskCreated {
		t.Fatalf("oldest event = %+v", got[1])
	}
}

func TestTaskService_GetTamperReport(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	created, err := svc.tasks.Create("Tamper report", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	reportJSON := `{
  "taskId": "ignored",
  "base": "abc123",
  "range": "abc123..HEAD",
  "files": ["internal/foo_test.go"],
  "findings": [{
    "file": "internal/foo_test.go",
    "category": "test",
    "severity": "high",
    "rule": "removed-test",
    "detail": "func TestFoo"
  }]
}`
	if _, err := svc.artifacts.Put(created.ID, artifact.Artifact{
		Kind:    artifact.KindGeneric,
		Name:    "tamper-report.json",
		StepID:  "detect_tampering",
		Content: []byte(reportJSON),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetTamperReport(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ReportAvailable {
		t.Fatal("ReportAvailable = false, want true")
	}
	if got.TaskID != created.ID || got.Base != "abc123" || got.Range != "abc123..HEAD" {
		t.Fatalf("report identity = (%q, %q, %q), want task/base/range", got.TaskID, got.Base, got.Range)
	}
	if !slices.Equal(got.Files, []string{"internal/foo_test.go"}) {
		t.Fatalf("Files = %v", got.Files)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("Findings len = %d, want 1", len(got.Findings))
	}
	if f := got.Findings[0]; f.File != "internal/foo_test.go" || f.Category != "test" || f.Severity != "high" || f.Rule != "removed-test" || f.Detail != "func TestFoo" {
		t.Fatalf("Finding = %+v", f)
	}
}

func TestTaskService_ListTaskProgress(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	created, err := svc.tasks.Create("Progress task", "", "headless")
	if err != nil {
		t.Fatal(err)
	}

	if got, err := svc.ListTaskProgress(created.ID); err != nil || len(got) != 0 {
		t.Fatalf("empty log = %v, %v; want [], nil", got, err)
	}

	for _, m := range []string{"first", "second"} {
		if err := svc.artifacts.AppendProgress(created.ID, artifact.ProgressEntry{
			Kind: artifact.ProgressKindProgress, Message: m,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := svc.ListTaskProgress(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Message != "second" || got[1].Message != "first" {
		t.Fatalf("ListTaskProgress = %+v, want newest-first [second, first]", got)
	}
}

func TestTaskService_ListTaskProgressNoStore(t *testing.T) {
	t.Parallel()
	svc := &TaskService{}
	got, err := svc.ListTaskProgress("any")
	if err != nil || len(got) != 0 {
		t.Fatalf("nil store = %v, %v; want [], nil", got, err)
	}
}

func TestTaskService_GetTamperReportMissing(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())

	got, err := svc.GetTamperReport("missing-report")
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "missing-report" || got.ReportAvailable {
		t.Fatalf("missing report = %+v, want unavailable for task", got)
	}
	if len(got.Files) != 0 || len(got.Findings) != 0 {
		t.Fatalf("missing report files/findings = %v/%v, want empty", got.Files, got.Findings)
	}
}

func TestTaskService_GetTamperReportMalformed(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	created, err := svc.tasks.Create("Bad tamper report", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.artifacts.Put(created.ID, artifact.Artifact{
		Kind:    artifact.KindGeneric,
		Name:    "tamper-report.json",
		Content: []byte(`{"taskId":`),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.GetTamperReport(created.ID); err == nil {
		t.Fatal("GetTamperReport malformed JSON err = nil, want error")
	}
}

func TestTaskService_BlessTampering(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.artifacts = artifact.New(t.TempDir())
	auditDir := t.TempDir()
	al, err := audit.NewLogger(auditDir)
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	svc.audit = al

	created, err := svc.tasks.Create("Bless tamper", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	reason := workflow.TamperFlaggedReasonPrefix + " removed-test in internal/foo_test.go"
	flagged, err := svc.tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		Tags:         task.Ptr([]string{"backend", "frontend"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.artifacts.Put(flagged.ID, artifact.Artifact{
		Kind: artifact.KindGeneric,
		Name: "tamper-report.json",
		Content: []byte(`{
  "taskId": "` + flagged.ID + `",
  "base": "abc123",
  "range": "abc123..HEAD",
  "files": ["internal/foo_test.go"],
  "findings": [
    {"file":"internal/foo_test.go","category":"test","severity":"high","rule":"removed-test","detail":"func TestFoo"},
    {"file":"internal/bar_test.go","category":"test","severity":"medium","rule":"changed-fixture","detail":"fixture"}
  ]
}`),
	}); err != nil {
		t.Fatal(err)
	}

	statusHook := make(chan [3]string, 1)
	svc.tasks.SetStatusChangeHook(func(taskID, from, to string) {
		statusHook <- [3]string{taskID, from, to}
	})
	got, err := svc.BlessTampering(flagged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusReadyReview {
		t.Fatalf("Status = %q, want %q", got.Status, task.StatusReadyReview)
	}
	if got.StatusReason != "" {
		t.Fatalf("StatusReason = %q, want cleared", got.StatusReason)
	}
	if !slices.Equal(got.Tags, []string{"backend", "frontend", workflow.TamperBlessedTag}) {
		t.Fatalf("Tags = %v, want existing tags plus blessed", got.Tags)
	}
	select {
	case fired := <-statusHook:
		if fired != [3]string{flagged.ID, string(task.StatusHumanRequired), string(task.StatusReadyReview)} {
			t.Fatalf("status hook = %v", fired)
		}
	case <-time.After(time.Second):
		t.Fatal("status hook did not fire")
	}

	events := readTaskServiceAuditEvents(t, auditDir)
	var bless audit.Event
	for i := range events {
		if events[i].Type == audit.EventTaskTamperBlessed {
			bless = events[i]
			break
		}
	}
	if bless.Type == "" {
		t.Fatalf("audit events = %+v, want tamper bless event", events)
	}
	if bless.TaskID != flagged.ID {
		t.Fatalf("audit task_id = %q, want %q", bless.TaskID, flagged.ID)
	}
	if got := bless.Data["previousStatus"]; got != string(task.StatusHumanRequired) {
		t.Fatalf("previousStatus = %v", got)
	}
	if got := bless.Data["previousStatusReason"]; got != reason {
		t.Fatalf("previousStatusReason = %v", got)
	}
	if got := bless.Data["reportAvailable"]; got != true {
		t.Fatalf("reportAvailable = %v", got)
	}
	if got := bless.Data["findingCount"]; got != float64(2) {
		t.Fatalf("findingCount = %v", got)
	}
	if got := bless.Data["highSeverityFindingCount"]; got != float64(1) {
		t.Fatalf("highSeverityFindingCount = %v", got)
	}
	if got := bless.Data["tagAdded"]; got != true {
		t.Fatalf("tagAdded = %v", got)
	}
}

func TestTaskService_BlessTamperingAlreadyBlessed(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	created, err := svc.tasks.Create("Already blessed", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	reason := workflow.TamperFlaggedReasonPrefix + " removed-test in internal/foo_test.go"
	flagged, err := svc.tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr(reason),
		Tags:         task.Ptr([]string{"backend", workflow.TamperBlessedTag}),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.BlessTampering(flagged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Tags, []string{"backend", workflow.TamperBlessedTag}) {
		t.Fatalf("Tags = %v, want no duplicate blessed tag", got.Tags)
	}
}

func TestTaskService_BlessTamperingRejectsNonTamperTask(t *testing.T) {
	t.Parallel()
	svc, _ := setupTaskService(t)
	created, err := svc.tasks.Create("Not tamper", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	human, err := svc.tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("needs human input"),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.BlessTampering(human.ID)
	if err == nil {
		t.Fatal("BlessTampering non-tamper err = nil, want error")
	}
	if !strings.Contains(err.Error(), "status=human-required") || !strings.Contains(err.Error(), workflow.TamperFlaggedReasonPrefix) {
		t.Fatalf("BlessTampering non-tamper err = %q, want actionable preconditions", err.Error())
	}
	got, err := svc.tasks.Get(human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired || slices.Contains(got.Tags, workflow.TamperBlessedTag) {
		t.Fatalf("task after rejected bless = status %q tags %v", got.Status, got.Tags)
	}
}

func readTaskServiceAuditEvents(t *testing.T, dir string) []audit.Event {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var events []audit.Event
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev audit.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				t.Fatal(err)
			}
			events = append(events, ev)
		}
	}
	return events
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
	// TaskType must be set durably — the enrich write above clears
	// enrich-pending, and that write re-fires the emit-path task.created
	// dispatch (fsnotify watcher). TaskType is the only guard left standing,
	// so skipTaskCreatedWorkflow must still hold on the persisted task.
	if got.TaskType != task.TaskTypeUmbrella {
		t.Fatalf("TaskType = %q, want %q so a re-fired task:created dispatch is skipped", got.TaskType, task.TaskTypeUmbrella)
	}
	if !skipTaskCreatedWorkflow(got) {
		t.Fatal("simulated watcher re-dispatch on the inert stub must be skipped, want no flat workflow")
	}
}

// TestTaskService_CreateTask_UmbrellaExpandFailureWithExistingTrackerMarksDuplicate
// guards #1570's follow-on fix: when a planner failure already produced a
// durable failure tracker (internal/umbrella.recordExpandFailure creates or
// updates one inside Expand), the manually-created stub must not also claim
// TaskTypeUmbrella — that would leave two tracker tasks for the same issue,
// confusing scanExisting/the gate about which one is authoritative.
func TestTaskService_CreateTask_UmbrellaExpandFailureWithExistingTrackerMarksDuplicate(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.cfg = &config.Config{Umbrella: config.UmbrellaConfig{Enabled: true}}
	const issueURL = "https://github.com/owner/repo/issues/1151"
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     1151,
			Title:      "☂️ already has a failure tracker",
			URL:        issueURL,
			Repository: "owner/repo",
			Labels:     []string{"umbrella", "backend"},
		}, nil
	}
	// Simulate a prior Expand call's recordExpandFailure having already
	// materialized the umbrella's durable failure tracker.
	if _, err := svc.tasks.CreateFull("umbrella tracker", "", task.AgentModeHeadless, task.Update{
		Issue:        task.Ptr(issueURL),
		TaskType:     task.Ptr(task.TaskTypeUmbrella),
		Status:       task.Ptr(task.StatusInProgress),
		StatusReason: task.Ptr("umbrella expansion failed (attempt 1): planner boom"),
		Tags:         task.Ptr([]string{"umbrella", umbrella.ExpandFailTag(1)}),
	}); err != nil {
		t.Fatalf("seed failure tracker: %v", err)
	}
	svc.umbrellaExpand = func(string) (umbrella.Result, error) {
		return umbrella.Result{}, errors.New("planner boom")
	}

	created, err := svc.CreateTask(issueURL, "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatalf("duplicate stub should survive a failed expansion: %v", err)
	}
	if got.TaskType == task.TaskTypeUmbrella {
		t.Fatalf("TaskType = %q, want non-umbrella duplicate since a failure tracker already exists for this issue", got.TaskType)
	}
	if !slices.Contains(got.Tags, umbrellaDuplicateTag) {
		t.Fatalf("Tags = %v, want to contain %q", got.Tags, umbrellaDuplicateTag)
	}
	if !skipTaskCreatedWorkflow(got) {
		t.Fatal("simulated watcher re-dispatch on the duplicate stub must be skipped, want no flat workflow")
	}
}

func TestTaskService_CreateTask_UmbrellaExpandDeleteFailureKeepsDuplicateNonTracker(t *testing.T) {
	svc, _ := setupTaskService(t)
	svc.cfg = &config.Config{Umbrella: config.UmbrellaConfig{Enabled: true}}
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     1151,
			Title:      "☂️ duplicate cleanup failed",
			URL:        "https://github.com/owner/repo/issues/1151",
			Repository: "owner/repo",
			Labels:     []string{"umbrella", "backend"},
		}, nil
	}
	svc.umbrellaExpand = func(string) (umbrella.Result, error) {
		return umbrella.Result{UmbrellaURL: "https://github.com/owner/repo/issues/1151", Created: 6}, nil
	}
	svc.deleteTask = func(string) error {
		return errors.New("delete boom")
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/1151", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatalf("duplicate stub should remain when cleanup delete fails: %v", err)
	}
	if got.Title != "☂️ duplicate cleanup failed" {
		t.Fatalf("Title = %q, want enriched duplicate title", got.Title)
	}
	if !slices.Equal(got.Tags, []string{"umbrella", "backend", umbrellaDuplicateTag}) {
		t.Fatalf("Tags = %v, want issue labels plus the durable duplicate-dispatch guard", got.Tags)
	}
	if got.StatusReason == "" {
		t.Fatal("StatusReason is empty, want an explanation for the duplicate stub")
	}
	if got.TaskType == task.TaskTypeUmbrella {
		t.Fatalf("TaskType = %q, want non-umbrella duplicate so gate/scanExisting cannot treat it as the live tracker", got.TaskType)
	}
	// TaskType alone no longer guards dispatch for this duplicate (by design,
	// to avoid the tracker-identity collision above), so the belt-and-braces
	// umbrellaDuplicateTag check in skipTaskCreatedWorkflow must hold instead —
	// a re-fired task:created dispatch (fsnotify watcher) must still be skipped.
	if got.Workflow != nil {
		t.Fatalf("workflow = %+v, want nil for a duplicate umbrella stub", got.Workflow)
	}
	if !skipTaskCreatedWorkflow(got) {
		t.Fatal("simulated watcher re-dispatch on the duplicate stub must be skipped, want no flat workflow")
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

func TestTaskService_ReconcilePendingEnrichment_RetriesOrphanedStub(t *testing.T) {
	svc, _ := setupTaskService(t)

	var succeed atomic.Bool
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		if !succeed.Load() {
			return github.Issue{}, errors.New("gh issue view: API rate limit exceeded")
		}
		return github.Issue{
			Number:     42,
			Title:      "recovered issue",
			URL:        "https://github.com/owner/repo/issues/42",
			Repository: "owner/repo",
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) { return nil, nil }
	svc.viewerLogin = func() string { return "me" }

	// The initial async fetch fails, orphaning the stub with the marker intact.
	created, err := svc.CreateTask("https://github.com/owner/repo/issues/42", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, want enrich-pending marker after a failed initial fetch", got.Tags)
	}
	if got.Title != "https://github.com/owner/repo/issues/42" {
		t.Fatalf("Title = %q, want the raw URL to survive a failed fetch", got.Title)
	}
	if got.Workflow != nil {
		t.Fatalf("Workflow = %+v, want none while the stub is still enrich-pending", got.Workflow)
	}

	// gh recovers; the maintenance reconcile pass re-enriches and dispatches.
	succeed.Store(true)
	svc.ReconcilePendingEnrichment()
	svc.wg.Wait()

	got = waitForWorkflow(t, svc, created.ID)
	if slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, marker should clear after reconcile", got.Tags)
	}
	if got.Title != "recovered issue" {
		t.Fatalf("Title = %q, want enriched title after reconcile", got.Title)
	}
}

// TestTaskService_EnrichFromIssue_LinkedPRsFailureKeepsPendingMarker is a
// regression test for the case where the issue fetch itself succeeds (title,
// body, and Issue URL are persisted) but the secondary, warn-only linked-PRs
// fetch fails. Previously the enrich-pending marker was cleared regardless,
// leaving the task in todo with no marker, no workflow dispatch, and no way
// for ReconcilePendingEnrichment to find and retry it — inert forever.
func TestTaskService_EnrichFromIssue_LinkedPRsFailureKeepsPendingMarker(t *testing.T) {
	svc, _ := setupTaskService(t)

	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     42,
			Title:      "flaky linked-prs issue",
			URL:        "https://github.com/owner/repo/issues/42",
			Repository: "owner/repo",
			Labels:     []string{"bug"},
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		return nil, errors.New("gh api: linked PRs lookup failed")
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/42", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, want enrich-pending marker retained after a linked-PRs fetch failure", got.Tags)
	}
	if got.Title != "flaky linked-prs issue" {
		t.Fatalf("Title = %q, want the real issue title even though the marker was kept", got.Title)
	}
	if got.Issue != "https://github.com/owner/repo/issues/42" {
		t.Fatalf("Issue = %q, want the issue URL persisted for reconcile fallback", got.Issue)
	}
	if got.Workflow != nil {
		t.Fatalf("Workflow = %+v, want none while the stub is still enrich-pending", got.Workflow)
	}
}

// TestTaskService_ReconcilePendingEnrichment_RetriesAfterLinkedPRsFailure
// covers the recovery half of the above: once the title has already been
// rewritten to the real issue title (so it no longer parses as a GitHub
// URL), the reconcile pass must fall back to the persisted Issue URL to
// re-derive the repo/number and retry.
func TestTaskService_ReconcilePendingEnrichment_RetriesAfterLinkedPRsFailure(t *testing.T) {
	svc, _ := setupTaskService(t)

	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     42,
			Title:      "flaky linked-prs issue",
			URL:        "https://github.com/owner/repo/issues/42",
			Repository: "owner/repo",
		}, nil
	}
	var succeed atomic.Bool
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		if !succeed.Load() {
			return nil, errors.New("gh api: linked PRs lookup failed")
		}
		return nil, nil
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/42", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, want enrich-pending marker before reconcile", got.Tags)
	}

	succeed.Store(true)
	svc.ReconcilePendingEnrichment()
	svc.wg.Wait()

	got = waitForWorkflow(t, svc, created.ID)
	if slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, marker should clear once the linked-PRs fetch succeeds", got.Tags)
	}
}

func TestTaskService_ReconcilePendingEnrichment_CoolsDownLinkedPRsFailure(t *testing.T) {
	svc, _ := setupTaskService(t)

	svc.fetchIssue = func(string, int) (github.Issue, error) {
		return github.Issue{
			Number:     42,
			Title:      "permanently flaky linked-prs issue",
			URL:        "https://github.com/owner/repo/issues/42",
			Repository: "owner/repo",
		}, nil
	}
	var linkedPRCalls atomic.Int32
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		linkedPRCalls.Add(1)
		return nil, errors.New("gh api: linked PRs lookup failed")
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/42", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	if got := linkedPRCalls.Load(); got != 1 {
		t.Fatalf("linked PR calls after initial enrichment = %d, want 1", got)
	}

	svc.ReconcilePendingEnrichment()
	svc.wg.Wait()
	if got := linkedPRCalls.Load(); got != 2 {
		t.Fatalf("linked PR calls after first reconcile = %d, want 2", got)
	}

	svc.ReconcilePendingEnrichment()
	svc.wg.Wait()
	if got := linkedPRCalls.Load(); got != 2 {
		t.Fatalf("linked PR calls after cooldown-gated reconcile = %d, want still 2", got)
	}

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Tags, enrichPendingTag) {
		t.Fatalf("Tags = %v, marker should remain while linked-PRs fetch keeps failing", got.Tags)
	}
}
func TestTaskService_ReconcilePendingEnrichment_SkipsTerminalStatus(t *testing.T) {
	svc, _ := setupTaskService(t)

	var fetched atomic.Bool
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		fetched.Store(true)
		return github.Issue{}, errors.New("should not be called")
	}

	// A stub the user cancelled must not be revived for another gh fetch.
	if _, err := svc.tasks.CreateFull("https://github.com/owner/repo/issues/42", "", "headless",
		task.Update{
			Tags:   task.Ptr([]string{enrichPendingTag}),
			Status: task.Ptr(task.StatusCancelled),
		}); err != nil {
		t.Fatal(err)
	}

	svc.ReconcilePendingEnrichment()
	svc.wg.Wait()

	if fetched.Load() {
		t.Fatal("reconcile fetched GitHub for a stub with a terminal (cancelled) status")
	}
}

func TestTaskService_ReconcilePendingEnrichment_SkipsInFlightInitialEnrichment(t *testing.T) {
	svc, _ := setupTaskService(t)

	var calls atomic.Int32
	block := make(chan struct{})
	release := make(chan struct{})
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		calls.Add(1)
		close(block)
		<-release
		return github.Issue{
			Number:     42,
			Title:      "recovered issue",
			URL:        "https://github.com/owner/repo/issues/42",
			Repository: "owner/repo",
		}, nil
	}
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) { return nil, nil }
	svc.viewerLogin = func() string { return "me" }

	// The original CreateTask enrichment goroutine is still in flight (blocked
	// on the gh fetch) when a maintenance tick fires; reconcile must not start
	// a second concurrent fetch for the same stub.
	created, err := svc.CreateTask("https://github.com/owner/repo/issues/42", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	<-block

	svc.ReconcilePendingEnrichment()

	close(release)
	svc.wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fetchIssue called %d times, want exactly 1 (reconcile must dedupe against the in-flight initial fetch)", got)
	}

	got, err := svc.GetTask(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "recovered issue" {
		t.Fatalf("Title = %q, want the enriched title once the in-flight fetch completes", got.Title)
	}
}

func TestTaskService_ReconcilePendingEnrichment_SkipsNonStubs(t *testing.T) {
	svc, _ := setupTaskService(t)

	var fetched atomic.Bool
	svc.fetchIssue = func(string, int) (github.Issue, error) {
		fetched.Store(true)
		return github.Issue{}, errors.New("should not be called")
	}

	// An ordinary task without the marker must never be touched by reconcile.
	if _, err := svc.tasks.Create("ordinary task", "", "headless"); err != nil {
		t.Fatal(err)
	}
	// A stub whose title is no longer a URL cannot be re-fetched; reconcile
	// must skip it rather than crash or spuriously fetch.
	if _, err := svc.tasks.CreateFull("hand-edited title", "", "headless",
		task.Update{Tags: task.Ptr([]string{enrichPendingTag})}); err != nil {
		t.Fatal(err)
	}

	svc.ReconcilePendingEnrichment()
	svc.wg.Wait()

	if fetched.Load() {
		t.Fatal("reconcile fetched GitHub for a non-URL / unmarked task")
	}
}
