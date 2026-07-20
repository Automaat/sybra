package review

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/provider"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/workflow"
)

type countingFailingAgentLauncher struct {
	err   error
	calls int
}

func (f *countingFailingAgentLauncher) StartAgent(taskID, role, mode, model, providerName, prompt, dir string, allowedTools []string, needsWorktree, oneShot bool, outputSchema, cleanRetryRef string, assignment workflow.AgentAssignment) (agentID, startedDir, baselineRef string, err error) {
	f.calls++
	return "", "", "", f.err
}
func (f *countingFailingAgentLauncher) HasRunningAgent(string) bool                     { return false }
func (f *countingFailingAgentLauncher) HasOtherRunningAgentForTask(string, string) bool { return false }
func (f *countingFailingAgentLauncher) FindRunningAgentForRole(string, string) (string, bool) {
	return "", false
}
func (f *countingFailingAgentLauncher) StopAgentsForTask(string, string) {}
func (f *countingFailingAgentLauncher) SendPrompt(string, string) error  { return nil }
func (f *countingFailingAgentLauncher) DefaultProvider() string          { return "claude" }
func (f *countingFailingAgentLauncher) ProviderRateLimited(string) bool  { return false }
func (f *countingFailingAgentLauncher) ProviderCanFailover(string) bool  { return false }
func (f *countingFailingAgentLauncher) ProviderHealthy(string) bool      { return false }
func (f *countingFailingAgentLauncher) IsDispatching(string) bool        { return false }
func (f *countingFailingAgentLauncher) AdmitDispatch(string, string, string) (admit bool, reason string) {
	return true, ""
}
func (f *countingFailingAgentLauncher) TryClaimDispatch(string) (workflow.DispatchClaim, bool) {
	return nil, true
}

func TestDispatchPRIssueWithOptions_RecoversRetryableHumanRequiredPRFix(t *testing.T) {
	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(taskStore, nil)

	wfStore, err := workflow.NewStore(filepath.Join(t.TempDir(), "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := wfStore.Save(workflow.Definition{
		ID:   prFixWorkflowID,
		Name: "PR Fix",
		Trigger: workflow.Trigger{On: "pr.event", Conditions: []workflow.Condition{{
			Field: "pr.issue_kind", Operator: "equals", Value: string(github.PRIssueCIFailure),
		}}},
		Steps: []workflow.Step{
			{
				ID:   "fix",
				Name: "Fix PR Issue",
				Type: workflow.StepRunAgent,
				Config: workflow.StepConfig{
					Role:   "pr-fix",
					Mode:   "headless",
					Model:  "sonnet",
					Prompt: "fix",
				},
				Next: []workflow.Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	launcher := &countingFailingAgentLauncher{
		err: &provider.UnhealthyError{Provider: "claude", Reason: provider.RateLimitReason, RateLimited: true},
	}
	engine := workflow.NewEngine(wfStore, &taskAdapter{tasks: tasks}, launcher, slog.New(slog.DiscardHandler))
	handler := &Handler{
		logger:         slog.New(slog.DiscardHandler),
		emit:           func(string, any) {},
		tasks:          tasks,
		prTracker:      github.NewIssueTracker(0),
		WorkflowEngine: engine,
	}

	created, err := tasks.Create("retry pr fix", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	created, err = tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("previous blocker"),
		ProjectID:    task.Ptr("owner/repo"),
		PRNumber:     task.Ptr(42),
		Branch:       task.Ptr("feat/retry"),
	})
	if err != nil {
		t.Fatal(err)
	}

	ok := handler.dispatchPRIssueWithOptions(context.Background(), created, github.PRIssue{
		TaskID: created.ID,
		Kind:   github.PRIssueCIFailure,
		PR: github.PullRequest{
			Number:     42,
			Repository: "owner/repo",
			HeadSHA:    "deadbeef",
		},
	}, []github.PRIssue{{
		TaskID: created.ID,
		Kind:   github.PRIssueCIFailure,
		PR: github.PullRequest{
			Number:     42,
			Repository: "owner/repo",
			HeadSHA:    "deadbeef",
		},
	}}, "fix prompt", t.TempDir(), dispatchFixOptions{})
	if !ok {
		t.Fatal("dispatchPRIssueWithOptions() = false, want retryable success")
	}

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusInReview {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusInReview)
	}
	if !strings.Contains(got.StatusReason, "provider claude unhealthy") {
		t.Fatalf("status_reason = %q, want provider unhealthy reason", got.StatusReason)
	}
	if got.Workflow == nil || got.Workflow.WorkflowID != prFixWorkflowID || got.Workflow.CurrentStep != "fix" {
		t.Fatalf("workflow = %+v, want live pr-fix at fix step", got.Workflow)
	}
	if launcher.calls != 1 {
		t.Fatalf("launch calls = %d, want 1", launcher.calls)
	}

	engine.ResumeStalled()

	if launcher.calls != 2 {
		t.Fatalf("launch calls after ResumeStalled = %d, want 2", launcher.calls)
	}
}

func TestDispatchPRIssueWithOptions_DoesNotRewritePermanentFailure(t *testing.T) {
	taskStore, err := task.NewStore(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := task.NewManager(taskStore, nil)

	wfStore, err := workflow.NewStore(filepath.Join(t.TempDir(), "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	if err := wfStore.Save(workflow.Definition{
		ID:   prFixWorkflowID,
		Name: "PR Fix",
		Trigger: workflow.Trigger{On: "pr.event", Conditions: []workflow.Condition{{
			Field: "pr.issue_kind", Operator: "equals", Value: string(github.PRIssueCIFailure),
		}}},
		Steps: []workflow.Step{
			{
				ID:   "fix",
				Name: "Fix PR Issue",
				Type: workflow.StepRunAgent,
				Config: workflow.StepConfig{
					Role:   "pr-fix",
					Mode:   "headless",
					Model:  "sonnet",
					Prompt: "fix",
				},
				Next: []workflow.Transition{{GoTo: ""}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	launcher := &countingFailingAgentLauncher{err: workflow.ErrNoProjectAssigned}
	engine := workflow.NewEngine(wfStore, &taskAdapter{tasks: tasks}, launcher, slog.New(slog.DiscardHandler))
	handler := &Handler{
		logger:         slog.New(slog.DiscardHandler),
		emit:           func(string, any) {},
		tasks:          tasks,
		prTracker:      github.NewIssueTracker(0),
		WorkflowEngine: engine,
	}

	created, err := tasks.Create("permanent failure", "", task.AgentModeHeadless)
	if err != nil {
		t.Fatal(err)
	}
	created, err = tasks.Update(created.ID, task.Update{
		Status:       task.Ptr(task.StatusHumanRequired),
		StatusReason: task.Ptr("existing blocker"),
		PRNumber:     task.Ptr(42),
		Branch:       task.Ptr("feat/permanent"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if handler.dispatchPRIssueWithOptions(context.Background(), created, github.PRIssue{
		TaskID: created.ID,
		Kind:   github.PRIssueCIFailure,
		PR:     github.PullRequest{Number: 42, Repository: "owner/repo", HeadSHA: "deadbeef"},
	}, []github.PRIssue{{
		TaskID: created.ID,
		Kind:   github.PRIssueCIFailure,
		PR:     github.PullRequest{Number: 42, Repository: "owner/repo", HeadSHA: "deadbeef"},
	}}, "fix prompt", t.TempDir(), dispatchFixOptions{}) {
		t.Fatal("dispatchPRIssueWithOptions() = true, want false for permanent failure")
	}

	got, err := tasks.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != task.StatusHumanRequired {
		t.Fatalf("status = %q, want %q", got.Status, task.StatusHumanRequired)
	}
}
