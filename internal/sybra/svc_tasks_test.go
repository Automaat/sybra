package sybra

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
	"github.com/Automaat/sybra/internal/umbrella"
)

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
	// A linked-PR fetch must NOT happen on the umbrella path — fail loudly if it does.
	svc.fetchIssueLinkedPRs = func(string, int) ([]github.PullRequest, error) {
		t.Fatal("linked-PR fetch must not run for an umbrella issue")
		return nil, nil
	}

	var expandedURL string
	svc.umbrellaExpand = func(issueURL string) (umbrella.Result, error) {
		expandedURL = issueURL
		return umbrella.Result{UmbrellaURL: issueURL, Created: 6}, nil
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/1151", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	if expandedURL != "https://github.com/owner/repo/issues/1151" {
		t.Fatalf("expander called with %q, want the umbrella URL", expandedURL)
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
	svc.umbrellaExpand = func(string) (umbrella.Result, error) {
		t.Fatal("expander must not run when umbrella.enabled is false")
		return umbrella.Result{}, nil
	}

	created, err := svc.CreateTask("https://github.com/owner/repo/issues/1151", "", "headless")
	if err != nil {
		t.Fatal(err)
	}
	svc.wg.Wait()

	// Disabled → flat enrichment proceeds and a workflow is started.
	got := waitForWorkflow(t, svc, created.ID)
	if got.Title != "☂️ umbrella but feature off" {
		t.Fatalf("Title = %q, want enriched title", got.Title)
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
