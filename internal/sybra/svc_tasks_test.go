package sybra

import (
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/github"
	"github.com/Automaat/sybra/internal/task"
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
